// Package query 提供独立于 REST/MCP 的只读 SQL 策略、执行限制与隐私审计。
package query

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Error 表示可以安全公开给客户端的错误；Code 不包含数据库返回的敏感字面值。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error 返回稳定的 ASCII 提示，不拼接原 SQL、参数或数据库错误详情。
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// policyError 根据 code 创建通用安全错误；返回值不会包含调用方输入。
func policyError(code string) *Error {
	return &Error{Code: code, Message: "Query rejected by read-only SQL policy"}
}

// set 将空格分隔的固定标识符生成集合；返回集合仅在包初始化时创建。
func set(words string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(words) {
		out[w] = true
	}
	return out
}

var tables = set("transactions assets import_batches import_records import_proposals import_reviews transaction_revisions query_audit query_patterns")
var functions = set(`count sum avg min max bool_and bool_or every array_agg string_agg json_agg jsonb_agg json_object_agg jsonb_object_agg
	abs ceil ceiling floor round trunc mod power sqrt sign greatest least coalesce nullif
	lower upper initcap length char_length character_length octet_length trim btrim ltrim rtrim substring substr left right position strpos replace translate concat concat_ws split_part regexp_replace regexp_match regexp_matches
	date_trunc date_part extract age to_char to_date to_timestamp make_date make_timestamp make_timestamptz timezone now transaction_timestamp statement_timestamp clock_timestamp
	json_build_object jsonb_build_object json_build_array jsonb_build_array json_array_length jsonb_array_length json_extract_path jsonb_extract_path json_extract_path_text jsonb_extract_path_text json_typeof jsonb_typeof jsonb_pretty jsonb_strip_nulls jsonb_array_elements jsonb_array_elements_text json_each jsonb_each json_each_text jsonb_each_text json_object_keys jsonb_object_keys
	array_length array_lower array_upper array_position array_positions array_to_string cardinality unnest generate_series
	row_number rank dense_rank percent_rank cume_dist ntile lag lead first_value last_value nth_value
	percentile_cont percentile_disc mode stddev stddev_pop stddev_samp variance var_pop var_samp
	encode decode md5`)
var types = set("int2 int4 int8 smallint integer bigint numeric decimal text varchar bpchar char boolean bool date timestamp timestamptz time timetz interval uuid json jsonb bytea float4 float8 real double precision")
var operators = set("+ - * / % ^ = <> != < <= > >= || ~~ !~~ ~~* !~~* ~ !~ ~* !~* -> ->> #> #>> @> <@ ? ?| ?& && & | # << >>")
var nodes = set(`SelectStmt ResTarget ColumnRef String Integer Float Boolean A_Const A_Expr BoolExpr NullTest BooleanTest FuncCall TypeCast TypeName ParamRef RangeVar RangeSubselect RangeFunction JoinExpr Alias SortBy WindowDef GroupingSet CaseExpr CaseWhen CoalesceExpr MinMaxExpr SQLValueFunction SubLink RowExpr ArrayExpr A_ArrayExpr A_Indirection A_Indices WithClause CommonTableExpr NamedArgExpr List IntList OidList BitString A_Star`)

// validate 使用 PostgreSQL 原生语法树检查 sql；只读 SELECT 的表达式、连接、CTE 和窗口均可组合。
// maxBytes 限制解析输入字节数；返回的错误只表示稳定拒绝原因。
func validate(sql string, maxBytes int) error {
	if len(sql) == 0 || len(sql) > maxBytes {
		return policyError("sql_size")
	}
	parsed, err := pgquery.ParseToJSON(sql)
	if err != nil {
		return policyError("sql_syntax")
	}
	var tree map[string]any
	if json.Unmarshal([]byte(parsed), &tree) != nil {
		return policyError("sql_syntax")
	}
	stmts, _ := tree["stmts"].([]any)
	if len(stmts) != 1 {
		return policyError("statement_count")
	}
	stmt, _ := stmts[0].(map[string]any)["stmt"].(map[string]any)
	if _, ok := stmt["SelectStmt"]; !ok {
		return policyError("statement_kind")
	}
	count := 0
	return inspect(tree, map[string]bool{}, 0, &count)
}

