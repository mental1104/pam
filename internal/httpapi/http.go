// Package httpapi 将 REST 请求转换为应用服务调用，不直接拼装业务 SQL。
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mental1104/pam/internal/app"
	"github.com/mental1104/pam/internal/domain"
	"github.com/mental1104/pam/internal/webui"
)

// API 持有线程安全的业务服务；所有金融和库存接口经统一认证后进入。
type API struct {
	service *app.Service
	ready   func(context.Context) error
}

// New 注册 REST、MCP 和就绪探针；service 负责业务，mcp 是同服务的协议适配器。
// token 至少 32 字符并由启动层验证；ready 检查真实数据库；browser 提供网页会话，nil 使用密钥登录。
// 返回受认证和并发限制的 Handler；已配置的 browser 必须在开始服务后保持配置不变。
func New(service *app.Service, mcp http.Handler, token string, ready func(context.Context) error, browser *webui.Server) http.Handler {
	a := &API{service: service, ready: ready}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/ready", a.health)
	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/v1/transactions", a.transactions)
	protected.HandleFunc("GET /api/v1/audit", a.auditDashboard)
	protected.HandleFunc("GET /api/v1/transactions/{id}", a.transaction)
	protected.HandleFunc("POST /api/v1/imports/transactions", a.importTransactions)
	protected.HandleFunc("POST /api/v1/imports/proposals", a.propose)
	protected.HandleFunc("GET /api/v1/imports/proposals/{id}", a.proposal)
	protected.HandleFunc("POST /api/v1/imports/proposals/{id}/confirm", a.confirm)
	protected.HandleFunc("GET /api/v1/imports/reviews", a.reviews)
	protected.HandleFunc("POST /api/v1/imports/reviews/{id}/resolve", a.resolve)
	protected.HandleFunc("GET /api/v1/imports/ranges", a.ranges)
	protected.HandleFunc("GET /api/v1/assets", a.assets)
	protected.HandleFunc("POST /api/v1/assets", a.createAsset)
	protected.HandleFunc("GET /api/v1/assets/{id}", a.asset)
	protected.HandleFunc("PUT /api/v1/assets/{id}", a.updateAsset)
	protected.HandleFunc("DELETE /api/v1/assets/{id}", a.deleteAsset)
	protected.Handle("/mcp", mcp)
	if browser == nil {
		browser = webui.New(token)
	}
	browser.Register(mux)
	authenticated := authenticate(token, protected)
	// 浏览器 Cookie 仅适配 REST；MCP 和其余路径仍要求原有 Bearer。
	mux.Handle("/api/", browser.WithSession(authenticated))
	mux.Handle("/", authenticated)
	return mux
}

// authenticate 为 next 校验单用户 Bearer token；token 不落日志，每次请求常量时间比较摘要。
// 同时最多接受八个请求，避免手机和 MCP 无界占用数据库；超额返回 429。
func authenticate(token string, next http.Handler) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + token))
	slots := make(chan struct{}, 8)
	// 每次请求独立持有配额，退出处理器时释放，取消不会遗留占位。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(expected[:], supplied[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pam"`)
			writeError(w, 401, "unauthorized", "authentication required")
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeError(w, 429, "busy", "too many concurrent requests")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		// WriteTimeout 只约束套接字；显式上下文期限还必须取消数据库锁等待和业务事务。
		requestCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		r = r.WithContext(requestCtx)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// health 对 r 的请求上下文追加两秒期限，检查 ready；w 仅返回通用状态，不泄露数据库错误。
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.ready(ctx); err != nil {
		writeError(w, 503, "unavailable", "database unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

// page 从 r 的查询参数读取 limit/offset；缺省 50/0，非法整数返回输入错误而不是静默回退。
func page(r *http.Request) (int, int, error) {
	limit, offset := 50, 0
	var err error
	if s := r.URL.Query().Get("limit"); s != "" {
		limit, err = strconv.Atoi(s)
		if err != nil {
			return 0, 0, domain.ValidationError{Message: "limit must be an integer"}
		}
	}
	if s := r.URL.Query().Get("offset"); s != "" {
		offset, err = strconv.Atoi(s)
		if err != nil {
			return 0, 0, domain.ValidationError{Message: "offset must be an integer"}
		}
	}
	return limit, offset, domain.ValidatePage(limit, offset)
}

// transactions 将 r 的组合筛选转换为域过滤器；platform 只接受单值，不静默选择重复参数。
// w 返回包含来源数组和过滤后总数的分页结果，或脱敏输入错误。
func (a *API) transactions(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, err)
		return
	}
	q := r.URL.Query()
	if len(q["platform"]) > 1 {
		respondError(w, domain.ValidationError{Message: "platform must be a single value"})
		return
	}
	result, err := a.service.ListTransactions(r.Context(), domain.TransactionFilter{From: q.Get("from"), To: q.Get("to"), Search: q.Get("search"), Merchant: q.Get("merchant"), MinAmount: q.Get("min_amount"), MaxAmount: q.Get("max_amount"), Direction: q.Get("direction"), Category: q.Get("category"), Source: q.Get("source"), Platform: q.Get("platform"), Limit: limit, Offset: offset})
	respond(w, 200, result, err)
}

