-- PAM V0 的规范化流水与原始来源分开存储；此迁移由数据库所有者在单一事务内执行。
CREATE SCHEMA IF NOT EXISTS pam;

CREATE TABLE pam.import_batches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source text NOT NULL,
    idempotency_key text NOT NULL,
    source_file_hash text,
    payload_hash text NOT NULL,
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (source, idempotency_key)
);

CREATE TABLE pam.transactions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    occurred_at timestamptz NOT NULL,
    direction text NOT NULL CHECK (direction IN ('income', 'expense')),
    amount numeric(20,4) NOT NULL CHECK (amount >= 0),
    currency text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    merchant text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    category text NOT NULL DEFAULT '',
    source text NOT NULL,
    external_id text,
    import_batch_id uuid NOT NULL REFERENCES pam.import_batches(id),
    fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
-- 稳定外部编号可强制唯一；指纹只建普通索引，合法重复消费必须能够显式创建。
CREATE UNIQUE INDEX transactions_source_external_idx ON pam.transactions(source, external_id) WHERE external_id IS NOT NULL;
CREATE INDEX transactions_occurred_idx ON pam.transactions(occurred_at DESC, id);
CREATE INDEX transactions_source_date_idx ON pam.transactions(source, occurred_at DESC);
CREATE INDEX transactions_direction_date_idx ON pam.transactions(direction, occurred_at DESC);
CREATE INDEX transactions_merchant_idx ON pam.transactions(lower(merchant));
CREATE INDEX transactions_category_date_idx ON pam.transactions(category, occurred_at DESC);
CREATE INDEX transactions_amount_idx ON pam.transactions(amount);
CREATE INDEX transactions_fingerprint_idx ON pam.transactions(source, fingerprint);

CREATE TABLE pam.import_records (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id uuid NOT NULL REFERENCES pam.import_batches(id),
    row_index integer NOT NULL,
    row_key text,
    original jsonb NOT NULL,
    normalized_submission jsonb NOT NULL,
    transaction_id uuid REFERENCES pam.transactions(id),
    status text NOT NULL CHECK (status IN ('created', 'skipped', 'duplicated', 'failed')),
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (batch_id, row_index)
);
CREATE INDEX import_records_transaction_idx ON pam.import_records(transaction_id);
CREATE INDEX import_batches_source_file_idx ON pam.import_batches(source, source_file_hash) WHERE source_file_hash IS NOT NULL;

CREATE TABLE pam.assets (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (length(name) > 0),
    category text NOT NULL DEFAULT '',
    brand text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    serial_number text NOT NULL DEFAULT '',
    purchase_date date,
    purchase_price numeric(20,4) CHECK (purchase_price >= 0),
    currency text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    merchant text NOT NULL DEFAULT '',
    related_transaction_id uuid REFERENCES pam.transactions(id),
    notes text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'stored', 'loaned', 'sold', 'retired', 'lost')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX assets_name_idx ON pam.assets(lower(name));
CREATE INDEX assets_transaction_idx ON pam.assets(related_transaction_id);

-- SQL 审计只保存解析器脱敏后的模式，绝不保存参数或数据库原始错误消息。
CREATE TABLE pam.query_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    fingerprint text NOT NULL,
    normalized_sql text NOT NULL,
    executed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    duration_ms bigint NOT NULL DEFAULT 0,
    row_count integer NOT NULL DEFAULT 0,
    status text NOT NULL CHECK (status IN ('running', 'success', 'rejected', 'error')),
    error_code text,
    source text NOT NULL DEFAULT 'mcp.query_readonly_sql'
);
CREATE INDEX query_audit_pattern_time_idx ON pam.query_audit(fingerprint, executed_at DESC);
CREATE INDEX query_audit_time_idx ON pam.query_audit(executed_at DESC);

CREATE VIEW pam.query_patterns AS
SELECT fingerprint, normalized_sql, count(*) AS calls,
       avg(duration_ms) AS average_duration_ms, max(duration_ms) AS max_duration_ms,
       min(executed_at) AS first_seen, max(executed_at) AS last_seen,
       count(*) FILTER (WHERE status IN ('error', 'rejected')) AS failures,
       count(*) FILTER (WHERE executed_at >= now() - interval '7 days') AS recent_calls
FROM pam.query_audit
GROUP BY fingerprint, normalized_sql;

COMMENT ON TABLE pam.transactions IS 'Canonical electronic income and expense records; amount is unsigned and direction determines cash flow. No product purchase is proven by a payment alone.';
COMMENT ON TABLE pam.import_batches IS 'Import provenance and idempotency scope; source_file_hash is the SHA-256 of the original file when available.';
COMMENT ON TABLE pam.import_records IS 'Original source JSON and submitted normalized data for every accepted batch row, including failures and duplicate candidates.';
COMMENT ON COLUMN pam.import_records.row_index IS 'Zero-based position in the submitted batch.';
COMMENT ON COLUMN pam.import_records.row_key IS 'Stable original file row identity; meaningful together with source and source_file_hash.';
COMMENT ON COLUMN pam.transactions.fingerprint IS 'Cautious duplicate candidate key; intentionally non-unique to preserve confirmed repeated purchases.';
COMMENT ON COLUMN pam.transactions.import_batch_id IS 'Batch that initially created the transaction; later provenance is in import_records.';
COMMENT ON COLUMN pam.transactions.occurred_at IS 'Source transaction time with explicit timezone, stored as an absolute timestamptz.';
COMMENT ON COLUMN pam.transactions.amount IS 'Exact nonnegative numeric(20,4); APIs encode this as a decimal string.';
COMMENT ON TABLE pam.assets IS 'Personal physical inventory; related_transaction_id is an optional user-confirmed association.';
COMMENT ON TABLE pam.query_audit IS 'Literal-free SQL invocation audit including rejected and failed attempts.';
COMMENT ON VIEW pam.query_patterns IS 'Query frequencies, recent activity, durations and failure counts aggregated by normalized SQL pattern.';