// inspectSelect 为 stmt 创建词法 CTE 作用域；outer 是外层可见名称，depth/count 维护统一复杂度限制。
// 非递归 WITH 的每个子查询只能看到此前 CTE；递归 WITH 才允许所有同级 CTE 和自引用。
// 否则内层或后续 CTE 名可能意外授权外层同名的 pg_catalog 表。
func inspectSelect(stmt map[string]any, outer map[string]bool, depth int, count *int) error {
	scope := map[string]bool{}
	for name := range outer {
		scope[name] = true
	}
	with, _ := stmt["withClause"].(map[string]any)
	ctes, _ := with["ctes"].([]any)
	recursive, _ := with["recursive"].(bool)
	if recursive {
		for _, item := range ctes {
			node, _ := item.(map[string]any)
			cte, _ := node["CommonTableExpr"].(map[string]any)
			name, _ := cte["ctename"].(string)
			scope[name] = true
		}
	}
	for _, item := range ctes {
		node, _ := item.(map[string]any)
		cte, ok := node["CommonTableExpr"].(map[string]any)
		if !ok {
			return policyError("sql_node")
		}
		if err := inspect(cte, scope, depth+1, count); err != nil {
			return err
		}
		name, _ := cte["ctename"].(string)
		scope[name] = true
	}
	// WITH 的其他字段仍进入通用检查，避免新版本解析器字段绕过默认拒绝策略。
	for name, value := range with {
		if name != "ctes" {
			if err := inspect(value, scope, depth+1, count); err != nil {
				return err
			}
		}
	}
	for name, value := range stmt {
		if name != "withClause" {
			if err := inspect(value, scope, depth+1, count); err != nil {
				return err
			}
		}
	}
	return nil
}

