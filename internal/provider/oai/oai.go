// Package oai 是「网页版聊天」几家共用的两件小工具。
//
// 为什么单独抽一层：妙手 CatPaw、百度文心、Antigravity 三家的私有协议各写各的，
// 但两头的「边界」是完全一样的 ——
//
//	入口：客户端发来的标准 OpenAI chat 请求，需要压成纯文本消息序列
//	      （这三家的网页协议都只吃纯文本，工具调用和图片都送不进去）。
//	出口：上游吐出来的增量文本，需要按 OpenAI 的形状写回客户端
//	      （流式是一串 chat.completion.chunk + [DONE]，非流式是一个 chat.completion）。
//
// 只依赖标准库，和内部其它 provider 包一样零外部依赖。
package oai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Message 是一条纯文本消息（role 保留 OpenAI 的写法：system / developer / user / assistant）。
type Message struct {
	Role string
	Text string
}

// ChatRequest 是标准 OpenAI chat 请求里这几家用得上的部分。
//
// HasTools / NonText 不是拿来用的，是拿来「拒绝」的：上游协议承载不了，
// 悄悄丢掉会让模型看不到用户真正发的东西，所以要在发请求前明确报错。
type ChatRequest struct {
	Model       string
	Messages    []Message
	Temperature *float64
	MaxTokens   int
	HasTools    bool
	NonText     bool
}

// rawMessage 是请求体里的一条消息（content 可能是字符串，也可能是分段数组）。
type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// rawContentPart 是分段 content 里的一段。
type rawContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseChat 解析标准 OpenAI chat 请求体。
func ParseChat(body []byte) (*ChatRequest, error) {
	var in struct {
		Model               string          `json:"model"`
		Messages            []rawMessage    `json:"messages"`
		Tools               json.RawMessage `json:"tools"`
		Temperature         *float64        `json:"temperature"`
		MaxTokens           *float64        `json:"max_tokens"`
		MaxCompletionTokens *float64        `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if len(in.Messages) == 0 {
		return nil, errors.New("messages 不能为空")
	}
	req := &ChatRequest{Model: in.Model, Temperature: in.Temperature}
	if in.MaxTokens != nil {
		req.MaxTokens = int(*in.MaxTokens)
	}
	// 新客户端把上限写在 max_completion_tokens 里，两者取其一。
	if req.MaxTokens <= 0 && in.MaxCompletionTokens != nil {
		req.MaxTokens = int(*in.MaxCompletionTokens)
	}
	if t := strings.TrimSpace(string(in.Tools)); t != "" && t != "null" && t != "[]" {
		req.HasTools = true
	}
	for _, m := range in.Messages {
		text, nonText, err := contentText(m.Content)
		if err != nil {
			return nil, err
		}
		if nonText {
			req.NonText = true
		}
		req.Messages = append(req.Messages, Message{Role: strings.TrimSpace(m.Role), Text: text})
	}
	return req, nil
}

// contentText 取一条消息的文本：content 可能是字符串，也可能是 [{"type":"text","text":"…"}]。
// 出现非文本段时把 nonText 置真，由调用方决定拒绝还是继续。
func contentText(raw json.RawMessage) (text string, nonText bool, err error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", false, nil
	}
	if strings.HasPrefix(s, "\"") {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", false, fmt.Errorf("消息内容解析失败: %w", err)
		}
		return v, false, nil
	}
	var parts []rawContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", false, fmt.Errorf("消息内容解析失败: %w", err)
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		default:
			nonText = true
		}
	}
	return b.String(), nonText, nil
}

// Accumulator 处理「上游到底发的是累积快照还是增量」这个分不清的问题。
//
// 妙手的 turn 是累积式（每帧给到目前的全文），Gemini 系的流是增量式（每帧只给新的一段），
// 上游换版本时这两种还可能对调。这里用「新串是否以已有全文开头」来判：是就取后缀，
// 不是就当成纯增量追加。两种形态都能正确还原，也就不用为每家各写一份。
type Accumulator struct {
	full string
}

// Feed 喂一帧原文，返回本次应该发给客户端的增量。
func (a *Accumulator) Feed(s string) string {
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, a.full) {
		d := s[len(a.full):]
		a.full = s
		return d
	}
	a.full += s
	return s
}

// Full 返回到目前为止的全文。
func (a *Accumulator) Full() string { return a.full }

// ===================== 出口：OpenAI 响应 =====================

// chunkChoice 是 chat.completion.chunk 里的一项。
type chunkChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// chunk 是一帧 stream chunk。
type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

// Stream 把增量文本写成 OpenAI 的 SSE 帧。
//
// 每次 Delta 都立刻写出去，是否 flush 由外层的 writer 决定（网关那边用带 Flush 的
// 包装器，这样客户端能真的看到逐字输出）。
type Stream struct {
	w       io.Writer
	id      string
	created int64
	model   string
	first   bool
}

// NewStream 构造一个流式写入器；model 只用于回填响应里的 model 字段。
func NewStream(w io.Writer, model string) *Stream {
	return &Stream{
		w:       w,
		id:      "chatcmpl-" + randomHex(12),
		created: time.Now().Unix(),
		model:   model,
		first:   true,
	}
}

// Delta 写出一帧增量。content 是正文，reasoning 是思维链（上游有就给）。
func (s *Stream) Delta(content, reasoning string) error {
	if content == "" && reasoning == "" {
		return nil
	}
	delta := map[string]any{}
	// OpenAI 的约定是第一帧带 role，客户端（Cherry Studio、LobeChat 之类）靠它建会话。
	if s.first {
		delta["role"] = "assistant"
		s.first = false
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if content != "" {
		delta["content"] = content
	}
	return s.write(chunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{Index: 0, Delta: delta}},
	})
}

// Finish 收尾：写带 finish_reason 的空 delta 帧，再补 [DONE]。
func (s *Stream) Finish(reason string) error {
	if reason == "" {
		reason = "stop"
	}
	err := s.write(chunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{Index: 0, Delta: map[string]any{}, FinishReason: &reason}},
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(s.w, "data: [DONE]\n\n")
	return err
}

// Fail 在流已经开始之后遇到上游错误时，尽量用 OpenAI 的 error 帧把原因告诉客户端
// （此时状态码已经发出去了，改不了）。
func (s *Stream) Fail(msg string) error {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "upstream_error"},
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(s.w, "data: "+string(payload)+"\n\n"); err != nil {
		return err
	}
	_, err = io.WriteString(s.w, "data: [DONE]\n\n")
	return err
}

func (s *Stream) write(c chunk) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = io.WriteString(s.w, "data: "+string(payload)+"\n\n")
	return err
}

// Completion 构造一个非流式的 chat.completion 响应体。
//
// usage 一律给 0：网页版协议不回 token 计数，这里不编数字，只保证字段形状齐全
// （不少客户端读不到 usage 会自己算或不显示，给 0 比缺字段更安全）。
func Completion(model, text, reasoning string) []byte {
	msg := map[string]any{"role": "assistant", "content": text}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	out := map[string]any{
		"id":      "chatcmpl-" + randomHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		// 走到这里说明 map 里有不可序列化的东西，属于代码错误，兜底回一个最小对象。
		return []byte(`{"object":"chat.completion","choices":[]}`)
	}
	return raw
}

// randomHex 生成 n 字节随机 hex，用作响应 id。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
