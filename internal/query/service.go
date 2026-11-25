package query

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Request 是任意只读 SQL 的调用边界；Parameters 只接受字符串、布尔、整数、json.Number 或 null。
// 金额参数应使用十进制字符串，避免 JSON 客户端先行转成二进制浮点。
type Request struct {
	SQL        string `json:"sql"`
	Parameters []any  `json:"parameters,omitempty"`
}

// Result 返回文本格式数据库值；numeric/float 保留十进制字符串，整数使用 json.Number。
// RowCount 是实际返回行数；Truncated 表示还存在因行数或字节上限省略的行。
type Result struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
}

// Limits 约束每次调用的时间、行数、JSON 响应字节数和输入 SQL 字节数；零值使用保守默认值。
type Limits struct {
	Timeout          time.Duration
	MaxRows          int
	MaxResponseBytes int
	MaxSQLBytes      int
}

// Service 同时持有只读执行池与独立审计写入池；两者由调用方关闭，可并发调用 Execute。
type Service struct {
	reader, writer *pgxpool.Pool
	limits         Limits
}

// New 创建应用服务；reader 必须是独立低权限账号，writer 必须能写 pam.query_audit。
// limits 的非正值使用默认值；返回值不接管连接池生命周期。
func New(reader, writer *pgxpool.Pool, limits Limits) *Service {
	if limits.Timeout <= 0 {
		limits.Timeout = 5 * time.Second
	}
	if limits.MaxRows <= 0 {
		limits.MaxRows = 500
	}
	if limits.MaxResponseBytes <= 0 {
		limits.MaxResponseBytes = 512 * 1024
	}
	if limits.MaxSQLBytes <= 0 {
		limits.MaxSQLBytes = 16 * 1024
	}
	return &Service{reader: reader, writer: writer, limits: limits}
}

// Check 在启动时验证 reader 的权限边界以及独立 writer 身份；ctx 取消将中止检查。
// 返回安全配置错误，不公开数据库账号、密码或原始错误。
func (s *Service) Check(ctx context.Context) error {
	var readerName, writerName string
	var unsafe bool
	err := s.reader.QueryRow(ctx, `SELECT current_user, rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR has_schema_privilege(current_user,'pam','CREATE') OR has_schema_privilege(current_user,'public','CREATE') OR has_database_privilege(current_user,current_database(),'CREATE') FROM pg_roles WHERE rolname=current_user`).Scan(&readerName, &unsafe)
	if err != nil || unsafe {
		return &Error{Code: "reader_configuration", Message: "Read-only database role is not sufficiently restricted"}
	}
	if err = s.writer.QueryRow(ctx, `SELECT current_user`).Scan(&writerName); err != nil || readerName == writerName {
		return &Error{Code: "reader_configuration", Message: "Separate query and audit database roles are required"}
	}
	for table := range tables {
		var writable bool
		err = s.reader.QueryRow(ctx, `SELECT has_table_privilege(current_user,$1,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`, "pam."+table).Scan(&writable)
		if err != nil || writable {
			return &Error{Code: "reader_configuration", Message: "Read-only role has unexpected table permissions"}
		}
	}
	return nil
}