// inspect 遍历 tree，限制嵌套 depth 和节点 count；ctes 只允许额外的本查询虚拟关系名。
// 返回首个策略错误；对所有嵌套子查询和 CTE 使用同一检查，禁止藏在 WITH 中的写入。
func inspect(tree any, ctes map[string]bool, depth int, count *int) error {
	*count++
	if depth > 128 || *count > 8192 {
		return policyError("sql_complexity")
	}
	switch v := tree.(type) {
	case map[string]any:
		for name, x := range v {
			if len(name) > 0 && unicode.IsUpper(rune(name[0])) && !nodes[name] {
				return policyError("sql_node")
			}
			m, _ := x.(map[string]any)
			switch name {
			case "SelectStmt":
				if m["intoClause"] != nil || m["lockingClause"] != nil {
					return policyError("select_side_effect")
				}
				if err := inspectSelect(m, ctes, depth+1, count); err != nil {
					return err
				}
				continue
			case "RangeVar":
				schema, _ := m["schemaname"].(string)
				catalog, _ := m["catalogname"].(string)
				table, _ := m["relname"].(string)
				if catalog != "" || (schema != "" && schema != "pam") || (!tables[table] && !(schema == "" && ctes[table])) {
					return policyError("relation_access")
				}
			case "FuncCall":
				if !allowedName(m["funcname"], functions) {
					return policyError("function_access")
				}
			case "TypeName", "typeName":
				// TypeCast 内嵌的 TypeName 在 parser JSON 中用字段名 typeName，
				// 并非独立 Node 包装；两种形状都必须检查，防止隐式调用危险类型输入函数。
				if !allowedName(m["names"], types) {
					return policyError("type_access")
				}
			case "A_Expr":
				if m["name"] != nil && !allowedName(m["name"], operators) {
					return policyError("operator_access")
				}
			case "ColumnRef":
				parts, _ := m["fields"].([]any)
				if len(parts) > 3 {
					return policyError("column_access")
				}
				if len(parts) == 3 {
					s := stringNode(parts[0])
					if s != "pam" {
						return policyError("column_access")
					}
				}
			}
			// 字面量的内容不含可执行语法，继续遍历其内部不会改变允许集。
			if err := inspect(x, ctes, depth+1, count); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range v {
			if err := inspect(x, ctes, depth+1, count); err != nil {
				return err
			}
		}
	}
	return nil
}

// stringNode 从 PostgreSQL String 节点读取标识符；node 形状不合法时返回空串并由调用方拒绝。
func stringNode(node any) string {
	m, _ := node.(map[string]any)
	s, _ := m["String"].(map[string]any)
	v, _ := s["sval"].(string)
	return v
}

// allowedName 验证 value 中的一段或 pg_catalog 限定名称是否属于 allowed；禁止任意 schema 重载。
func allowedName(value any, allowed map[string]bool) bool {
	parts, ok := value.([]any)
	if !ok || len(parts) < 1 || len(parts) > 2 {
		return false
	}
	if len(parts) == 2 && stringNode(parts[0]) != "pg_catalog" {
		return false
	}
	return allowed[stringNode(parts[len(parts)-1])]
}

// normalized 将 SQL 变为不含字面值、注释及自由标识符的审计模式。
// maxBytes 限制输入；无法安全解析的 SQL 使用固定占位符，绝不落盘原始错误或输入。
func normalized(sql string, maxBytes int) (string, string) {
	placeholder := "<unparseable-or-rejected>"
	// 非 SELECT、复杂度超限及策略拒绝使用稳定原因分组；只把受限 SELECT 树交给 deparser。
	if err := validate(sql, maxBytes); err != nil {
		placeholder = "<rejected:" + errorCode(err) + ">"
		return hash(placeholder), placeholder
	}
	tree, err := pgquery.Parse(sql)
	if err != nil || len(tree.Stmts) != 1 || tree.Stmts[0].Stmt.GetSelectStmt() == nil {
		return hash(placeholder), placeholder
	}
	// 所有常量都变成占位参数；未知别名/列名同样可能藏有私密信息，必须规范化。
	known := set(`pam transactions assets import_batches import_records query_audit query_patterns
	 id occurred_at direction amount currency merchant description category source external_id import_batch_id original created_at updated_at
	 name brand model serial_number purchase_date purchase_price related_transaction_id notes status
	 fingerprint normalized_sql executed_at duration_ms row_count error_code calls average_duration_ms max_duration_ms first_seen last_seen failures recent_calls
	 idempotency_key source_file_hash payload_hash batch_id row_index row_key original normalized_submission transaction_id status reason result`)
	for k := range functions {
		known[k] = true
	}
	for k := range types {
		known[k] = true
	}
	for k := range operators {
		known[k] = true
	}
	known["pg_catalog"] = true
	aliases := map[string]string{}
	next := int32(1)
	redact(tree.ProtoReflect(), known, aliases, &next)
	text, err := pgquery.Deparse(tree)
	if err != nil {
		return hash(placeholder), placeholder
	}
	fp, err := pgquery.Fingerprint(text)
	if err != nil {
		fp = hash(text)
	}
	return fp, text
}

// redact 原地清理 msg 的常量和未知标识符；known 是公开 schema 名，aliases 统一别名，next 分配占位编号。
// 仅处理已经由 PostgreSQL 解析产生的树，不将用户 JSON 交给原生 deparser。
func redact(msg protoreflect.Message, known map[string]bool, aliases map[string]string, next *int32) {
	if n, ok := msg.Interface().(*pgquery.Node); ok {
		if n.GetAConst() != nil {
			n.Node = &pgquery.Node_ParamRef{ParamRef: &pgquery.ParamRef{Number: *next, Location: -1}}
			*next++
			return
		}
		if p := n.GetParamRef(); p != nil {
			p.Number = *next
			*next++
			return
		}
	}
	// Descriptor 顺序确定，确保字面值相同结构得到稳定占位编号，而不依赖 map 遍历顺序。
	fields := msg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !msg.Has(fd) {
			continue
		}
		v := msg.Get(fd)
		if fd.IsList() {
			list := v.List()
			if fd.Kind() == protoreflect.MessageKind {
				for j := 0; j < list.Len(); j++ {
					redact(list.Get(j).Message(), known, aliases, next)
				}
			}
			continue
		}
		if fd.Kind() == protoreflect.MessageKind {
			redact(v.Message(), known, aliases, next)
			continue
		}
		if fd.Kind() == protoreflect.StringKind {
			value := v.String()
			if value != "" && !known[value] {
				replacement, ok := aliases[value]
				if !ok {
					replacement = fmt.Sprintf("identifier_%d", len(aliases)+1)
					aliases[value] = replacement
				}
				msg.Set(fd, protoreflect.ValueOfString(replacement))
			}
		}
	}
}

// hash 对固定占位符或已清理文本生成稳定指纹；value 不得是原始 SQL 或参数。
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
