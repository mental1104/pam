package httpapi

import (
	"github.com/mental1104/pam/internal/domain"
	"net/http"
	"strconv"
)

// auditDashboard 从 r 读取唯一筛选值，w 返回同一快照的图表、分组和分页；不提供任意 SQL 执行入口。
func (a *API) auditDashboard(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, err)
		return
	}
	q := r.URL.Query()
	if q.Get("limit") == "" {
		limit = 20
	}
	for key, values := range q {
		if len(values) != 1 {
			respondError(w, domain.ValidationError{Message: "audit filters must be single values"})
			return
		}
		switch key {
		case "from", "to", "status", "error_code", "fingerprint", "source", "search", "sort", "min_duration_ms", "limit", "offset":
		default:
			respondError(w, domain.ValidationError{Message: "unknown audit filter"})
			return
		}
	}
	var minimum int64
	if q.Get("min_duration_ms") != "" {
		minimum, err = strconv.ParseInt(q.Get("min_duration_ms"), 10, 64)
		if err != nil {
			respondError(w, domain.ValidationError{Message: "minimum duration must be integer milliseconds"})
			return
		}
	}
	result, err := a.service.AuditDashboard(r.Context(), domain.AuditFilter{From: q.Get("from"), To: q.Get("to"), Status: q.Get("status"), ErrorCode: q.Get("error_code"), Fingerprint: q.Get("fingerprint"), Search: q.Get("search"), Source: q.Get("source"), Sort: q.Get("sort"), MinDuration: minimum, Limit: limit, Offset: offset})
	respond(w, 200, result, err)
}
