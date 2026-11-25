#!/bin/bash
# 用途：把已完成快照恢复到一个全新的 pam_restore_* 隔离数据库，并与当前停写的业务库逐表核对行数。
# 输入：$1 为 /backups 内快照目录名，$2 为新数据库名；PGHOST/PGDATABASE/PGUSER/PGPASSWORD 提供管理连接。
# 输出：恢复证据目录；失败返回非零，不删除目标数据库，不改写正式库，不恢复旧凭据。
# 前置条件：备份后相关写入者保持停止，避免用变化中的当前计数与快照比较。
set -euo pipefail
umask 077
pam_snapshot_name="${1:-}"
pam_restore_database="${2:-}"
case "$pam_snapshot_name" in ''|.|..|*[!a-zA-Z0-9._-]*) echo 'Invalid snapshot name' >&2; exit 1;; esac
case "$pam_restore_database" in pam_restore_?*) ;; *) echo 'Restore target must start with pam_restore_ and include a suffix' >&2; exit 1;; esac
case "$pam_restore_database" in *[!a-z0-9_]*) echo 'Restore target contains invalid characters' >&2; exit 1;; esac
[ "${#pam_restore_database}" -le 63 ] || { echo 'Restore target is too long' >&2; exit 1; }
pam_source_database="${PGDATABASE:-pam}"
[ "$pam_restore_database" != "$pam_source_database" ] || { echo 'Restore target must differ from source' >&2; exit 1; }
pam_snapshot="/backups/$pam_snapshot_name"
[ -d "$pam_snapshot" ] && [ ! -L "$pam_snapshot" ] && [ -f "$pam_snapshot/COMPLETE" ] || { echo 'Snapshot is not complete' >&2; exit 1; }
for pam_file in pam.dump contents.txt init-db.sh go.mod SHA256SUMS COMPLETE; do
  [ -f "$pam_snapshot/$pam_file" ] && [ ! -L "$pam_snapshot/$pam_file" ] || { echo 'Snapshot file is missing or is a symlink' >&2; exit 1; }
done
cd "$pam_snapshot"
sha256sum -c SHA256SUMS >/dev/null

# 使用 psql 的字面值引用检查存在性，不把数据库名拼接为可执行 SQL。
pam_exists="$(psql -X -qAt -v ON_ERROR_STOP=1 --dbname=postgres --set=target="$pam_restore_database" <<'SQL'
SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'target');
SQL
)"
[ "$pam_exists" = f ] || { echo 'Restore target already exists; refusing to overwrite it' >&2; exit 1; }
pam_evidence="$pam_snapshot/restore-$pam_restore_database"
mkdir "$pam_evidence"

# 只统计业务库用户表；SQL 标识符由服务器 format(%I) 引用，输出不包含任何财务正文。
psql -X -qAt -v ON_ERROR_STOP=1 --dbname="$pam_source_database" > "$pam_evidence/expected-counts.tsv" <<'SQL'
SELECT format('SELECT %L || chr(9) || count(*)::text FROM %I.%I', table_schema || '.' || table_name, table_schema, table_name)
FROM information_schema.tables
WHERE table_schema IN ('pam','public') AND table_type='BASE TABLE'
ORDER BY table_schema, table_name
\gexec
SQL

