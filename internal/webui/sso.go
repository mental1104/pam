package webui

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const ssoJWKSPath = "/.well-known/pomerium/jwks.json"
const ssoJWKSMaxBytes = 64 << 10
const ssoJWKSCacheLifetime = time.Minute

// SSOConfig 明确浏览器 SSO 的信任边界；由部署配置提供，绝不从请求或 JWT 内的 URL 推导。
// JWKSURL 是固定的 Pomerium 公钥端点，内部 HTTP 必须位于受控容器网络；Issuer、Audience
// 是 Pomerium 路由断言的精确值，默认都为路由主机名；Username 是唯一允许换取会话的用户名。
type SSOConfig struct {
	JWKSURL  string
	Issuer   string
	Audience string
	Username string
}

// ssoPublicKey 保存一把已检查类型与强度的非对称公钥；algorithm 防止同一个 kid 被跨算法使用。
type ssoPublicKey struct {
	algorithm string
	key       crypto.PublicKey
}

// ssoVerifier 在进程内校验 Pomerium 断言，不调用身份提供方，也不改变 Bearer REST/MCP 认证。
// config 与 client 配置后不再变化；mu 保护公钥缓存、到期时间和短期失败状态，可并发验签。
type ssoVerifier struct {
	config     SSOConfig
	client     *http.Client
	mu         sync.Mutex
	keys       map[string]ssoPublicKey
	expires    time.Time
	retryAfter time.Time
}

// ssoClaims 只接收 Pomerium 签名载荷中的用户名及标准断言声明；不信任未签名的身份请求头。
// v0.33.3 将 jwt_claims_headers 的来源 claim（如 username）作为字符串加入签名载荷。
type ssoClaims struct {
	jwt.RegisteredClaims
	Username string `json:"username"`
}

// ssoJWKSet 是远端 JWKS 的有界解码模型；公钥最多 16 把，其余字段由单键验证器严格检查。
type ssoJWKSet struct {
	Keys []ssoJWK `json:"keys"`
}

// ssoJWK 仅声明本服务支持的 ES256/RS256 验签字段；证书链、私钥和 JWT 指定的远端地址均不使用。
type ssoJWK struct {
	KID string `json:"kid"`
	KTY string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	CRV string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// ConfigureSSO 校验 config 并安装 SSO 校验器；必须在处理请求前调用，失败时保留原有配置。
// 空白信任值、带凭据/查询/片段的 URL 或非标准 JWKS 路径返回通用配置错误，不访问网络。
// 公钥首次登录时惰性加载；HTTP 不读取代理环境变量，也不跟随重定向到其他信任位置。
func (s *Server) ConfigureSSO(config SSOConfig) error {
	endpoint, err := url.Parse(config.JWKSURL)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Opaque != "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Path != ssoJWKSPath ||
		endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return errors.New("invalid SSO configuration")
	}
	for _, value := range []string{config.Issuer, config.Audience, config.Username} {
		if value == "" || len(value) > 512 || strings.ContainsAny(value, " \t\r\n") {
			return errors.New("invalid SSO configuration")
		}
	}
	s.sso = &ssoVerifier{
		config: config,
		client: &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				Proxy:                 nil,
				ResponseHeaderTimeout: 2 * time.Second,
				TLSHandshakeTimeout:   2 * time.Second,
				MaxConnsPerHost:       2,
				MaxIdleConnsPerHost:   1,
				IdleConnTimeout:       30 * time.Second,
			},
			// 取钥地址由部署固定；重定向即失败，不能扩大信任目标。
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return nil
}

