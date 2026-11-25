-- 预检只保存提案，不写账本。既有 import_records 继续保存实际提交的每行证据。
CREATE TABLE pam.import_proposals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source text NOT NULL,
    idempotency_key text NOT NULL,
    payload_hash text NOT NULL,
    request jsonb NOT NULL,
    preview jsonb NOT NULL,
    committed boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(source, idempotency_key)
);
-- 待核直接引用已有来源行；重启和跨会话读取不依赖客户端缓存。
CREATE TABLE pam.import_reviews (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    record_id uuid NOT NULL UNIQUE REFERENCES pam.import_records(id),
    kind text NOT NULL CHECK(kind IN ('duplicate','correction','parse_error')),
    state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','resolved')),
    resolution jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    resolved_at timestamptz
);
CREATE INDEX import_reviews_pending_idx ON pam.import_reviews(state,created_at,id);
-- 账本更正以不可变前后快照留痕；不覆盖或删除原始 import_records。
CREATE TABLE pam.transaction_revisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id uuid NOT NULL REFERENCES pam.transactions(id),
    review_id uuid NOT NULL UNIQUE REFERENCES pam.import_reviews(id),
    before_content jsonb NOT NULL,
    after_content jsonb NOT NULL,
    reason text NOT NULL,
    evidence jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX transaction_revisions_transaction_idx ON pam.transaction_revisions(transaction_id,created_at DESC,id DESC);
