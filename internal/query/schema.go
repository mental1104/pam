package query

// Schema 描述公开查询表及语义；迁移表、系统 catalog 与其他业务 schema 不属于 SQL 工具范围。
const Schema = `PAM V0 read-only query schema

Only SELECT (including joins, nested selects, CTEs, grouping, windows and unions) is allowed.
Use unqualified table names or the pam schema. Use $1, $2 parameters, preferably decimal strings for money.
Numeric and floating result columns are returned as strings to preserve their exact PostgreSQL representation.
Timestamp output uses PostgreSQL text format with timezone. Lists may truncate at configured row/byte limits.
The supplied deployment sets a 3-second timeout, 500 rows, 256 KiB JSON response and 32 KiB SQL input.

pam.transactions:
  id uuid primary key; occurred_at timestamptz; direction text income|expense;
  amount numeric(20,4), non-negative; currency text, uppercase ISO-style three-letter code;
  merchant text; description text; category text (empty when unspecified); source text;
  external_id text nullable; import_batch_id uuid references import_batches(id);
  fingerprint text (non-unique cautious duplicate candidate key);
  created_at, updated_at timestamptz.
  Stable external_id values are unique within a source; external_id may be null.
  Original source data is in import_records, NOT a transactions.original column.
  Payment records identify date, merchant and money, not necessarily the purchased product.
  Treat product matches based on merchant/description as candidates unless original data confirms the product.

pam.import_proposals: id uuid; source, idempotency_key, payload_hash text; request, preview jsonb; committed boolean; created_at timestamptz. Preview is not a ledger write.
pam.import_reviews: id uuid; record_id uuid references import_records; kind duplicate|correction|parse_error; state pending|resolved; resolution jsonb; created_at, resolved_at timestamptz. Similarity is only a candidate, never confirmed identity.
pam.transaction_revisions: id uuid; transaction_id uuid; review_id uuid; before_content, after_content, evidence jsonb; reason text; created_at timestamptz. Retains explicit human correction evidence and snapshots.

pam.assets:
  id uuid primary key; name, category, brand, model, serial_number text;
  purchase_date date nullable; purchase_price nonnegative numeric(20,4) nullable; currency text;
  merchant text; related_transaction_id uuid optional references transactions(id);
  notes text; status text active|stored|loaned|sold|retired|lost;
  created_at, updated_at timestamptz. Name is required; other text defaults to empty.

pam.import_batches:
  id uuid primary key; source text; idempotency_key text;
  source_file_hash text nullable (SHA-256 original file); payload_hash text;
  result jsonb nullable (import statistics and row outcomes); created_at timestamptz.
  Unique(source,idempotency_key) makes identical requests retryable and rejects changed content.
pam.import_records:
  id uuid primary key; batch_id uuid references import_batches(id); row_index integer (zero based);
  row_key text nullable (stable source file row identity); original jsonb;
  normalized_submission jsonb; transaction_id uuid nullable references transactions(id);
  status text created|skipped|duplicated|failed; reason text; created_at timestamptz.
  Unique(batch_id,row_index). Join r.transaction_id=t.id for originals and r.batch_id=b.id for batches.
  Original records and failed/conflicting imports remain traceable; duplicate candidates are not proof of identity.

pam.query_audit:
  id uuid; fingerprint text; normalized_sql text (values and free identifiers redacted);
  executed_at timestamptz; duration_ms bigint; row_count integer;
  status running|success|rejected|error; error_code text nullable; source text.
  raw_sql text nullable; parameters jsonb nullable; raw_truncated boolean.
  Owner-authorized original SQL and bound values are retained for new records; legacy originals are unavailable.
  A running record without a terminal update indicates an interrupted invocation or audit outage.
  Parser failures and non-SELECT statements use a fixed privacy-safe placeholder pattern.

pam.query_patterns:
  fingerprint, normalized_sql, calls, average_duration_ms, max_duration_ms,
  first_seen, last_seen, failures, recent_calls (calls in the most recent seven days).
  Compare recent_calls/time windows with calls and timestamps for trends; do not infer causation from frequency.

No row locks, SELECT INTO, writes, DDL, unrestricted functions, custom operators/types,
system catalog access, file/network/large-object functions, set_config or advisory locks.
Supported functions include ordinary aggregates, numeric/string/date/JSON functions and windows.
Database statement timeout and a separate read-only role also apply.
`
