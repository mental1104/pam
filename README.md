# Personal Asset Manager

PAM是可自行部署的个人交易与实物资产应用：Go单体、PostgreSQL、同源网页、REST、MCP和原生iOS。截图导入使用Apple Vision本地OCR与确定性Parser，正常导入不依赖LLM。

## 功能

- 精确十进制金额、筛选分页、原始来源及逐行导入回执。
- 幂等导入、增量预检、持久待核和带前后快照/理由/证据的人工更正；相似记录只产生候选。
- 招商银行、支付宝、微信截图Parser；iOS多图、OCR、证据预览、编辑及明确人工确认。
- 实物资产及可选交易关联；受限MCP只读SQL；带图表下钻、耗时/错误原因和原文参数详情的SQL审计中心。

Parser仍是实验功能。合成测试和模拟器编译不证明真实账单准确率或真机行为，见[验收边界](docs/acceptance.md)。

## 启动

需要Docker Compose。从[.env.example](.env.example)创建仓库外的私有配置，填写独立随机URL-safe密码和至少32字符令牌。从项目根目录运行：

```sh
# Dockerfile提供CGO和GCC构建环境。
docker build -f deploy/Dockerfile -t pam:v0 .
# 先初始化数据库、执行匹配版本迁移，再启动应用。
docker compose --env-file /private/pam.env -f deploy/compose.yaml up -d
```

默认http://127.0.0.1:18088，REST位于/api/v1，MCP位于/mcp。网页会话和iOS需要可信HTTPS反代。数据库不发布端口；迁移、业务及只读账号分离。详见[运维说明](docs/operations.md)。

## 验证

```sh
# 数据库集成测试需要隔离DSN；未配置时明确跳过。
go test ./...
go vet ./...
CGO_ENABLED=1 go test -race ./...
# Swift核心测试；Vision和模拟器编译需要Apple环境。
cd ios
swift test
```

Actions仅运行非草稿PR，验证后端/数据库/HTTP/MCP、macOS Swift/Vision合成烟测及无签名iOS模拟器构建，没有自动部署或生产访问。

## 数据边界

公开仓库只包含代码、通用配置和[testdata/synthetic](testdata/synthetic)合成夹具。真实账单、截图、OCR、数据库、备份、审计正文、个人部署和历史运行记录在仓库外维护。现有Go模块及iOS Bundle/Keychain命名空间保留，以维持应用身份；它们不是访问凭据。

[导入](docs/import.md) · [MCP](docs/mcp.md) · [网页](docs/web.md) · [审计](docs/audit.md) · [iOS](ios/README.md) · [隐私](docs/privacy.md)
