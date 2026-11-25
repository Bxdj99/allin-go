package gateway

import (
	"encoding/json"
	"log"
)

// 缓存前缀保护。
//
// 背景：上游的前缀缓存（prompt cache）要求「前面的字节完全一致」才能命中。
// 缓存命中的输入按约 10% 计价，没命中按全价。
//
// 我们自己有两个功能会改写请求体，一旦触发就会打乱前缀：
//
//	历史压缩  —— 把较早的工具调用组摘要化，直接改动中段内容
//	上下文裁剪 —— 丢掉较早的消息，从中间截断
//
// 风险在于：这些功能「省下的 token」可能远小于「丢失的缓存折扣」。
// 举例（真实数据）：
//	单次输入 20 万 token，其中工具定义 4.2 万在最前面
//	若压缩省下 3 万 token，但让整段 20 万都变成非缓存 → 反而更贵
//
// 所以这里做两件事：
//  1. 静态估算「这次改写值不值」，不值得就跳过并说明原因
//  2. 无条件打日志，让用户知道缓存被破坏了

// cacheBreakMinSaving 是「值得为缓存损失而改写的」最小节省比例。
//
// 取值依据：缓存命中价约为全价的 10%，即命中部分省 90%。
// 若改写只减少了 r 比例的输入，却让原本可命中的部分全部失效，
// 那么改写收益约 r，损失约 0.9 *（原本可命中比例）。
// 保守取 30%：低于这个值，改写大概率是亏的。
const cacheBreakMinSaving = 0.30

// cachePrefixRewriteWorthwhile 判断一次请求体改写是否值得（会破坏缓存前缀）。
//
//	before 改写前估算 token
//	after  改写后估算 token
//	cacheLikely 该会话此前是否可能已建立缓存
//
// 返回 (是否值得, 原因)。
func cachePrefixRewriteWorthwhile(before, after int, cacheLikely bool) (bool, string) {
	if before <= 0 || after < 0 {
		return false, "数据不足"
	}
	if after >= before {
		return false, "没有实际减少"
	}
	if !cacheLikely {
		// 本来就没有缓存可失，能省就是赚
		return true, "无缓存可破坏"
	}
	saving := float64(before-after) / float64(before)
	if saving < cacheBreakMinSaving {
		return false,
			"改写只省 " + pct(saving) + "，但会让整段前缀缓存失效，不划算"
	}
	return true, "省 " + pct(saving) + "，超过缓存损失阈值"
}

func pct(v float64) string {
	if v < 0 {
		v = 0
	}
	// 保留一位小数，复用文件内已有的 itoaSmall
	n := int(v*1000 + 0.5)
	return itoaSmall(n/10) + "." + itoaSmall(n%10) + "%"
}

// EstimateBodyTokens 粗估一个请求体的 token 数，用于判断改写是否划算。
//
// 口径与 trim.go 保持一致：CJK 1 字符 1 token，其余 4 字符 1 token。
// 只做数量级判断，不需要精确。
func EstimateBodyTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range string(body) {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// toolsTokenShare 估算 tools 字段占整个请求体的比例。
//
// 工具定义在最前面且每轮都发，是缓存命中价值的核心来源：
// 只要 tools 不变、system 不变、历史只追加，前缀就能复用。
func toolsTokenShare(body []byte) (int, float64) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return 0, 0
	}
	raw, ok := m["tools"]
	if !ok || len(raw) == 0 {
		return 0, 0
	}
	toolsTok := EstimateBodyTokens(raw)
	total := EstimateBodyTokens(body)
	if total <= 0 {
		return toolsTok, 0
	}
	return toolsTok, float64(toolsTok) / float64(total)
}

// logCachePrefixRisk 在改写请求体后打一条提示，说明缓存可能失效。
func logCachePrefixRisk(stage string, before, after int, reason string) {
	if before <= 0 {
		return
	}
	log.Printf("[CACHE-RISK] %s：%d → %d tokens（%s）；前缀已改变，本轮大概率无法复用缓存",
		stage, before, after, reason)
}
