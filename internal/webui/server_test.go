package webui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mental1104/pam/internal/app"
	"github.com/mental1104/pam/internal/httpapi"
	"github.com/mental1104/pam/internal/webui"
)

const accessToken = "pam-browser-fixture-token-000000000000000000000000"
const origin = "https://pam.example.test"
const browserCookieName = "__Host-pam_session"

// newAPI 使用真实路由与认证中间件建立隔离夹具；业务输入在校验层结束，不建立数据库连接。
// t 仅标记调用辅助函数；返回 Handler 不监听端口，MCP 替身只记录认证是否已通过。
func newAPI(t *testing.T) http.Handler {
	t.Helper()
	return httpapi.New(app.New(nil), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 真实 Bearer 中间件已经执行；替身不回显 Authorization 或其它请求头。
		w.WriteHeader(http.StatusNoContent)
	}), accessToken, func(ctx context.Context) error { return ctx.Err() }, nil)
}

// request 把 method/path/body 和可选 Cookie/Origin/Bearer 交给 handler；返回响应及未被适配前的请求。
// handler 仅在本进程运行；所有正文和凭据都是合成夹具，函数不访问网络或业务存储。
func request(handler http.Handler, method, path, body, requestOrigin, authorization string, cookie *http.Cookie) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(method, origin+path, strings.NewReader(body))
	if requestOrigin != "" {
		r.Header.Set("Origin", requestOrigin)
	}
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w, r
}

// login 通过公开会话入口使用正确凭据登录 handler；t 失败时不会打印令牌或 Cookie。
// 返回的随机 Cookie 仅保存在测试内存中，用于模拟浏览器后续请求。
func login(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	w, _ := request(handler, http.MethodPost, "/web/session", `{"token":"`+accessToken+`"}`, origin, "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("login returned HTTP %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatal("login returned an unexpected response body")
	}
	for _, values := range w.Header() {
		for _, value := range values {
			if strings.Contains(value, accessToken) {
				t.Fatal("login response header exposed long-term credential")
			}
		}
	}
	response := w.Result()
	defer response.Body.Close()
	for _, cookie := range response.Cookies() {
		if cookie.Name == browserCookieName {
			return cookie
		}
	}
	t.Fatal("login omitted session cookie")
	return nil
}

// authenticated 读取公开会话状态；t 要求固定 JSON 形状，返回是否已登录而不输出正文。
func authenticated(t *testing.T, handler http.Handler, cookie *http.Cookie) bool {
	t.Helper()
	w, _ := request(handler, http.MethodGet, "/web/session", "", "", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("session status returned HTTP %d", w.Code)
	}
	var state struct {
		Authenticated bool `json:"authenticated"`
	}
	if json.Unmarshal(w.Body.Bytes(), &state) != nil {
		t.Fatal("invalid session status JSON")
	}
	if strings.Contains(w.Body.String(), accessToken) || (cookie != nil && strings.Contains(w.Body.String(), cookie.Value)) {
		t.Fatal("session status exposed credentials")
	}
	return state.Authenticated
}

// TestBrowserLoginAndCookieAttributes 验证成功登录、Cookie 安全标志及不回显长期凭据；t 使用公开 HTTP 入口。
func TestBrowserLoginAndCookieAttributes(t *testing.T) {
	handler := newAPI(t)
	if authenticated(t, handler, nil) {
		t.Fatal("anonymous session authenticated")
	}
	cookie := login(t, handler)
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge <= 0 || cookie.Expires.IsZero() {
		t.Fatal("session cookie security attributes are incomplete")
	}
	if cookie.Value == accessToken || len(cookie.Value) < 32 {
		t.Fatal("session identifier is not independent of access token")
	}
	if !authenticated(t, handler, cookie) {
		t.Fatal("new session was not accepted")
	}
	w, _ := request(handler, http.MethodPost, "/web/session", `{"token":"wrong-token"}`, origin, "", nil)
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Fatal("incorrect credential created a session")
	}
	if strings.Contains(w.Body.String(), accessToken) {
		t.Fatal("login response exposed long-term credential")
	}
}

// TestBrowserLoginRejectsInvalidOrigins 验证跨源、降级及伪造代理来源都不能建立会话；t 只用合成 Token。
func TestBrowserLoginRejectsInvalidOrigins(t *testing.T) {
	handler := newAPI(t)
	for _, value := range []string{"", "null", "https://evil.example.test", "https://pam.example.test.evil.invalid", "http://pam.example.test", origin + "/path", origin + "?query=1", origin + "#fragment", "https://user@pam.example.test"} {
		w, _ := request(handler, http.MethodPost, "/web/session", `{"token":"`+accessToken+`"}`, value, "", nil)
		if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
			t.Fatal("invalid origin created a browser session")
		}
	}
	r := httptest.NewRequest(http.MethodPost, "http://internal:8080/web/session", strings.NewReader(`{"token":"`+accessToken+`"}`))
	r.Header.Set("Origin", origin)
	r.Header.Set("X-Forwarded-Host", "pam.example.test")
	r.Header.Set("Forwarded", "proto=https;host=pam.example.test")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("forwarded header replaced the request host boundary")
	}
}

