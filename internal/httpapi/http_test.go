package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mental1104/pam/internal/app"
)

// TestAuthenticationAndCancellation 验证未认证请求不会进入业务，认证后取消能够到达处理器并释放执行。
// t 提供失败报告；仅使用合成 token，不建立数据库或保存真实凭据。
func TestAuthenticationAndCancellation(t *testing.T) {
	token := strings.Repeat("x", 32)
	entered := make(chan struct{})
	finished := make(chan struct{})
	bounded := make(chan bool, 1)
	// 处理器代表一个等待数据库的业务操作，必须拥有有限期限并响应上游取消。
	handler := authenticate(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		bounded <- ok && time.Until(deadline) <= 15*time.Second
		close(entered)
		<-r.Context().Done()
		w.WriteHeader(503)
	}))
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", "/", nil))
	if unauthorized.Code != 401 {
		t.Fatalf("expected HTTP401, got%d", unauthorized.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	// 测试 goroutine 只拥有当前请求；finished 同步确保取消后的断言没有共享数据竞态。
	go func() { handler.ServeHTTP(httptest.NewRecorder(), request); close(finished) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("authenticated operation did not begin")
	}
	if !<-bounded {
		t.Error("authenticated operation has no bounded deadline")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach operation")
	}
}

// TestHealthFailurePrivacy 验证就绪依赖失败只公开固定提示；t 使用合成敏感字符串检查响应正文。
func TestHealthFailurePrivacy(t *testing.T) {
	handler := New(nil, http.NotFoundHandler(), strings.Repeat("x", 32), func(ctx context.Context) error { return errors.New("synthetic-private-database-password") }, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/health/ready", nil))
	if response.Code != 503 || strings.Contains(response.Body.String(), "synthetic-private") {
		t.Fatal("health failure leaked dependency details or returned success")
	}
}

// TestPlatformFilterContractRejectsInvalidInput 验证公开参数只接受单个标准枚举，错误在数据库访问前返回。
// t 使用没有连接池的服务，任何错误绕过输入校验都会令场景失败，不会连接生产数据库。
func TestPlatformFilterContractRejectsInvalidInput(t *testing.T) {
	token := strings.Repeat("p", 32)
	handler := New(app.New(nil), http.NotFoundHandler(), token, func(ctx context.Context) error { return ctx.Err() }, nil)
	for _, query := range []string{"platform=unknown", "platform=all", "platform=wechat%3Bcmb", "platform=wechat&platform=alipay", "platform=other&platform=other"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/transactions?"+query, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid platform query returned HTTP %d", w.Code)
		}
	}
}

// TestAuditAuthenticationAndValidation 验证审计接口仍受认证约束，非法筛选在访问数据库前被拒绝。
func TestAuditAuthenticationAndValidation(t *testing.T) {
	token := strings.Repeat("a", 32)
	handler := New(app.New(nil), http.NotFoundHandler(), token, func(context.Context) error { return nil }, nil)
	for _, path := range []string{"/api/v1/audit?status=invalid", "/api/v1/audit?status=error&status=success", "/api/v1/audit?min_duration_ms=NaN", "/api/v1/audit?from=2026-11-25", "/api/v1/audit?limit=101", "/api/v1/audit?fingerprint=bad", "/api/v1/audit?unknown=1"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != 400 {
			t.Fatalf("expected validation failure for %s: %d", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/audit", nil))
	if response.Code != 401 {
		t.Fatal("audit is not protected")
	}
}
