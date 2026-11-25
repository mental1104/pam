// Package webui 提供同源网页和短期浏览器会话；不实现业务 SQL 或保存浏览器明文密钥。
package webui

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// files 随二进制携带页面；所有内容都是不含账单和凭据的公共静态资源。
//
//go:embed static/*
var files embed.FS

const cookieName = "__Host-pam_session"
const sessionLifetime = 12 * time.Hour

// Server 管理进程内有界会话；重启或更换访问密钥会撤销全部旧浏览器会话。
// token 仅用于服务端认证适配，sessions 只保存随机会话标识摘要和绝对到期时间。
type Server struct {
	token    string
	sso      *ssoVerifier
	mu       sync.Mutex
	sessions map[[32]byte]time.Time
}

// New 使用已有单用户 token 创建独立网页适配器；调用方须保证 token 至少 32 字符。
// 返回值可并发使用，不打开数据库连接，也不改变 MCP 的认证方式。
func New(token string) *Server {
	return &Server{token: token, sessions: make(map[[32]byte]time.Time)}
}

// Register 向 mux 注册精确静态路径和会话接口；不会覆盖 API、MCP 或未知路径。
// 页面仅包含界面骨架，财务内容必须随后通过认证 REST 读取。
func (s *Server) Register(mux *http.ServeMux) {
	for route, name := range map[string]string{"GET /{$}": "index.html", "GET /app.js": "app.js", "GET /audit.js": "audit.js", "GET /styles.css": "styles.css", "GET /favicon.svg": "favicon.svg"} {
		// 每条路由捕获自己的资源名，禁止通配路径访问嵌入目录或展示目录清单。
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) { serveFile(w, r, name) })
	}
	mux.HandleFunc("GET /web/session", s.status)
	mux.HandleFunc("POST /web/session", s.login)
	mux.HandleFunc("DELETE /web/session", s.logout)
	mux.HandleFunc("GET /web/sso", s.SSOLogin)
}

// headers 为 w 禁止缓存、嵌套页面和跨源脚本；仅允许同源静态资源与 API。
func headers(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
}

// serveFile 按固定 name 把嵌入文件写到 w；r 只用于标准 GET/HEAD 和范围请求处理。
func serveFile(w http.ResponseWriter, r *http.Request, name string) {
	headers(w)
	data, err := files.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// sameOrigin 判断 r 的 Origin 是否为当前 HTTPS 主机；不信任可伪造的 Forwarded 头。
// 仅浏览器会话的状态变化要求此检查；显式 Bearer API 客户端保持原有行为。
func sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && origin.Scheme == "https" && origin.Host == r.Host && origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}

// valid 判断 r 的随机 Cookie 摘要是否仍在有效会话中；到期会话即时清除，原始标识不落盘。
func (s *Server) valid(r *http.Request) bool {
	cookie, err := r.Cookie(cookieName)
	if err != nil || len(cookie.Value) != 43 {
		return false
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, found := s.sessions[digest]
	if found && !time.Now().Before(expires) {
		delete(s.sessions, digest)
		return false
	}
	return found
}

// status 为 r 返回是否已登录；w 不返回 token、Cookie 值或财务信息。
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	headers(w)
	w.Header().Set("Content-Type", "application/json")
	result := struct {
		Authenticated bool   `json:"authenticated"`
		LoginURL      string `json:"login_url,omitempty"`
	}{Authenticated: s.valid(r)}
	if s.sso != nil {
		result.LoginURL = "/web/sso"
	}
	_ = json.NewEncoder(w).Encode(result)
}

// login 校验 r 的同源 JSON 密钥并创建有界随机会话；w 仅通过 Secure/HttpOnly Cookie 交付凭据。
// 无效输入不区分不存在用户等细节，密钥既不回显也不写入持久存储。
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	headers(w)
	if !sameOrigin(r) {
		http.Error(w, "same-origin HTTPS request required", 403)
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid login request", 400)
		return
	}
	expected, supplied := sha256.Sum256([]byte(s.token)), sha256.Sum256([]byte(input.Token))
	if subtle.ConstantTimeCompare(expected[:], supplied[:]) != 1 {
		http.Error(w, "authentication required", 401)
		return
	}
	if !s.issue(w) {
		http.Error(w, "session capacity unavailable", 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// issue 向 w 写入新会话 Cookie；随机源或并发会话容量不可用时返回 false，不撤销其他有效会话。
func (s *Server) issue(w http.ResponseWriter) bool {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return false
	}
	value := base64.RawURLEncoding.EncodeToString(random[:])
	expires := time.Now().Add(sessionLifetime)
	s.mu.Lock()
	for key, expiry := range s.sessions {
		if !time.Now().Before(expiry) {
			delete(s.sessions, key)
		}
	}
	if len(s.sessions) >= 256 {
		s.mu.Unlock()
		return false
	}
	s.sessions[sha256.Sum256([]byte(value))] = expires
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds()), Expires: expires})
	return true
}

// logout 对 r 当前会话执行服务端撤销并清除 w 中的 Cookie；要求同源 HTTPS，重复退出仍成功。
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	headers(w)
	if !sameOrigin(r) {
		http.Error(w, "same-origin HTTPS request required", 403)
		return
	}
	if cookie, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, sha256.Sum256([]byte(cookie.Value)))
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	w.WriteHeader(http.StatusNoContent)
}

// WithSession 为 next 适配已验证网页会话，只应挂载到 /api/；MCP 不接受浏览器 Cookie。
// r 若已带 Authorization 则保留显式凭据；Cookie 写请求必须同源，避免跨站请求伪造。
func (s *Server) WithSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && s.valid(r) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
				headers(w)
				http.Error(w, "same-origin HTTPS request required", 403)
				return
			}
			// 凭据只在服务端当前请求副本中适配；不注入 HTML、JS、响应头或客户端存储。
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+s.token)
		}
		next.ServeHTTP(w, r)
	})
}
