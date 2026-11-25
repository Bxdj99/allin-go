package workbuddy

import (
	"encoding/json"
	"regexp"

	"allin/internal/config"
)

// thinking 签名恢复。
//
// 背景：这个上游对 assistant 历史里的 thinking 块有签名校验。
// 签名不是它自己签发的时候，会返回 11159 或 11133（thinking_signature_invalid），
// 把整个请求拒掉。而**同一段历史，只要不带 thinking 块就能通过**。
//
// 典型触发：客户端（尤其 agent 类）带来的历史里夹着别的渠道签发的 thinking 块，
// 原样转发就会被拒。这也是「App 版能用、网关版被拦」的直接原因——
// App 里有这套恢复逻辑，网关之前没有。
//
// 处理原则（v1.37.0 对齐上游 WorkBuddySignatureRetryPolicy）：
//   - 只对 **HTTP 400** 的确定性拒绝生效。401/403 是凭据问题，
//     422/429 是协议或限流问题，改请求体都不会好，重发只是白打一次。
//   - 只在错误信息明确指向签名问题时生效
//   - 命中保护性历史就**放弃重试**（见 containsProtectedHistory）
//   - 只能删「空且无签名」的 thinking 块（见 emptyUnsignedThinkingOnly）
//   - 本来就没有可删的块时返回 nil，调用方据此不重发
//
// 上一版会无条件删掉所有 thinking（含带签名、带内容的），
// 属于「有损重试」：删掉有签名的思考再发，上游仍可能判定历史被篡改，
// 而且丢了模型真实的思考内容。现在收紧到只动真正无内容的那一种。
var signatureRefusal = regexp.MustCompile(`(?i)thinking_signature_invalid|invalid\s+` + "`?" +
	`signature` + "`?" + `\s+in\s+` + "`?" + `thinking|block is bound to a different conversation|` +
	`["']internal_code["']\s*:\s*["']?11159`)

// StopReason 说明为什么不再重试（日志用，便于排查）。
type StopReason string

const (
	StopNotSignatureRejection StopReason = "not_signature_rejection"
	StopStatusNotRetryable    StopReason = "http_status_not_retryable"
	StopAlreadyRetried        StopReason = "already_retried"
	StopEnterpriseSkipped     StopReason = "enterprise_skipped"
	StopProtectedHistory      StopReason = "protected_history"
	StopRetryWouldBeLossy     StopReason = "retry_would_be_lossy"
	StopNothingToRetry        StopReason = "nothing_safe_to_retry"
)

// SignatureRetryDecision 是一次签名重试判定的结果。
type SignatureRetryDecision struct {
	// Retry 为 true 时 Body 是可用于重发的请求体；false 时 Body 为 nil。
	Retry  bool
	Body   []byte
	Reason StopReason
}

// DecideSignatureRetry 判断是否该去掉 thinking 块后重发一次。
//
//	status         上游返回的 HTTP 状态码
//	errBody        上游响应体
//	body           本次发出的请求体
//	enterpriseAcct 企业账号：不做签名重试（上游行为一致）
func DecideSignatureRetry(status int, errBody string, body []byte, enterpriseAcct bool) SignatureRetryDecision {
	if enterpriseAcct {
		return SignatureRetryDecision{Reason: StopEnterpriseSkipped}
	}
	// 只认 400：签名校验失败是这个上游的固定拒绝形态。
	// 其它 4xx 去掉 thinking 也不会通过，重发只多烧一次额度。
	if status != 400 {
		return SignatureRetryDecision{Reason: StopStatusNotRetryable}
	}
	if errBody == "" || len(errBody) > 1<<20 {
		return SignatureRetryDecision{Reason: StopNotSignatureRejection}
	}
	if !signatureRefusal.MatchString(errBody) {
		return SignatureRetryDecision{Reason: StopNotSignatureRejection}
	}
	if containsProtectedHistory(body) {
		return SignatureRetryDecision{Reason: StopProtectedHistory}
	}
	candidate, ok := emptyUnsignedThinkingOnly(body)
	if !ok {
		return SignatureRetryDecision{Reason: StopRetryWouldBeLossy}
	}
	if candidate == nil {
		return SignatureRetryDecision{Reason: StopNothingToRetry}
	}
	return SignatureRetryDecision{Retry: true, Body: candidate}
}

// IsSignatureRefusal 保留旧接口：判断一次拒绝是否属于 thinking 签名问题。
//
// 注意它只做「错误体像不像签名拒绝」的判断，**不再**决定能不能重试。
// 需要决定是否重发时用 DecideSignatureRetry。
func IsSignatureRefusal(status int, body string) bool {
	if status != 400 {
		return false
	}
	if body == "" || len(body) > 1<<20 {
		return false
	}
	return signatureRefusal.MatchString(body)
}

