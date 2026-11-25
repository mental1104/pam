-- 用户于 2026-11-25 明确要求保存今后原始 SQL 与绑定参数；旧行保持 NULL，不伪造历史原文。
ALTER TABLE pam.query_audit ADD COLUMN raw_sql text;
ALTER TABLE pam.query_audit ADD COLUMN parameters jsonb;
ALTER TABLE pam.query_audit ADD COLUMN raw_truncated boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN pam.query_audit.raw_sql IS 'Original submitted SQL under owner-authorized capture policy; NULL means legacy or unavailable, not empty SQL.';
COMMENT ON COLUMN pam.query_audit.parameters IS 'Original bound parameters; may contain private financial values. Never logged to application logs.';
COMMENT ON COLUMN pam.query_audit.raw_truncated IS 'Capture exceeded the bounded 128 KiB SQL/parameter budget; UI must not present it as complete.';