// SSOLogin 验证 r 唯一的 X-Pomerium-Jwt-Assertion，并通过 w 发放已有浏览器会话后 303 回首页。
// 仅 GET 可用；未配置返回 503，无效或缺失的签名/声明一律 401，容量不足返回 503；不输出断言内容。
// Pomerium 必须先以配置所有者的专属策略保护该路径；本层仍独立验签，防止绕过代理伪造身份头。
func (s *Server) SSOLogin(w http.ResponseWriter, r *http.Request) {
	headers(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sso == nil {
		http.Error(w, "SSO unavailable", http.StatusServiceUnavailable)
		return
	}
	assertions := r.Header.Values("X-Pomerium-Jwt-Assertion")
	if len(assertions) != 1 || len(assertions[0]) == 0 || len(assertions[0]) > 16<<10 ||
		strings.ContainsAny(assertions[0], " \t\r\n,") || s.sso.verify(r.Context(), assertions[0]) != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !s.issue(w) {
		http.Error(w, "session capacity unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// verify 对 assertion 校验算法、签名、精确 iss/aud、必填 exp、可选 nbf/iat 与唯一用户名。
// ctx 的取消会传播到取钥请求；所有失败均返回错误而不创建会话，调用方不得输出错误中的原始断言。
func (v *ssoVerifier) verify(ctx context.Context, assertion string) error {
	claims := &ssoClaims{}
	_, err := jwt.ParseWithClaims(assertion, claims,
		// 仅从固定 JWKS 按 kid 取公钥；jku/x5u 等令牌头永远不作为网络地址使用。
		func(token *jwt.Token) (any, error) {
			kid, ok := token.Header["kid"].(string)
			if !ok || kid == "" || len(kid) > 128 {
				return nil, errors.New("invalid signing key identifier")
			}
			key, err := v.publicKey(ctx, kid)
			if err != nil || key.algorithm != token.Method.Alg() {
				return nil, errors.New("signing key unavailable")
			}
			return key.key, nil
		}, jwt.WithValidMethods([]string{"ES256", "RS256"}), jwt.WithExpirationRequired(),
		jwt.WithIssuer(v.config.Issuer), jwt.WithAudience(v.config.Audience), jwt.WithIssuedAt())
	if err != nil {
		return errors.New("invalid SSO assertion")
	}
	if claims.Username != v.config.Username {
		return errors.New("unauthorized SSO identity")
	}
	return nil
}

// publicKey 为 kid 返回仍有效的公钥；ctx 取消或远端错误时失败，绝不使用过期缓存。
// 持锁取钥最多两秒，串行合并缓存更新；未知 kid 在缓存期内直接拒绝，避免攻击者驱动无限刷新。
// 失败后等待五秒再重试，限制不可达上游的请求放大；该等待不阻塞请求，仅返回错误。
func (v *ssoVerifier) publicKey(ctx context.Context, kid string) (ssoPublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if !now.Before(v.expires) {
		if now.Before(v.retryAfter) {
			return ssoPublicKey{}, errors.New("signing keys unavailable")
		}
		keys, err := v.fetchKeys(ctx)
		if err != nil {
			// 单个浏览器取消不能把其余用户的登录缓存置为失败；只有上游错误才启用退避。
			if ctx.Err() == nil {
				v.retryAfter = time.Now().Add(5 * time.Second)
			}
			return ssoPublicKey{}, errors.New("signing keys unavailable")
		}
		v.keys, v.expires = keys, time.Now().Add(ssoJWKSCacheLifetime)
		v.retryAfter = time.Time{}
	}
	key, found := v.keys[kid]
	if !found {
		return ssoPublicKey{}, errors.New("unknown signing key")
	}
	return key, nil
}

// fetchKeys 从唯一配置端点读取最多 64 KiB JWKS，返回已验证的公钥集合；ctx 控制网络取消。
// 非 200、重定向、畸形或重复键、空集合均失败；响应体无论成功与否都关闭，不保留过期公钥。
func (v *ssoVerifier) fetchKeys(ctx context.Context) (map[string]ssoPublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.config.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	response, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("unexpected signing key response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, ssoJWKSMaxBytes+1))
	if err != nil || len(body) > ssoJWKSMaxBytes {
		return nil, errors.New("invalid signing key response size")
	}
	var document ssoJWKSet
	if json.Unmarshal(body, &document) != nil || len(document.Keys) == 0 || len(document.Keys) > 16 {
		return nil, errors.New("invalid signing key document")
	}
	keys := make(map[string]ssoPublicKey, len(document.Keys))
	seen := make(map[string]bool, len(document.Keys))
	for _, jwk := range document.Keys {
		if jwk.KID == "" || len(jwk.KID) > 128 || seen[jwk.KID] {
			return nil, errors.New("invalid signing key identifier")
		}
		seen[jwk.KID] = true
		// 不支持的密钥可以与合法签名密钥共存，但绝不交给 JWT 库自动选择算法。
		if (jwk.Alg != "ES256" && jwk.Alg != "RS256") || (jwk.Use != "" && jwk.Use != "sig") {
			continue
		}
		key, err := jwk.publicKey()
		if err != nil {
			return nil, err
		}
		keys[jwk.KID] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("no supported signing keys")
	}
	return keys, nil
}

// publicKey 将 jwk 的规范 Base64URL 公钥字段解码成 ES256 或 RS256 公钥；不读取私钥字段，不接受弱 RSA。
// 返回值仅含验签所需公钥；曲线错误、越界坐标、算法类型不符或 RSA 位数/指数非法时返回错误。
func (jwk ssoJWK) publicKey() (ssoPublicKey, error) {
	invalid := errors.New("invalid signing key")
	switch {
	case jwk.Alg == "ES256" && jwk.KTY == "EC" && jwk.CRV == "P-256":
		x, xErr := base64.RawURLEncoding.Strict().DecodeString(jwk.X)
		y, yErr := base64.RawURLEncoding.Strict().DecodeString(jwk.Y)
		if xErr != nil || yErr != nil || len(x) != 32 || len(y) != 32 {
			return ssoPublicKey{}, invalid
		}
		pointX, pointY := new(big.Int).SetBytes(x), new(big.Int).SetBytes(y)
		if !elliptic.P256().IsOnCurve(pointX, pointY) {
			return ssoPublicKey{}, invalid
		}
		return ssoPublicKey{algorithm: jwk.Alg, key: &ecdsa.PublicKey{Curve: elliptic.P256(), X: pointX, Y: pointY}}, nil
	case jwk.Alg == "RS256" && jwk.KTY == "RSA":
		n, nErr := base64.RawURLEncoding.Strict().DecodeString(jwk.N)
		e, eErr := base64.RawURLEncoding.Strict().DecodeString(jwk.E)
		if nErr != nil || eErr != nil || len(n) < 256 || len(n) > 1024 || len(e) == 0 || len(e) > 4 || n[0] == 0 || e[0] == 0 {
			return ssoPublicKey{}, invalid
		}
		modulus, exponent := new(big.Int).SetBytes(n), new(big.Int).SetBytes(e).Int64()
		if modulus.BitLen() < 2048 || modulus.Bit(0) != 1 || exponent < 3 || exponent > 1<<31-1 || exponent%2 == 0 {
			return ssoPublicKey{}, invalid
		}
		return ssoPublicKey{algorithm: jwk.Alg, key: &rsa.PublicKey{N: modulus, E: int(exponent)}}, nil
	default:
		return ssoPublicKey{}, invalid
	}
}