// TestBrowserSessionExpiryAndForgery 使用虚拟时间验证绝对到期；t 不睡眠真实小时，也不改变生产时钟。
func TestBrowserSessionExpiryAndForgery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler := newAPI(t)
		cookie := login(t, handler)
		forged := *cookie
		forged.Value = strings.Repeat("x", len(cookie.Value))
		if authenticated(t, handler, &forged) {
			t.Fatal("forged cookie was accepted")
		}
		if !authenticated(t, handler, cookie) {
			t.Fatal("legitimate cookie was invalidated by a forged request")
		}
		time.Sleep(13 * time.Hour)
		if authenticated(t, handler, cookie) {
			t.Fatal("expired cookie was accepted")
		}
		w, _ := request(handler, http.MethodGet, "/api/v1/assets/not-a-uuid", "", "", "", cookie)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("expired cookie reached protected REST API")
		}
	})
}

// TestBrowserLogoutRevokesSession 验证退出同时清除浏览器 Cookie 和服务端资格，跨源退出不影响原会话。
// t 的 REST 写请求都在身份或域校验阶段结束，没有数据库写入。
func TestBrowserLogoutRevokesSession(t *testing.T) {
	handler := newAPI(t)
	cookie := login(t, handler)
	w, _ := request(handler, http.MethodDelete, "/web/session", "", "https://evil.example.test", "", cookie)
	if w.Code != http.StatusForbidden || !authenticated(t, handler, cookie) {
		t.Fatal("cross-origin logout revoked a session")
	}
	w, _ = request(handler, http.MethodDelete, "/web/session", "", origin, "", cookie)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout returned HTTP %d", w.Code)
	}
	cleared := false
	for _, value := range w.Result().Cookies() {
		if value.Name == browserCookieName && value.MaxAge < 0 && value.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not expire browser cookie")
	}
	if authenticated(t, handler, cookie) {
		t.Fatal("copied pre-logout cookie remained valid")
	}
	w, _ = request(handler, http.MethodPost, "/api/v1/assets", `{}`, origin, "", cookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("revoked cookie reached REST write validation")
	}
	w, _ = request(handler, http.MethodPost, "/mcp", `{}`, "", "Bearer "+accessToken, nil)
	if w.Code != http.StatusNoContent {
		t.Fatal("browser logout revoked explicit bearer access")
	}
}

// TestBrowserRESTWriteOriginAndExplicitBearer 验证 Cookie 写入的同源限制及显式凭据优先级。
// t 以缺少必填名称的资产输入触发 400，从而证明认证已通过且不触碰数据库。
func TestBrowserRESTWriteOriginAndExplicitBearer(t *testing.T) {
	handler := newAPI(t)
	cookie := login(t, handler)
	w, _ := request(handler, http.MethodPost, "/api/v1/assets", `{}`, origin, "", cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("same-origin write did not reach validation: HTTP %d", w.Code)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, value := range []string{"", "https://evil.example.test", "http://pam.example.test"} {
			w, _ = request(handler, method, "/api/v1/assets", `{}`, value, "", cookie)
			if w.Code != http.StatusForbidden {
				t.Fatalf("cross-origin %s was not rejected", method)
			}
		}
	}
	w, _ = request(handler, http.MethodPost, "/api/v1/assets", `{}`, "https://evil.example.test", "Bearer "+accessToken, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatal("explicit bearer unexpectedly acquired cookie CSRF requirements")
	}
	w, _ = request(handler, http.MethodPost, "/api/v1/assets", `{}`, origin, "Bearer wrong-token", cookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("browser cookie replaced invalid explicit credentials")
	}
}

// TestBrowserCookieDoesNotAuthenticateMCP 验证真实路由只对 REST 适配会话，MCP 仍要求显式 Bearer。
// t 使用只返回 204 的 MCP 替身，不创建数据库或执行 SQL。
func TestBrowserCookieDoesNotAuthenticateMCP(t *testing.T) {
	handler := newAPI(t)
	cookie := login(t, handler)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w, _ := request(handler, method, "/mcp", `{}`, origin, "", cookie)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("browser cookie authenticated MCP")
		}
		w, _ = request(handler, method, "/mcp", `{}`, "", "Bearer "+accessToken, cookie)
		if w.Code != http.StatusNoContent {
			t.Fatal("explicit MCP bearer was not preserved")
		}
	}
}

// TestBrowserAdapterClonesRequest 验证 Cookie 适配只修改传给下游的副本，不将长期 token 留在原请求。
// t 的下游仅检查 Header 是否正确，从不把凭据写入响应或持久状态。
func TestBrowserAdapterClonesRequest(t *testing.T) {
	browser := webui.New(accessToken)
	mux := http.NewServeMux()
	browser.Register(mux)
	cookie := login(t, mux)
	adapted := browser.WithSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+accessToken {
			t.Error("downstream did not receive verified authorization")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	w, original := request(adapted, http.MethodPost, "/api/v1/assets", `{}`, origin, "", cookie)
	if w.Code != http.StatusNoContent || original.Header.Get("Authorization") != "" {
		t.Fatal("session adaptation mutated caller request")
	}
}
