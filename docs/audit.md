# SQL审计中心

读取经PAM的MCP只读SQL调用，不是PostgreSQL全部内部语句。GET /api/v1/audit复用现有认证，查看/刷新不产生自引用审计，不执行用户提交的SQL。

提供总数/成功率/执行错误/P95、堆叠趋势、状态/耗时分布、错误原因、常见模式、分页与详情。状态、时间、原因及模式可下钻；支持日期、SQL原文/模式搜索、状态、慢查询和耗时排序，兼容桌面/手机。

query_audit另存raw_sql、parameters JSONB及raw_truncated。成功/错误/拒绝均保留原文，无参数保存[]；旧记录保持NULL，界面明确不能还原。SQL/参数各128KiB上限，超限标记不完整，SQL在有效UTF-8边界截断，超限参数整体不保存。parameters_json以JSON文本字符串传输/展示，避免浏览器Number改变大数。证据通过textContent展示，复制不执行查询。

所有统计/明细来自单个repeatable-read只读快照。API默认全部历史，网页默认七天；from包含、to排除，按北京时间展示，空时段补零，最多约36个整小时桶。成功率分母success+rejected+error，排除running。耗时分布/均值/P95/最长只用success/error；1秒为慢阈值，总耗时含服务与审计开销，不等于数据库计划耗时。返回行数不是扫描行数，running不能证明仍在运行。

参数：from/to（RFC3339）、status、error_code、fingerprint（16/64位）、source、search、min_duration_ms、sort=recent|slow、limit（1–100，默认20）、offset。拒绝未知/重复值，5秒预算，取消传到数据库。应用校验/固定参数化读取位于internal/app和internal/store，MCP查询同一表/视图。

原文、绑定值、审计截图和运行统计属于私有数据，不放Git/公开CI工件；数据库错误正文不保存，仅解释稳定错误码。
