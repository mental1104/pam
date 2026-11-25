package httpapi

import (
	"github.com/mental1104/pam/internal/domain"
	"net/http"
)

// propose 将 r 的批次持久化为只读预检，w 返回候选及提案 ID。
func (a *API) propose(w http.ResponseWriter, r *http.Request) {
	var req domain.ImportRequest
	if err := decode(r, &req); err != nil {
		respondError(w, err)
		return
	}
	v, err := a.service.Propose(r.Context(), req)
	respond(w, 200, v, err)
}

// proposal 从 r 的 id 读取跨会话预检，w 返回原请求及预检结果。
func (a *API) proposal(w http.ResponseWriter, r *http.Request) {
	v, err := a.service.Proposal(r.Context(), r.PathValue("id"))
	respond(w, 200, v, err)
}

// confirm 仅确认 r 指定的已保存预检，w 返回重新查重后的实际入库结果。
func (a *API) confirm(w http.ResponseWriter, r *http.Request) {
	v, err := a.service.Confirm(r.Context(), r.PathValue("id"))
	respond(w, 200, v, err)
}

// reviews 按 r 的状态及分页读取持久待核，w 返回同一应用服务的数据。
func (a *API) reviews(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, err)
		return
	}
	v, err := a.service.Reviews(r.Context(), r.URL.Query().Get("state"), limit, offset)
	respond(w, 200, v, err)
}

// resolve 解码 r 的明确人工决定及理由，w 返回持久决定或冲突。
func (a *API) resolve(w http.ResponseWriter, r *http.Request) {
	var d domain.ReviewDecision
	if err := decode(r, &d); err != nil {
		respondError(w, err)
		return
	}
	v, err := a.service.Resolve(r.Context(), r.PathValue("id"), d)
	respond(w, 200, v, err)
}

// ranges 向 w 返回记录日期范围，r 提供认证后的取消上下文。
func (a *API) ranges(w http.ResponseWriter, r *http.Request) {
	v, err := a.service.SourceRanges(r.Context())
	respond(w, 200, v, err)
}