# $1 为数据库名、$2 为输出摘要文件；只将服务器 COPY 的规范行 JSON 流送入 SHA-256，不持久化财务明文。
# 所有当前表都有 id 或 version；按该稳定标识排序并固定 UTC，使时间、UUID、精确金额和来源 JSON 可完整比对。
# pipefail 使 COPY 或数据库连接失败不能被成功计算空流摘要掩盖。
table_hashes() {
  local pam_hash_database="$1" pam_hash_output="$2" pam_table pam_count pam_schema pam_name pam_hash
  : > "$pam_hash_output"
  while IFS=$'\t' read -r pam_table pam_count; do
    pam_schema="${pam_table%%.*}"
    pam_name="${pam_table#*.}"
    pam_hash="$(psql -X -qAt -v ON_ERROR_STOP=1 --dbname="$pam_hash_database" --set=table_schema="$pam_schema" --set=table_name="$pam_name" <<'SQL' | sha256sum
SET TIME ZONE 'UTC';
SELECT format('COPY (SELECT to_jsonb(t)::text FROM %I.%I t ORDER BY COALESCE(to_jsonb(t)->>''id'',to_jsonb(t)->>''version'') COLLATE "C") TO STDOUT', :'table_schema', :'table_name')
\gexec
SQL
)"
    printf '%s\t%s\n' "$pam_table" "${pam_hash%% *}" >> "$pam_hash_output"
  done < "$pam_evidence/expected-counts.tsv"
}
table_hashes "$pam_source_database" "$pam_evidence/expected-content-sha256.tsv"

# 新库归迁移账号所有，但不授予应用/只读账号连接权；该库仅作为恢复验证证据保留。
createdb --maintenance-db=postgres --owner=pam_migrator --template=template0 "$pam_restore_database"
psql -X -v ON_ERROR_STOP=1 --dbname=postgres --set=target="$pam_restore_database" > "$pam_evidence/database-acl.log" <<'SQL'
SELECT format('REVOKE CONNECT ON DATABASE %I FROM PUBLIC', :'target') \gexec
SQL
printf '%s\n' "$pam_restore_database" > "$pam_evidence/database.txt"
pg_restore --exit-on-error --no-owner --no-acl --role=pam_migrator --dbname="$pam_restore_database" "$pam_snapshot/pam.dump" > "$pam_evidence/restore.log" 2>&1
psql -X -qAt -v ON_ERROR_STOP=1 --dbname="$pam_restore_database" > "$pam_evidence/restored-counts.tsv" <<'SQL'
SELECT format('SELECT %L || chr(9) || count(*)::text FROM %I.%I', table_schema || '.' || table_name, table_schema, table_name)
FROM information_schema.tables
WHERE table_schema IN ('pam','public') AND table_type='BASE TABLE'
ORDER BY table_schema, table_name
\gexec
SQL
cmp "$pam_evidence/expected-counts.tsv" "$pam_evidence/restored-counts.tsv"
table_hashes "$pam_restore_database" "$pam_evidence/restored-content-sha256.tsv"
cmp "$pam_evidence/expected-content-sha256.tsv" "$pam_evidence/restored-content-sha256.tsv"
psql -X -qAt -v ON_ERROR_STOP=1 --dbname="$pam_restore_database" > "$pam_evidence/integrity.tsv" <<'SQL'
SELECT 'transaction_batch_orphans', count(*) FROM pam.transactions t LEFT JOIN pam.import_batches b ON b.id=t.import_batch_id WHERE b.id IS NULL
UNION ALL SELECT 'import_record_batch_orphans', count(*) FROM pam.import_records r LEFT JOIN pam.import_batches b ON b.id=r.batch_id WHERE b.id IS NULL
UNION ALL SELECT 'import_record_transaction_orphans', count(*) FROM pam.import_records r LEFT JOIN pam.transactions t ON t.id=r.transaction_id WHERE r.transaction_id IS NOT NULL AND t.id IS NULL
UNION ALL SELECT 'asset_transaction_orphans', count(*) FROM pam.assets a LEFT JOIN pam.transactions t ON t.id=a.related_transaction_id WHERE a.related_transaction_id IS NOT NULL AND t.id IS NULL;
SQL
awk -F '|' '$2 != 0 { exit 1 }' "$pam_evidence/integrity.tsv"
cd "$pam_evidence"
sha256sum expected-counts.tsv restored-counts.tsv expected-content-sha256.tsv restored-content-sha256.tsv integrity.tsv restore.log database.txt database-acl.log > SHA256SUMS
date -u +%Y-%m-%dT%H:%M:%SZ > RESTORE_COMPLETE
printf '%s\n' "$pam_evidence"
