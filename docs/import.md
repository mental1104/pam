# 导入契约与 Atlas 口径

后端接受明确的规范化批次，不自动抓取支付平台。CSV准备器实现指定文件契约，截图链路使用本地OCR和确定性Parser，不依赖LLM。

每批包含 `source`、稳定 `idempotency_key`、可选的完整原文件 `source_file_hash`，以及 1–1000 条 `records`。提供文件哈希时每条记录必须有稳定 `row_key`。以下为合成示例：

```json
{
  "source": "example",
  "idempotency_key": "synthetic-statement-1",
  "records": [{
    "row_key": "1",
    "occurred_at": "2025-06-15T12:30:00+08:00",
    "direction": "expense",
    "amount": "199.9900",
    "currency": "CNY",
    "merchant": "Example shop",
    "description": "Payment; product unconfirmed",
    "category": "electronics",
    "external_id": "synthetic-1",
    "original": {"source_row": "synthetic", "amount_text": "-199.9900"}
  }]
}
```

金额使用非负十进制字符串，最多 16 位整数和 4 位小数，禁止指数、千位分隔和二进制浮点。时间必须包含时区。`original` 必须为 JSON 对象，完整保留来源信息。`confirm_new: true` 仅用于**经过确认**的无稳定编号重复候选，不能绕过稳定外部编号冲突。

后端在一个数据库事务内处理整批，保存点隔离可归因于单行的约束错误；数据库断开等基础设施失败回滚整批。统计四类互斥：created 新增、skipped 已存在、duplicated 待确认/编号冲突、failed 无效行。每条输入都在 `pam.import_records` 保留原始 JSON、规范化提交、结果原因和可用的交易关联。

重放同一 source/idempotency_key 和同样内容，不新增流水；返回 `replayed=true`、`created=0`，之前新增计入本次 skipped。同一幂等键但内容变化返回 409。改变分批大小、映射规则或原始 JSON 会改变提交内容；不要复用旧键修改历史批次。无稳定编号跨文件仅靠指纹相似的记录不会静默跳过。

## CSV与截图

pam-prepare只读转换YYYY-收支流水-最终合并.csv契约，保留原始列、来源日期/时区/精度。歧义或缺失行进入私人报告，不从金额符号猜未知方向，也不把日期占位午夜当作真实时间。

pam-prepare-cmb只读指定账单，可选旧分类参考不覆盖来源。稳定行身份优先来自图片、纵坐标和账户，不伪造外部编号。命令均要求显式input/output，不内置个人目录。公开样例全部在testdata/synthetic。

POST /api/v1/imports/proposals保存提案并跨来源查候选，preview.created只是候选新增数。GET /api/v1/imports/proposals/{id}跨会话恢复；POST /api/v1/imports/proposals/{id}/confirm人工确认后重新查重，以最终回执为准。直接导入可用review_cross_source=true；相近时间/金额/币种/方向/商户仅产生疑似重复。

GET /api/v1/imports/reviews?state=pending|resolved&limit=50&offset=0读取持久待核；POST /api/v1/imports/reviews/{id}/resolve接收action、必填reason、可选transaction_id/record。link人工关联、create确认独立交易、correct保留前后快照/理由/证据、dismiss关闭但保留输入。不能绕过稳定编号冲突；同决定可重放，不同决定冲突。

GET /api/v1/imports/ranges返回已知来源/账户的记录范围和笔数；confirmed_coverage=[]表示未确认完整账单覆盖。网页、iOS及MCP manage_imports共用应用服务。

原图按SHA保存在iOS受保护目录，结构化来源保留OCR文本/坐标/置信度/尺寸、版本和人工复核标记。存在pam_parse_issues但未明确pam_human_reviewed=true时不自动入库。不上传截图给第三方OCR/LLM。

账单来源不等于支付渠道，多来源不拆为多笔支出。退款、账户划转、借款和消费有独立口径；未经来源标记确认，不把全部expense直接当净消费。商户匹配也不单独证明商品身份。
