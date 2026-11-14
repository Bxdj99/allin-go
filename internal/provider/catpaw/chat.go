// 本文件把「标准 OpenAI chat 请求」接到妙手的三步会话机上（round → event → turn）。
//
// 与同目录 catpaw.go 里的 Chat / ParseTurn 的关系：
//
//	Chat      一次性读完整个 turn 响应体再解析，适合后台批量调用；
//	Run       逐帧读取 turn 的 SSE，边说边转换，是网关流式转发用的入口。
//
// 两者共用同一套帧解析（decodeTurnFrame），避免出现「两处解析、口径不一致」。
package catpaw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"allin/internal/provider/oai"
)

// Run 跑一次完整对话，把妙手的累积 SSE 帧翻成标准 OpenAI 响应写到 out。
//
// stream=true 时逐帧写 chat.completion.chunk（末帧补 [DONE]）；false 时聚合成一个
// chat.completion。返回的错误可以用 errors.Is / errors.As 分辨：
//
//	ErrSignInRequired        → 登录态过期（换账号或让用户重新登录）
//	ErrUnsupported           → 内容超出扁平文本协议（工具 / 图片 / 未知角色），不必重试
//	*UpstreamError           → 上游明确报错，带 HTTP 状态与业务 code
func Run(ctx context.Context, acc *Account, model string, reqBody []byte, out io.Writer, stream bool) error {
	if acc == nil || acc.Token == "" {
		return ErrSignInRequired
	}
	req, err := oai.ParseChat(reqBody)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	// 图片等非文本段送不进去；明确拒绝，不能让模型「看不见还照答」。
	if req.NonText {
		return fmt.Errorf("%w: 妙手只收纯文本，图片等非文本内容送不进去", ErrUnsupported)
	}
	msgs := make([]Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, Message{Role: m.Role, Content: m.Text})
	}
	if err := CheckSupported(msgs, req.HasTools); err != nil {
		return err
	}

	modelType := ModelTypeOf(model)
	convID := uuidLike()
	msgID := uuidLike()
	turnID := uuidLike()
	prompt := BuildPrompt(msgs)

	// 同一个 message 要原样出现在 round 和 turn 里，所以做成闭包复用。
	userMsg := func() map[string]any {
		return map[string]any{
			"type":      "user",
			"messageId": msgID,
			"content": []any{
				map[string]any{"type": "text", "text": prompt},
			},
			"finished": true,
		}
	}

	// ① round：建会话
	round := map[string]any{
		"conversationId": convID,
		"message":        userMsg(),
		"modelType":      modelType,
		"source":         Source,
		"mode":           Mode,
		"permissionMode": PermissionMode,
		"toolVersion":    ToolVersion,
		"repositories":   []any{},
	}
	if err := postCodeZero(ctx, Origin+PathRound, spanHeaders(acc, "0.2", "conversation.round"), round); err != nil {
		return err
	}

	// ② event：标记会话运行中
	event := map[string]any{
		"conversationId": convID,
		"eventType":      "conversation",
		"data":           map[string]any{"status": "running"},
	}
	if err := postCodeZero(ctx, Origin+PathEvent, spanHeaders(acc, "0.3", "conversation.event"), event); err != nil {
		return err
	}

	// ③ turn：真正的答案（SSE，累积帧）
	turn := map[string]any{
		"conversationId": convID,
		"turnRequestId":  turnID,
		"message":        userMsg(),
		"modelType":      modelType,
		"source":         Source,
		"mode":           Mode,
		"permissionMode": PermissionMode,
		"toolConfigs":    []any{},
		"toolVersion":    ToolVersion,
	}
	raw, err := json.Marshal(turn)
	if err != nil {
		return fmt.Errorf("catpaw: turn 请求体序列化失败: %w", err)
	}
	h := spanHeaders(acc, "0.4.1", "conversation.turn")
	h["Accept"] = "text/event-stream"
	h["enableheartbeat"] = "true"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, Origin+PathTurn, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for k, v := range h {
		httpReq.Header.Set(k, v)
	}
	resp, err := HTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &UpstreamError{Op: "turn", Status: resp.StatusCode, Message: shortErr(string(body))}
	}

	return relayTurn(resp.Body, model, out, stream)
}

// relayTurn 逐帧读 turn 的 SSE 并写回 OpenAI 响应。
//
// 上游帧是累积式的（每帧给到目前的全文），所以用 oai.Accumulator 取增量：
// 它同时兼容「累积快照」和「纯增量」两种形态，上游哪天改成增量也不用动这里。
func relayTurn(src io.Reader, model string, out io.Writer, stream bool) error {
	var (
		accText = &oai.Accumulator{}
		accReas = &oai.Accumulator{}
		sw      *oai.Stream
		seen    bool
	)
	if stream {
		sw = oai.NewStream(out, model)
	}

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20) // 单帧上限放到 8 MiB：长答案的累积帧会越来越大
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // 空行分隔符、event:、心跳注释等一律跳过
		}
		data := strings.TrimSpace(line[5:])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		text, reasoning, err := decodeTurnFrame(data)
		if err != nil {
			// 流已经开始写给客户端时只能尽力告知；没开始时外层还能换号。
			if sw != nil {
				_ = sw.Fail(err.Error())
			}
			return err
		}
		if text == "" && reasoning == "" {
			continue
		}
		seen = true
		if sw != nil {
			if err := sw.Delta(accText.Feed(text), accReas.Feed(reasoning)); err != nil {
				return err
			}
		} else {
			accText.Feed(text)
			accReas.Feed(reasoning)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("catpaw: turn 读取中断: %w", err)
	}
	if !seen {
		return errors.New("catpaw: turn 没有任何内容帧")
	}
	if sw != nil {
		return sw.Finish("stop")
	}
	_, err := out.Write(oai.Completion(model, accText.Full(), accReas.Full()))
	return err
}

// decodeTurnFrame 解一帧 turn 的 data，返回该帧给出的（累积）正文与推理文本。
//
// 非内容帧（心跳、状态通知）返回空串而不是错误：上游会把它们混在同一个流里。
func decodeTurnFrame(data string) (text, reasoning string, err error) {
	var frame struct {
		Error   json.RawMessage `json:"error"`
		Message *struct {
			Content []struct {
				Type             string  `json:"type"`
				Text             *string `json:"text"`
				ReasoningContent *string `json:"reasoningContent"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return "", "", nil // 解析不了就当噪音跳过，和 ParseTurn 的容错口径一致
	}
	if len(frame.Error) > 0 && string(frame.Error) != "null" {
		var e struct {
			Code int    `json:"code"`
			Msg  string `json:"message"`
		}
		_ = json.Unmarshal(frame.Error, &e)
		return "", "", fmt.Errorf("catpaw: 上游返回错误 code=%d %s", e.Code, e.Msg)
	}
	if frame.Message == nil {
		return "", "", nil
	}
	for _, c := range frame.Message.Content {
		if c.Type != "text" {
			continue
		}
		if c.Text != nil {
			text = *c.Text
		}
		if c.ReasoningContent != nil {
			reasoning = *c.ReasoningContent
		}
	}
	return text, reasoning, nil
}

// StatusCode 让网关能按统一的接口取上游 HTTP 状态（换号重试 / 回给客户端都用它）。
func (e *UpstreamError) StatusCode() int { return e.Status }