// transaction 根据 r 的 UUID 路径读取流水及原始来源；w 返回完整明细或 404。
func (a *API) transaction(w http.ResponseWriter, r *http.Request) {
	result, err := a.service.GetTransaction(r.Context(), r.PathValue("id"))
	respond(w, 200, result, err)
}

// importTransactions 解码 r 的规范化批次，通过应用事务导入；w 返回逐行统计，不提供任意 SQL 写入。
func (a *API) importTransactions(w http.ResponseWriter, r *http.Request) {
	var input domain.ImportRequest
	if err := decode(r, &input); err != nil {
		respondError(w, err)
		return
	}
	result, err := a.service.Import(r.Context(), input)
	respond(w, 200, result, err)
}

// assets 根据 r 的分页参数浏览库存；w 返回空数组或分页数据。
func (a *API) assets(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := page(r)
	if err != nil {
		respondError(w, err)
		return
	}
	result, err := a.service.ListAssets(r.Context(), limit, offset)
	respond(w, 200, result, err)
}

// asset 读取 r 路径中的单个库存 UUID；w 输出实体或错误。
func (a *API) asset(w http.ResponseWriter, r *http.Request) {
	result, err := a.service.GetAsset(r.Context(), r.PathValue("id"))
	respond(w, 200, result, err)
}

// createAsset 验证并持久化 r 中的库存输入；成功经 w 返回 201 和新实体。
func (a *API) createAsset(w http.ResponseWriter, r *http.Request) {
	var input domain.AssetInput
	if err := decode(r, &input); err != nil {
		respondError(w, err)
		return
	}
	result, err := a.service.CreateAsset(r.Context(), input)
	respond(w, 201, result, err)
}

// updateAsset 将 r 中的完整库存输入替换指定实体；不存在返回 404，不创建隐式新记录。
func (a *API) updateAsset(w http.ResponseWriter, r *http.Request) {
	var input domain.AssetInput
	if err := decode(r, &input); err != nil {
		respondError(w, err)
		return
	}
	result, err := a.service.UpdateAsset(r.Context(), r.PathValue("id"), input)
	respond(w, 200, result, err)
}

// deleteAsset 删除 r 指定库存；w 成功返回无正文 204，交易原始记录始终保留。
func (a *API) deleteAsset(w http.ResponseWriter, r *http.Request) {
	if err := a.service.DeleteAsset(r.Context(), r.PathValue("id")); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decode 将 r 的单一 JSON 对象解码到 out；拒绝未知字段和尾随 JSON，数字保持 json.Number。
func decode(r *http.Request, out any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return domain.ValidationError{Message: "invalid JSON request"}
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return domain.ValidationError{Message: "request must contain one JSON object"}
	}
	return nil
}

// respond 按 err 是否存在选择错误映射或向 w 写入给定 status 的 value。
func respond(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		respondError(w, err)
		return
	}
	writeJSON(w, status, value)
}

// respondError 只把稳定业务错误公开给 w；内部错误记录事件而不输出财务字段和数据库细节。
func respondError(w http.ResponseWriter, err error) {
	var validation domain.ValidationError
	switch {
	case errors.As(err, &validation):
		writeError(w, 400, "validation", validation.Message)
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, 404, "not_found", "record not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, 409, "conflict", "record conflict")
	default:
		slog.Error("request failed", "code", "internal")
		writeError(w, 500, "internal", "request failed")
	}
}

// writeError 把 code/message 封装为所有 REST 路径共享的错误结构，使用给定 status 写到 w。
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// writeJSON 把 value 编码为 JSON 后写到 w；编码失败在响应头发送前替换为安全内部错误。
func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = 500
		data = []byte(`{"error":{"code":"internal","message":"response encoding failed"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
