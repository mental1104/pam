# 验证与证据边界

公开交易、账号、商户、金额、OCR文本/坐标和图片样例全部为人工构造。真实语料、识别报告、来源统计和生产截图/回执在部署者私有存储，不上传Git、Issue、PR、CI日志或工件。

- Go：go test ./...、go vet ./...、CGO环境go test -race ./...。
- 数据库：隔离PostgreSQL，真实migrator/app/reader权限，迁移、幂等、待核、更正、只读限制和审计。使用PAM_TEST_DATABASE_URL、PAM_TEST_WRITER_URL、PAM_TEST_READER_URL，缺失时明确跳过。
- 独立HTTP/MCP：PAM_ACCEPTANCE_URL与PAM_ACCESS_TOKEN，客户端只走公开协议，不链接内部服务；导入会留存，只指向合成隔离库。
- 浏览器：Node.js、Playwright/Chromium、openssl；设置PAM_BROWSER_BACKEND_URL、PAM_BROWSER_TEST_TOKEN、PAM_BROWSER_OUTPUT_DIR。review.cjs/audit.cjs经本机TLS代理访问隔离服务；audit前用tests/fixtures/audit创建合成记录。生产浏览器程序由私有运维项目管理。
- Swift：ios下swift test，Apple环境实际执行合成Vision烟测；macOS/Xcode无签名模拟器构建不代表界面或真机验收。
- 真实账单：可选私有corpus回归，缺失时明确跳过。不把合成结果当作真实金额/方向/时间/商户或漏行准确率。

Actions仅在非草稿PR运行，具体版本结果以Checks为准，没有生产部署或生产凭据。未执行的真实截图、界面操作和真机项目不能报告完成。