// Execute 校验 req、先登记审计，再以只读事务运行查询，最后独立提交审计终态。
// ctx 限制排队与查询；审计使用独立短超时，客户端断开也不会遗漏终态。审计失败时不交付数据。
func (s *Service) Execute(ctx context.Context, req Request) (result Result, err error) {
	started := time.Now()
	fingerprint, pattern := normalized(req.SQL, s.limits.MaxSQLBytes)
	validation := validate(req.SQL, s.limits.MaxSQLBytes)
	parameters, paramErr := safeParameters(req.Parameters)
	if validation == nil {
		validation = paramErr
	}
	status := "running"
	code := ""
	if validation != nil {
		status = "rejected"
		code = errorCode(validation)
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	var auditID string
	rawSQL, rawParameters, truncated := captureOriginal(req)
	insertErr := s.writer.QueryRow(auditCtx, `INSERT INTO pam.query_audit (fingerprint,normalized_sql,executed_at,status,error_code,source,raw_sql,parameters,raw_truncated) VALUES ($1,$2,$3,$4,NULLIF($5,''),'mcp.query_readonly_sql',$6,$7::jsonb,$8) RETURNING id::text`, fingerprint, pattern, started, status, code, rawSQL, rawParameters, truncated).Scan(&auditID)
	cancel()
	if insertErr != nil {
		return Result{}, &Error{Code: "audit_unavailable", Message: "Query audit storage is unavailable"}
	}
	// 在运行前落盘 running，进程意外退出仍可观察到已受理的 SQL 调用。
	defer func() {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer finishCancel()
		if validation != nil {
			status = "rejected"
		} else if err != nil {
			status = "error"
		} else {
			status = "success"
		}
		_, auditErr := s.writer.Exec(finishCtx, `UPDATE pam.query_audit SET duration_ms=$2,row_count=$3,status=$4,error_code=NULLIF($5,'') WHERE id=$1::uuid`, auditID, time.Since(started).Milliseconds(), result.RowCount, status, errorCode(err))
		if auditErr != nil {
			result = Result{}
			err = &Error{Code: "audit_unavailable", Message: "Query audit completion could not be persisted"}
		}
	}()
	if validation != nil {
		return Result{}, validation
	}
	queryCtx, queryCancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer queryCancel()
	result, err = s.run(queryCtx, req.SQL, parameters)
	if err != nil {
		return Result{}, publicDatabaseError(err)
	}
	return result, nil
}

// captureOriginal 按所有者明确授权保存 req 原始 SQL 和参数；模式归一化仍独立用于聚类。
// 每类原文最多 128 KiB，覆盖正常查询限制；超限或无法编码时明确标记，不写无效 JSON 或日志正文。
func captureOriginal(req Request) (string, *string, bool) {
	const budget = 128 * 1024
	raw := req.SQL
	truncated := len(raw) > budget
	if truncated {
		raw = raw[:budget]
		for !utf8.ValidString(raw) && len(raw) > 0 {
			raw = raw[:len(raw)-1]
		}
	}
	values := req.Parameters
	if values == nil {
		values = []any{}
	}
	body, err := json.Marshal(values)
	if err != nil || len(body) > budget {
		return raw, nil, true
	}
	encoded := string(body)
	return raw, &encoded, truncated
}

// run 在 ctx 生命周期内执行已校验 sql；parameters 已规范化，只读事务始终回滚释放。
// 返回受行数和字节数限制的结果；所有路径都关闭 rows，错误由上层转换为安全提示。
func (s *Service) run(ctx context.Context, sql string, parameters []any) (Result, error) {
	result := Result{Columns: []string{}, Rows: [][]any{}}
	tx, err := s.reader.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	// 查询取消后仍尝试回滚，但清理不能因网络故障无限阻塞调用返回。
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cleanupCancel()
		_ = tx.Rollback(cleanupCtx)
	}()
	// search_path 优先 pg_catalog，函数与运算符不会由业务 schema 意外覆盖。
	_, err = tx.Exec(ctx, fmt.Sprintf(`SET LOCAL search_path=pg_catalog,pam; SET LOCAL statement_timeout='%d'; SET LOCAL lock_timeout='%d'; SET LOCAL idle_in_transaction_session_timeout='%d'`, s.limits.Timeout.Milliseconds(), s.limits.Timeout.Milliseconds(), s.limits.Timeout.Milliseconds()))
	if err != nil {
		return result, err
	}
	// pgx 的 SimpleProtocol 使用其 SQL 字面值编码器安全替换参数，使十进制字符串
	// 在 PostgreSQL 中按表达式上下文推断类型；应用不拼接参数或自行实现转义。
	args := append([]any{pgx.QueryExecModeSimpleProtocol}, parameters...)
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	for _, field := range fields {
		result.Columns = append(result.Columns, field.Name)
	}
	envelope, _ := json.Marshal(result)
	used := len(envelope) + 32
	if used > s.limits.MaxResponseBytes {
		return Result{}, &Error{Code: "response_columns_too_large", Message: "Query column metadata exceeds response limit"}
	}
	for rows.Next() {
		if len(result.Rows) >= s.limits.MaxRows {
			result.Truncated = true
			break
		}
		raw := rows.RawValues()
		row := make([]any, len(raw))
		for i, value := range raw {
			if value == nil {
				continue
			}
			row[i] = decodeValue(fields[i].DataTypeOID, value)
		}
		encoded, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			return Result{}, marshalErr
		}
		if used+len(encoded)+1 > s.limits.MaxResponseBytes {
			result.Truncated = true
			break
		}
		used += len(encoded) + 1
		result.Rows = append(result.Rows, row)
	}
	if rows.Err() != nil {
		return Result{}, rows.Err()
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

// decodeValue 将 oid 对应的 PostgreSQL 文本 value 转成精度不丢失的 JSON 值；不修改借用的原字节。
func decodeValue(oid uint32, value []byte) any {
	switch oid {
	case 16:
		return bytes.Equal(value, []byte("t"))
	case 20, 21, 23, 26:
		return json.Number(string(value))
	case 114, 3802:
		var parsed any
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if decoder.Decode(&parsed) == nil {
			return parsed
		}
	}
	return string(value)
}

// safeParameters 将 values 转为 pgx 可绑定的标量；浮点与对象拒绝，避免隐含精度损失或任意编码器行为。
// 返回新切片，不改变调用方参数；最多允许 128 个且字符串总长不超过 64KiB。
func safeParameters(values []any) ([]any, error) {
	if len(values) > 128 {
		return nil, policyError("parameter_count")
	}
	out := make([]any, len(values))
	size := 0
	for i, value := range values {
		switch v := value.(type) {
		case nil, bool, int, int16, int32, int64, uint16, uint32:
			out[i] = v
		case string:
			size += len(v)
			out[i] = v
		case json.Number:
			// JSON Number 不转 float64；由 PostgreSQL 按查询上下文解析十进制文本。
			if !json.Valid([]byte(v.String())) {
				return nil, policyError("parameter_type")
			}
			var check any
			dec := json.NewDecoder(bytes.NewBufferString(v.String()))
			dec.UseNumber()
			if dec.Decode(&check) != nil {
				return nil, policyError("parameter_type")
			}
			if _, ok := check.(json.Number); !ok {
				return nil, policyError("parameter_type")
			}
			out[i] = v.String()
			size += len(v.String())
		default:
			return nil, policyError("parameter_type")
		}
		if size > 64*1024 {
			return nil, policyError("parameter_size")
		}
	}
	return out, nil
}

// errorCode 提取 err 的稳定策略码或 SQLSTATE；返回空串表示成功，不保存数据库原始错误消息。
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	var own *Error
	if errors.As(err, &own) {
		return own.Code
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "57014"
	}
	return "query_failed"
}

// publicDatabaseError 将 err 转成可公开的 SQLSTATE 提示；不回传可能包含字面值的 detail/hint/message。
func publicDatabaseError(err error) error {
	var own *Error
	if errors.As(err, &own) {
		return own
	}
	return &Error{Code: errorCode(err), Message: fmt.Sprintf("Read-only query failed (%s)", errorCode(err))}
}
