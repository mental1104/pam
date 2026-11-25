# PAM 开发约定

- Go模块化单体、PostgreSQL、REST/MCP、同源网页与原生SwiftUI；应用服务位于internal/app和internal/query，协议层只适配传输。
- 金额用JSON十进制字符串和数据库NUMERIC，禁止二进制浮点财务计算。
- SQL使用PostgreSQL AST校验；审计另存原始SQL与绑定参数，归一化模式用于聚合，历史原文不可还原。日志不记录金融正文、凭据或数据库错误正文。
- 新代码提供中文优先的职责、参数、返回值及关键路径注释。
- 验证包括Go测试、vet、race、隔离数据库和独立HTTP/MCP；iOS构建需要macOS/Xcode。
- 只提交代码、通用配置和合成夹具。真实账单、OCR/语料、统计回执、个人部署配置及运维历史留在仓库外。
- Git作者使用公开账号及noreply邮箱。现有CI保持非草稿PR触发，不增加push或定时任务。
