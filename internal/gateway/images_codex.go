package gateway

import (
	"encoding/json"
	"log"
	"net/http"

	"allin/internal/provider/codex"
)

// Codex 生图路由。
//
// 生图和聊天不是同一条路：它有自己的端点、自己的请求体、
// 并且只认 ChatGPT 登录态账号。所以在 forward 之前单独分流，
// 不塞进通用转发逻辑里 —— 那套逻辑会改写 model、套 context 裁剪，
// 对生图请求都是错的。

// handleImagesCodex 处理 model 为 gpt-image-* 的 /v1/images/generations 请求。
//
// 返回 handled=false 表示这次请求不归它管，调用方继续走原有转发。
func (s *Server) handleImagesCodex(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var head struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &head) != nil {
		return false
	}
	if !codex.IsImageModel(head.Model) {
		return false
	}

	cfg := s.store.Snapshot()
	prov := cfg.FindProvider(codex.ProviderID)
	if prov == nil || !prov.IsEnabled() {
		s.fail(w, http.StatusBadRequest,
			"生图需要 codex 供应商：请先在「供应商」里添加 OpenAI Codex 并登录 ChatGPT 账号")
		return true
	}

	payload, err := codex.BuildImagePayload(body)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return true
	}

	// 生图慢，给足时间；换号只做一轮，失败就如实回报。
	exclude := map[string]bool{}
	var lastErr string
	for attempt := 0; attempt < 2; attempt++ {
		acct := s.pool.Pick(prov, exclude)
		if acct == nil {
			break
		}
		exclude[acct.ID] = true

		raw, gerr := codex.GenerateImage(r.Context(), codex.ImageAccount{
			Access:    acct.Key(),
			AccountID: acct.UID,
		}, payload)
		if gerr == nil {
			s.pool.Success(prov.ID, acct.ID)
			log.Printf("[IMG] ok model=%s account=%s size=%d", head.Model, acct.ID, len(raw))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(raw)
			return true
		}

		lastErr = gerr.Error()
		var he *codex.ImageHTTPError
		if ok := asImageHTTPErr(gerr, &he); ok {
			// 401/403 是账号问题，换个号可能就好；其余直接回报。
			if he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden {
				s.pool.Fail(prov.ID, acct.ID, he.Status, he.Body)
				continue
			}
			s.fail(w, he.Status, "生图失败："+he.Body)
			return true
		}
		s.pool.Fail(prov.ID, acct.ID, 0, lastErr)
	}

	if lastErr == "" {
		lastErr = "没有可用的 codex 账号"
	}
	s.fail(w, http.StatusBadGateway, "生图失败："+lastErr)
	return true
}

func asImageHTTPErr(err error, out **codex.ImageHTTPError) bool {
	if e, ok := err.(*codex.ImageHTTPError); ok {
		*out = e
		return true
	}
	return false
}
