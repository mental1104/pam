#!/bin/sh
# 输入 backup 目录名（可省略）；使用专用 PAM 管理连接生成一致性逻辑快照。
# 依赖 PostgreSQL 客户端、sha256sum；仅写 /backups，不自动删除历史，也不停止业务。
# 成功输出快照路径；失败不生成 COMPLETE，调用方不得把半成品视为有效备份。
set -eu
umask 077
pam_batch="${1:-pam-$(date -u +%Y%m%dT%H%M%SZ)}"
case "$pam_batch" in ''|.|..|*[!a-zA-Z0-9._-]*) echo 'Invalid backup name' >&2; exit 1;; esac
pam_target="/backups/$pam_batch"
mkdir "$pam_target"
pg_dump --format=custom --no-owner --no-acl --file="$pam_target/pam.dump"
pg_restore --list "$pam_target/pam.dump" > "$pam_target/contents.txt"
cp /source/deploy/init-db.sh "$pam_target/init-db.sh"
cp /source/go.mod "$pam_target/go.mod"
cd "$pam_target"
sha256sum pam.dump contents.txt init-db.sh go.mod > SHA256SUMS
sha256sum -c SHA256SUMS >/dev/null
date -u +%Y-%m-%dT%H:%M:%SZ > COMPLETE
printf '%s\n' "$pam_target"
