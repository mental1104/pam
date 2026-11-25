# 招商银行CSV准备器

只读显式指定的账单CSV，输出私人导入批次/待核报告，不连接网络或数据库，不修改原件。可选旧分类表仅供参考。

```sh
# 路径均为部署者的仓库外私有输入/输出，目标目录必须尚不存在。
go run ./cmd/pam-prepare-cmb --input /private/statements/cmb.csv --reference /private/statements/classification.csv --output /private/prepared/cmb-new --batch-size 100
```

批大小50–100，保留原始列、金额、精度、账户及位置证据，不用行号伪造银行编号。后续增量需核对旧交易/候选，不能只换幂等键就整表重导。详见[导入契约](../../docs/import.md)。
