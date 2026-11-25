// PAM 提供独立迁移、REST/MCP 服务和无凭据健康检查入口。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mental1104/pam/internal/app"
	"github.com/mental1104/pam/internal/httpapi"
	"github.com/mental1104/pam/internal/mcpserver"
	"github.com/mental1104/pam/internal/query"
	"github.com/mental1104/pam/internal/webui"
	"github.com/mental1104/pam/migrations"
)

// main 安装结构化日志与终止信号；只公开安全错误，不把连接字符串或 SQL 输出到日志。
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("pam stopped", "reason", err.Error())
		os.Exit(1)
	}
}

// open 根据环境键 key 建立最多四连接的池；ctx 控制连接就绪期限，失败返回不含 DSN 的错误。
func open(ctx context.Context, key string) (*pgxpool.Pool, error) {
	value := os.Getenv(key)
	if value == "" {
		return nil, fmt.Errorf("missing %s", key)
	}
	config, err := pgxpool.ParseConfig(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", key)
	}
	config.MaxConns = 4
	config.MinConns = 0
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("database pool unavailable")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database connection unavailable")
	}
	return pool, nil
}

// run 根据 args 选择 migrate、healthcheck 或默认 serve；ctx 结束后限时关闭服务器和连接池。
// 迁移进程仅使用 owner 连接，常驻服务仅持有业务和只读凭据，失败不继续监听。
func run(ctx context.Context, args []string) error {
	mode := "serve"
	if len(args) > 0 {
		mode = args[0]
	}
	if len(args) > 1 {
		return fmt.Errorf("expected serve, migrate or healthcheck")
	}
	if mode == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		r, err := client.Get("http://127.0.0.1:8080/health/ready")
		if err != nil {
			return fmt.Errorf("healthcheck unavailable")
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("healthcheck not ready")
		}
		return nil
	}
	if mode != "serve" && mode != "migrate" {
		return fmt.Errorf("expected serve, migrate or healthcheck")
	}
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	writer, err := open(connectCtx, "PAM_DATABASE_URL")
	if err != nil {
		return err
	}
	defer writer.Close()
	if mode == "migrate" {
		if err := migrations.Apply(ctx, writer); err != nil {
			return fmt.Errorf("migration failed")
		}
		slog.Info("migrations applied")
		return nil
	}
	token := os.Getenv("PAM_ACCESS_TOKEN")
	if len(token) < 32 {
		return fmt.Errorf("PAM_ACCESS_TOKEN must contain at least 32 characters")
	}
	reader, err := open(connectCtx, "PAM_READER_DATABASE_URL")
	if err != nil {
		return err
	}
	defer reader.Close()
	// 为 JSON-RPC 包装、受限请求 id 和简短文本摘要预留 4KiB，整个查询响应保持在 256KiB 内。
	queries := query.New(reader, writer, query.Limits{Timeout: 3 * time.Second, MaxRows: 500, MaxResponseBytes: (256 << 10) - 4096, MaxSQLBytes: 32 << 10})
	if err := queries.Check(connectCtx); err != nil {
		return err
	}
	service := app.New(writer)
	// 就绪检查仅 Ping 两个实际连接池；不把健康探针本身写入用户 SQL 审计。
	ready := func(ctx context.Context) error {
		if err := writer.Ping(ctx); err != nil {
			return err
		}
		return reader.Ping(ctx)
	}
	browser := webui.New(token)
	// NAS 单点登录只验证固定代理的签名断言；未配置时保留单用户访问密钥登录。
	if os.Getenv("PAM_WEB_SSO_JWKS_URL") != "" {
		if err := browser.ConfigureSSO(webui.SSOConfig{JWKSURL: os.Getenv("PAM_WEB_SSO_JWKS_URL"), Issuer: os.Getenv("PAM_WEB_SSO_ISSUER"), Audience: os.Getenv("PAM_WEB_SSO_AUDIENCE"), Username: os.Getenv("PAM_WEB_SSO_USERNAME")}); err != nil {
			return fmt.Errorf("invalid web SSO configuration")
		}
	}
	handler := httpapi.New(service, mcpserver.New(service, queries), token, ready, browser)
	address := os.Getenv("PAM_ADDR")
	if address == "" {
		address = ":8080"
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	stopped := make(chan error, 1)
	// 服务器 goroutine 只提交一次结果，主路径统一拥有关闭和连接池释放责任。
	go func() { stopped <- server.ListenAndServe() }()
	slog.Info("pam listening", "address", address)
	select {
	case err := <-stopped:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP server failed")
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("HTTP shutdown timeout")
		}
		return nil
	}
}