// WithoutThinkingBlocks 保留旧接口，等价于 DecideSignatureRetry 的宽松版：
// 只删「空且无签名」的 thinking 块，遇到任何保护性内容返回 nil。
//
// 新代码请直接用 DecideSignatureRetry。
func WithoutThinkingBlocks(body []byte) []byte {
	out, ok := emptyUnsignedThinkingOnly(body)
	if !ok {
		return nil
	}
	return out
}

// containsProtectedHistory 检查历史里有没有「动了就会出错」的内容。
//
// 出现 signature 字段或 redacted_thinking 块，说明这段历史是上游签发的，
// 删掉其中的 thinking 会让签名与内容对不上，重发反而更容易被拒。
func containsProtectedHistory(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return false
	}
	rawMsgs, ok := root["messages"]
	if !ok {
		return false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return false
	}
	for _, m := range msgs {
		rawContent, ok := m["content"]
		if !ok {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(rawContent, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			var probe map[string]json.RawMessage
			if json.Unmarshal(b, &probe) != nil {
				continue
			}
			if _, has := probe["signature"]; has {
				return true
			}
			var t struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(b, &t)
			if t.Type == "redacted_thinking" {
				return true
			}
		}
	}
	return false
}

// emptyUnsignedThinkingOnly 删掉「空且无签名」的 thinking 块。
//
// 只有这种形状可以被安全移除：
//
//	{"type":"thinking","thinking":""}   —— 恰好两个字段，且 thinking 是空字符串
//
// 返回值：
//   - (nil, true)   没有需要删的块，调用方不该重发
//   - (nil, false)  存在不能安全删除的内容（有签名 / 有正文 / 有额外字段）
//   - (body, true)  删好了，可以重发
func emptyUnsignedThinkingOnly(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return nil, true
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil, true
	}
	rawMsgs, ok := root["messages"]
	if !ok {
		return nil, true
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return nil, true
	}

	changed := false
	// 注意必须用下标访问：msgs 是 []map，用 range 拿到的是 map 头副本，
	// 改 m["content"] 不会写回切片（踩过 —— 判定 retry=true 但内容没变）。
	for i := range msgs {
		m := msgs[i]
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role != "assistant" {
			continue
		}
		rawContent, ok := m["content"]
		if !ok {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(rawContent, &blocks) != nil {
			continue
		}

		kept := make([]json.RawMessage, 0, len(blocks))
		removed := false
		for _, b := range blocks {
			var t struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(b, &t)
			if t.Type != "thinking" {
				kept = append(kept, b)
				continue
			}

			// 是 thinking 块。只有「空且无签名、且只有 type/thinking 两个字段」才能删。
			var fields map[string]json.RawMessage
			if json.Unmarshal(b, &fields) != nil {
				return nil, false
			}
			if _, has := fields["signature"]; has {
				return nil, false
			}
			var text string
			if raw, has := fields["thinking"]; !has || json.Unmarshal(raw, &text) != nil || text != "" {
				return nil, false
			}
			if len(fields) != 2 {
				// 还有别的字段（比如 cache_control），删掉会改变语义
				return nil, false
			}
			removed = true
		}

		if !removed {
			continue
		}
		// 删空了补一个空 text 块——上游不接受 content 为空的 assistant 消息
		if len(kept) == 0 {
			empty, _ := json.Marshal(map[string]any{"type": "text", "text": ""})
			kept = append(kept, empty)
		}
		nb, err := json.Marshal(kept)
		if err != nil {
			return nil, false
		}
		m["content"] = nb
		changed = true
	}

	if !changed {
		return nil, true
	}
	// 必须把改过的 messages 写回 root：
	// msgs 是从 root["messages"] 反序列化出来的独立副本，
	// 只改 msgs 再 Marshal(root) 等于什么都没做（踩过）。
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return nil, false
	}
	root["messages"] = newMsgs

	out, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	return out, true
}

// IsEnterpriseAccount 判断账号是否属于企业租户。
//
// 上游用账号的 enterpriseId 字段判定（WorkBuddyChatGatewayAdapter.java:31）。
// 本项目的账号模型里没有企业身份位——登录链路一律带 X-No-Enterprise-Id: true，
// 拿到的都是个人版 token，所以固定返回 false。
//
// 保留这个函数而不是直接写 false：将来若接入企业账号，
// 只需改这里，不用回头翻调用点。
func IsEnterpriseAccount(a *config.Account) bool {
	if a == nil {
		return false
	}
	// 目前没有企业身份字段；Realm 只区分 cn/global，不代表企业。
	return false
}
