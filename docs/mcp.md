# MCP与权限

Streamable HTTP入口/mcp，发现、资源、工具均要求Bearer。HTTPS及外部客户端授权由部署者配置，令牌不放URL/公开配置；网页Cookie不认证MCP。

query_readonly_sql支持单条受AST检查的SELECT、CTE、子查询、连接/聚合/窗口，仅访问公开pam表及允许的函数/类型/运算符。禁止写CTE、DDL、SELECT INTO、锁、系统目录及文件/网络等副作用。import_transactions和manage_imports通过共享应用服务提供受限导入/预检/确认/待核，没有任意SQL写接口。

先读pam://schema或工具发现中的同一份模式。使用$1参数，金额优先用十进制字符串。输出columns/rows/row_count/truncated；截断结果不是完整总数，应聚合或缩小范围。提供的部署限制为3秒、500行、256KiB响应和32KiB SQL，以运行配置为准。

独立pam_reader只读事务与AST双重限制查询，另一个连接写审计。先记录running，再提交success/rejected/error；审计存储失败不交付结果。保存原始SQL/参数并独立归一化模式；AST指纹16位，拒绝模式64位，查询中心兼容两者。日志不保存原文/绑定值，审计和日志都不保存数据库错误正文。详见[audit.md](audit.md)。

交易付款证据不是商品凭据，相似记录和账单完整性需要明确确认。部署者的隧道ID、客户端登记、身份服务与认证材料由私有运维管理；安装配置或健康检查不能替代实际客户端调用验收，不关闭认证来实现接入。
