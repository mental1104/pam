package webui

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ssoFixture 提供纯合成的签名身份与可切换 JWKS 服务；不读取真实 DSM 会话、私钥或财务数据。
// body、status、calls 允许测试并发读写服务状态；server 由测试清理，key 仅用于签署测试断言。
type ssoFixture struct {
	server *httptest.Server
	key    *ecdsa.PrivateKey
	body   atomic.Value
	status atomic.Int32
	calls  atomic.Int32
}

// newSSOFixture 为 t 创建 ES256 公钥端点并注册清理；返回的私钥仅存在于当前测试内存中。
func newSSOFixture(t *testing.T) *ssoFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &ssoFixture{key: key}
	f.status.Store(http.StatusOK)
	f.setKeys(t, []ssoJWK{ecSSOKey("first", &key.PublicKey)})
	// 模拟远端只读 JWKS；所有响应均为合成公钥，不涉及身份提供方或真实网络。
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.URL.Path != ssoJWKSPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(f.status.Load()))
		_, _ = w.Write([]byte(f.body.Load().(string)))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// setKeys 将 keys 编码为当前 fixture 的 JWKS；t 接收编码失败，不改变服务状态码。
func (f *ssoFixture) setKeys(t *testing.T, keys []ssoJWK) {
	t.Helper()
	body, err := json.Marshal(ssoJWKSet{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	f.body.Store(string(body))
}

// ecSSOKey 把合成 P-256 公钥 key 编码为指定 kid 的 JWKS 项；不包含任何私钥字段。
func ecSSOKey(kid string, key *ecdsa.PublicKey) ssoJWK {
	return ssoJWK{
		KID: kid, KTY: "EC", Use: "sig", Alg: "ES256", CRV: "P-256",
		X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

// config 返回 fixture 公钥端点和真实部署格式的固定主机名；所有身份值均为合成测试输入。
func (f *ssoFixture) config() SSOConfig {
	return SSOConfig{
		JWKSURL: f.server.URL + ssoJWKSPath, Issuer: "pam.example.com",
		Audience: "pam.example.com", Username: "synthetic-owner",
	}
}

// ssoTestClaims 构造当前有效的 Pomerium 风格声明，再按 scenario 引入一个独立安全边界错误。
// 返回的新 map 只供当前测试修改，不保存真实用户标识或完整身份提供方令牌。
func ssoTestClaims(scenario string) jwt.MapClaims {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": "pam.example.com", "aud": "pam.example.com", "sub": "synthetic-owner",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(4 * time.Minute).Unix(), "username": "synthetic-owner",
	}
	switch scenario {
	case "wrong_issuer":
		claims["iss"] = "sso.example.com"
	case "missing_issuer":
		delete(claims, "iss")
	case "wrong_audience":
		claims["aud"] = "doc.example.com"
	case "missing_audience":
		delete(claims, "aud")
	case "expired":
		claims["exp"] = now.Add(-time.Minute).Unix()
	case "missing_expiry":
		delete(claims, "exp")
	case "future_issued_at":
		claims["iat"] = now.Add(time.Minute).Unix()
	case "future_not_before":
		claims["nbf"] = now.Add(time.Minute).Unix()
	case "wrong_username":
		claims["username"] = "synthetic-other"
	case "missing_username":
		delete(claims, "username")
	case "multiple_usernames":
		claims["username"] = "synthetic-owner,synthetic-other"
	case "username_array":
		claims["username"] = []string{"synthetic-owner"}
	}
	return claims
}

// signSSO 使用 method/key 为 claims 签名；kid 为空时故意省略键标识，t 接收签名失败。
func signSSO(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	// 即使断言携带地址，也必须只读取部署指定的 JWKS；此不可解析地址不会被访问。
	token.Header["jku"] = "https://untrusted.invalid/keys"
	value, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// TestSSOLoginAuthenticatesSignedOwner 验证登录只接受正确签名与全部声明，失败不发放会话。
// t 管理独立请求；成功还检查安全 Cookie、无缓存响应和固定首页重定向。
func TestSSOLoginAuthenticatesSignedOwner(t *testing.T) {
	f := newSSOFixture(t)
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	scenarios := []string{
		"valid", "wrong_issuer", "missing_issuer", "wrong_audience", "missing_audience",
		"expired", "missing_expiry", "future_issued_at", "future_not_before", "wrong_username",
		"missing_username", "multiple_usernames", "username_array", "wrong_signature", "hs256",
		"none", "missing_kid", "unknown_kid", "malformed", "missing_assertion", "duplicate_header",
		"header_identity_only",
	}
	for _, scenario := range scenarios {
		// 各场景使用独立会话管理器；fixture 公钥不变，错误来自本次合成断言。
		t.Run(scenario, func(t *testing.T) {
			s := New("synthetic-access-token-with-at-least-32-bytes")
			if err := s.ConfigureSSO(f.config()); err != nil {
				t.Fatal(err)
			}
			method, key, kid := jwt.SigningMethod(jwt.SigningMethodES256), any(f.key), "first"
			switch scenario {
			case "wrong_signature":
				key = other
			case "hs256":
				method, key = jwt.SigningMethodHS256, []byte("synthetic-hmac-secret")
			case "none":
				method, key = jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType
			case "missing_kid":
				kid = ""
			case "unknown_kid":
				kid = "unknown"
			}
			assertion := signSSO(t, method, key, kid, ssoTestClaims(scenario))
			if scenario == "malformed" {
				assertion = "invalid.assertion"
			}
			req := httptest.NewRequest(http.MethodGet, "https://pam.example.com/web/sso", nil)
			if scenario != "missing_assertion" && scenario != "header_identity_only" {
				req.Header.Set("X-Pomerium-Jwt-Assertion", assertion)
			}
			if scenario == "duplicate_header" {
				req.Header.Add("X-Pomerium-Jwt-Assertion", assertion)
			}
			req.Header.Set("X-Documents-User", "synthetic-owner")
			response := httptest.NewRecorder()
			s.SSOLogin(response, req)
			if scenario != "valid" {
				if response.Code != http.StatusUnauthorized || len(response.Result().Cookies()) != 0 {
					t.Fatalf("invalid assertion issued a session: status=%d", response.Code)
				}
				return
			}
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unexpected successful login response: status=%d", response.Code)
			}
			cookies := response.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != cookieName || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Domain != "" || cookies[0].Path != "/" {
				t.Fatal("successful login did not issue a protected host cookie")
			}
			check := httptest.NewRequest(http.MethodGet, "https://pam.example.com/web/session", nil)
			check.AddCookie(cookies[0])
			if !s.valid(check) || strings.Contains(response.Body.String(), assertion) {
				t.Fatal("session was invalid or the assertion leaked")
			}
		})
	}
}

// TestSSOConfigurationAndMethod 拒绝不明确的信任配置；未配置和错误方法均不得创建会话。
// t 的子场景不发起网络请求，确保配置检查不会提早访问身份系统。
func TestSSOConfigurationAndMethod(t *testing.T) {
	f := newSSOFixture(t)
	for _, scenario := range []string{"empty_url", "wrong_scheme", "credentials", "query", "fragment", "wrong_path", "issuer", "audience", "username"} {
		// 每次只破坏一个配置字段，以证明各信任边界会独立快速失败。
		t.Run(scenario, func(t *testing.T) {
			config := f.config()
			switch scenario {
			case "empty_url":
				config.JWKSURL = ""
			case "wrong_scheme":
				config.JWKSURL = "file:///" + ssoJWKSPath
			case "credentials":
				config.JWKSURL = "http://user:password@pomerium:8443" + ssoJWKSPath
			case "query":
				config.JWKSURL += "?alternate=true"
			case "fragment":
				config.JWKSURL += "#alternate"
			case "wrong_path":
				config.JWKSURL = f.server.URL + "/another-key-source"
			case "issuer":
				config.Issuer = ""
			case "audience":
				config.Audience = " pam.example.com"
			case "username":
				config.Username = ""
			}
			s := New("synthetic-token")
			if s.ConfigureSSO(config) == nil || s.sso != nil {
				t.Fatal("invalid configuration installed a verifier")
			}
		})
	}
	if f.calls.Load() != 0 {
		t.Fatal("configuration unexpectedly fetched signing keys")
	}
	s := New("synthetic-token")
	for method, status := range map[string]int{http.MethodGet: http.StatusServiceUnavailable, http.MethodPost: http.StatusMethodNotAllowed} {
		response := httptest.NewRecorder()
		s.SSOLogin(response, httptest.NewRequest(method, "https://pam.example.com/web/sso", nil))
		if response.Code != status || len(response.Result().Cookies()) != 0 {
			t.Fatalf("unexpected unconfigured SSO status: %d", response.Code)
		}
	}
}

// TestSSOKeyCacheConcurrentAndFailClosed 证明并发验签共用一次取钥，未知 kid 不刷新，过期失败不使用旧键。
// t 管理同步屏障，所有签名和远端错误均在本地合成，不执行生产网络操作。
func TestSSOKeyCacheConcurrentAndFailClosed(t *testing.T) {
	f := newSSOFixture(t)
	s := New("synthetic-token")
	if err := s.ConfigureSSO(f.config()); err != nil {
		t.Fatal(err)
	}
	assertion := signSSO(t, jwt.SigningMethodES256, f.key, "first", ssoTestClaims("valid"))
	var workers sync.WaitGroup
	results := make(chan error, 24)
	for i := 0; i < cap(results); i++ {
		workers.Add(1)
		// 多个调用同时首次登录，互斥缓存只允许一个取钥请求。
		go func() {
			defer workers.Done()
			results <- s.sso.verify(context.Background(), assertion)
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	unknown := signSSO(t, jwt.SigningMethodES256, f.key, "unknown", ssoTestClaims("valid"))
	if s.sso.verify(context.Background(), unknown) == nil || f.calls.Load() != 1 {
		t.Fatal("unknown signing key refreshed a live cache")
	}
	// 缓存到期后只信任新公布的轮换公钥；已经移除的旧 kid 不得继续验签。
	rotated, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.setKeys(t, []ssoJWK{ecSSOKey("rotated", &rotated.PublicKey)})
	s.sso.mu.Lock()
	s.sso.expires = time.Time{}
	s.sso.mu.Unlock()
	rotatedAssertion := signSSO(t, jwt.SigningMethodES256, rotated, "rotated", ssoTestClaims("valid"))
	if s.sso.verify(context.Background(), rotatedAssertion) != nil || s.sso.verify(context.Background(), assertion) == nil || f.calls.Load() != 2 {
		t.Fatal("signing key rotation did not replace the expired cache")
	}
	// 模拟缓存到期且 JWKS 停止服务：此前合法的断言也必须失败，不延长旧公钥的信任时间。
	s.sso.mu.Lock()
	s.sso.expires = time.Time{}
	s.sso.mu.Unlock()
	f.status.Store(http.StatusServiceUnavailable)
	if s.sso.verify(context.Background(), rotatedAssertion) == nil || s.sso.verify(context.Background(), rotatedAssertion) == nil || f.calls.Load() != 3 {
		t.Fatal("expired keys were reused or failed fetches were not bounded")
	}
}

// TestSSOCancelledLoginDoesNotPoisonCache 证明一个取消的浏览器请求不会阻止随后合法用户取钥。
// t 的第一个请求在发出前取消，第二个请求必须立刻正常完成，无需等待失败退避时间。
func TestSSOCancelledLoginDoesNotPoisonCache(t *testing.T) {
	f := newSSOFixture(t)
	s := New("synthetic-token")
	if err := s.ConfigureSSO(f.config()); err != nil {
		t.Fatal(err)
	}
	assertion := signSSO(t, jwt.SigningMethodES256, f.key, "first", ssoTestClaims("valid"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.sso.verify(ctx, assertion) == nil {
		t.Fatal("cancelled login unexpectedly succeeded")
	}
	if s.sso.verify(context.Background(), assertion) != nil || f.calls.Load() != 1 {
		t.Fatal("cancelled login poisoned the shared key cache")
	}
}

// TestSSORejectsUntrustedKeyDocuments 覆盖公钥响应的大小、类型、重复标识与畸形数据边界。
// t 的每个场景使用独立缓存，防止之前的合法公钥掩盖远端无效响应。
func TestSSORejectsUntrustedKeyDocuments(t *testing.T) {
	f := newSSOFixture(t)
	valid := ecSSOKey("first", &f.key.PublicKey)
	for _, scenario := range []string{"malformed", "oversized", "empty", "too_many", "duplicate", "wrong_curve", "off_curve", "algorithm_type", "symmetric", "encryption_only"} {
		// 以下响应均由测试服务生成；不允许任何错误响应降级成未经验证的身份。
		t.Run(scenario, func(t *testing.T) {
			key := valid
			keys := []ssoJWK{key}
			switch scenario {
			case "empty":
				keys = nil
			case "too_many":
				keys = make([]ssoJWK, 17)
			case "duplicate":
				keys = append(keys, key)
			case "wrong_curve":
				key.CRV = "P-384"
				keys[0] = key
			case "off_curve":
				key.X = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				key.Y = key.X
				keys[0] = key
			case "algorithm_type":
				key.KTY = "RSA"
				keys[0] = key
			case "symmetric":
				key.Alg, key.KTY = "HS256", "oct"
				keys[0] = key
			case "encryption_only":
				key.Use = "enc"
				keys[0] = key
			}
			f.setKeys(t, keys)
			if scenario == "malformed" {
				f.body.Store("not-json")
			}
			if scenario == "oversized" {
				f.body.Store(strings.Repeat(" ", ssoJWKSMaxBytes+1))
			}
			s := New("synthetic-token")
			if err := s.ConfigureSSO(f.config()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.sso.fetchKeys(context.Background()); err == nil {
				t.Fatal("untrusted key document was accepted")
			}
		})
	}
}

// TestSSORS256AndWeakKeyBoundary 使用合成 2048 位 RSA 证明受支持算法可登录，并独立拒绝弱模数。
// t 只生成一次私钥，不从磁盘读取部署密钥。
func TestSSORS256AndWeakKeyBoundary(t *testing.T) {
	f := newSSOFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := ssoJWK{
		KID: "rsa", KTY: "RSA", Use: "sig", Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
	f.setKeys(t, []ssoJWK{jwk})
	s := New("synthetic-token")
	if err := s.ConfigureSSO(f.config()); err != nil {
		t.Fatal(err)
	}
	assertion := signSSO(t, jwt.SigningMethodRS256, key, "rsa", ssoTestClaims("valid"))
	if err := s.sso.verify(context.Background(), assertion); err != nil {
		t.Fatal("valid RSA assertion was rejected")
	}
	jwk.N = base64.RawURLEncoding.EncodeToString(make([]byte, 128))
	if _, err := jwk.publicKey(); err == nil {
		t.Fatal("weak RSA modulus was accepted")
	}
}

// TestSSOFixedJWKSNoRedirectAndCancellation 证明取钥不能沿重定向扩大信任，且请求取消会中止网络等待。
// t 启动的本地服务不记录或接收真实身份令牌。
func TestSSOFixedJWKSNoRedirectAndCancellation(t *testing.T) {
	var redirectHits atomic.Int32
	// 重定向目标只计算命中次数；正确实现绝不会访问它。
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	// 配置端点故意重定向到另一个服务，以验证客户端不会自动跟随。
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+ssoJWKSPath, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	s := New("synthetic-token")
	config := SSOConfig{JWKSURL: redirect.URL + ssoJWKSPath, Issuer: "pam.example.com", Audience: "pam.example.com", Username: "synthetic-owner"}
	if err := s.ConfigureSSO(config); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sso.fetchKeys(context.Background()); err == nil || redirectHits.Load() != 0 {
		t.Fatal("JWKS redirect expanded the trust boundary")
	}
	// 先取消调用上下文，证明取钥不会忽略取消后继续启动网络操作。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.sso.fetchKeys(ctx); err == nil {
		t.Fatal("cancelled key fetch unexpectedly succeeded")
	}
}
