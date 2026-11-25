#!/usr/bin/env bash
# 检查已跟踪文件，阻止常见私人路径、凭据和数据文件误入公开代码。
# 无参数；依赖 Git、grep、awk。只输出文件路径和固定原因，不输出匹配正文；不访问运行数据库。
# 这是有限的发布检查，不替代对 diff、历史、公开讨论及 CI 工件的人工审阅。
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
pam_failed=0
while IFS= read -r -d '' pam_file; do
  case "$pam_file" in
    .env.example) ;;
    .env|.env.*|*.env|*.dump|*.db|*.sqlite*|*.pem|*.key|*.p12|*.har|*.log|*.jsonl|*.bundle|*.png|*.jpg|*.jpeg|*.heic|*.pdf|data/*|private/*|evidence/*|backups/*|corpus/*)
      printf 'Private file type or location: %s\n' "$pam_file" >&2
      pam_failed=1 ;;
    *.csv)
      # 只允许显式隔离的合成 CSV；每个数据行都必须有 Synthetic 标记。
      if [[ "$pam_file" != testdata/synthetic/*.csv ]] || ! awk 'NR>1 && NF && $0 !~ /Synthetic/ {exit 1}' "$pam_file"; then
        printf 'CSV is outside the declared synthetic fixtures: %s\n' "$pam_file" >&2
        pam_failed=1
      fi ;;
  esac
done < <(git ls-files -z)
# 只报告文件名；避免发布检查本身在 CI 日志里复述误提交的秘密值。
pam_pattern='/volume[0-9]+/|-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY|github_pat_|gh[pousr]_[A-Za-z0-9]{20,}|sk-proj-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|[A-Za-z0-9._%+-]+@(gmail|outlook|qq|163|icloud)\.com'
if git grep -I -l -E "$pam_pattern" -- . ':!scripts/check-public-content.sh' ':!go.sum'; then
  printf 'Review the listed files before publication.\n' >&2
  pam_failed=1
else
  pam_grep_status=$?
  # grep 返回1表示无匹配，其余错误不可当成成功扫描。
  [[ "$pam_grep_status" -eq 1 ]] || exit "$pam_grep_status"
fi
[[ "$pam_failed" -eq 0 ]] || exit 1
printf 'Public-content checks passed.\n'
