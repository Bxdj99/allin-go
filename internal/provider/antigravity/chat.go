// 本文件把「标准 OpenAI chat 请求」接到 Antigravity 的 Code Assist 聊天端点上：
//
//	POST https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse
//	body：{model, project, userAgent, requestType, requestId, request{contents, systemInstruction,
//	       generationConfig, sessionId}}
//
// 三个约束都在这里落实：
//   - 传输层必须是 HTTP/1.1（cloudcode-pa 一旦协商到 h2 就 reset）→ 用本包的 HTTPClient，
//     它的 Transport 关掉了 ForceAttemptHTTP2；网关启动时还会把它换成带内置根证书的那一份。
//   - project 不能空：先看账号里存的，没有就 loadCodeAssist / onboard 补一次（EnsureProject）。
//   - system prompt 命中「会被回假 429」的片段时，BuildEnvelope 内部已经用 CloakSystem 打散。
package antigravity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"allin/internal/provider/oai"
)

// Run 跑一次对话，把 streamGenerateContent 的 SSE 翻成标准 OpenAI 响应写到 out。
//
// stream=true 写 chat.completion.chunk（末帧补 [DONE]），false 聚合成 chat.completion。
// 返回的错误可以用 errors.As(*UpstreamError) / errors.Is(ErrSignInRequired) 分辨。
func Run(ctx context.Context, acc *Account, model string, reqBody []byte, out io.Writer, stream bool) error {
	if acc == nil {
		return ErrSignInRequired
	}
	req, err := oai.ParseChat(reqBody)
	if err != nil {
		return err
	}
	// 本包的 BuildEnvelope 只做纯文本换算：工具定义、图片、thoughtSignature 的换算
	// 体量很大且未移植，与其悄悄丢掉不如明确拒绝。
	if req.HasTools {
		return errors.New("antigravity: 当前接线不支持工具调用（信封里的 toolConfig 还没移植）")
	}
	if req.NonText {
		return errors.New("antigravity: 当前接线只支持纯文本消息")
	}

	access, err := EnsureToken(ctx, acc)
	if err != nil {
		return err
	}
	project, err := EnsureProject(ctx, acc, access)
	if err != nil {
		return err
	}

	payload := &ChatPayload{Temperature: req.Temperature, MaxTokens: req.MaxTokens}
	for _, m := range req.Messages {
		payload.Messages = append(payload.Messages, ChatMessage{Role: m.Role, Content: m.Text})
	}
	env, err := BuildEnvelope(payload, model, project)
	if err != nil {
		return err
	}

	httpReq, err := StreamRequest(ctx, acc, env)
	if err != nil {
		return err
	}
	resp, err := HTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &UpstreamError{Op: "streamGenerateContent", Status: resp.StatusCode, Message: shortErr(string(body))}
	}

	return relayStream(resp.Body, model, out, stream)
}

// geminiPart 是候选内容里的一段。
type geminiPart struct {
	Text    string `json:"text"`
	Thought bool   `json:"thought,omitempty"`
}

// geminiCandidate 是一个候选回答。
type geminiCandidate struct {
	Content struct {
		Role  string       `json:"role"`
		Parts []geminiPart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason,omitempty"`
	Index        int    `json:"index"`
}

// geminiError 是上游的错误对象。
type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// geminiFrame 是 SSE 里的一帧。
//
// cloudcode-pa 有的版本把真正的 Gemini 响应再包一层 {"response": {...}}，
// 所以这里让 frame 引用自己：解出来先看 Response 是否非空，非空就用里层。
type geminiFrame struct {
	Response     *geminiFrame      `json:"response,omitempty"`
	Candidates   []geminiCandidate `json:"candidates,omitempty"`
	ModelVersion string            `json:"modelVersion,omitempty"`
	Error        *geminiError      `json:"error,omitempty"`
}

// relayStream 逐帧读 SSE 并写回 OpenAI 响应。
func relayStream(src io.Reader, model string, out io.Writer, stream bool) error {
	var (
		accText = &oai.Accumulator{}
		accReas = &oai.Accumulator{}
		sw      *oai.Stream
		seen    bool
		finish  string
	)
	if stream {
		sw = oai.NewStream(out, model)
	}

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // 空行、event: 、注释心跳一律跳过
		}
		data := strings.TrimSpace(line[5:])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var frame geminiFrame
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			continue // 非内容帧当噪音跳过
		}
		if frame.Error != nil {
			err := &UpstreamError{
				Op:      "streamGenerateContent",
				Status:  frame.Error.Code,
				Message: shortErr(frame.Error.Status + " " + frame.Error.Message),
			}
			if sw != nil {
				_ = sw.Fail(err.Error())
			}
			return err
		}
		inner := &frame
		if frame.Response != nil {
			inner = frame.Response
			if inner.Error != nil {
				err := &UpstreamError{Op: "streamGenerateContent", Status: inner.Error.Code, Message: shortErr(inner.Error.Message)}
				if sw != nil {
					_ = sw.Fail(err.Error())
				}
				return err
			}
		}
		for _, c := range inner.Candidates {
			for _, p := range c.Content.Parts {
				if p.Text == "" {
					continue
				}
				seen = true
				if p.Thought {
					// Gemini 的思考段落在 thought:true 的 part 里，对应 OpenAI 的 reasoning_content。
					if sw != nil {
						if err := sw.Delta("", accReas.Feed(p.Text)); err != nil {
							return err
						}
					} else {
						accReas.Feed(p.Text)
					}
					continue
				}
				if sw != nil {
					if err := sw.Delta(accText.Feed(p.Text), ""); err != nil {
						return err
					}
				} else {
					accText.Feed(p.Text)
				}
			}
			if c.FinishReason != "" {
				finish = mapFinishReason(c.FinishReason)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("antigravity: 上游读取中断: %w", err)
	}
	if !seen {
		return errors.New("antigravity: 上游没有任何内容帧")
	}
	if sw != nil {
		return sw.Finish(finish)
	}
	_, err := out.Write(oai.Completion(model, accText.Full(), accReas.Full()))
	return err
}

// mapFinishReason 把 Gemini 的完成原因映射成 OpenAI 的取值。
func mapFinishReason(reason string) string {
	switch strings.ToUpper(reason) {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
		return "content_filter"
	default:
		return "stop"
	}
}

// StatusCode 让网关能按统一接口取上游 HTTP 状态（换号重试 / 回给客户端都用它）。
func (e *UpstreamError) StatusCode() int { return e.Status }
