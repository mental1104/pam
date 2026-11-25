#!/bin/sh
# 仅在新的专用 PostgreSQL 数据卷首次启动时运行。
# 输入为三个独立角色密码；通过 psql 字面值引用传递，不向日志输出密码。
# 创建角色、收紧 PUBLIC 权限及默认表授权；失败时 initdb 停止，不触碰其他数据库。
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=migrator_password="$PAM_MIGRATOR_PASSWORD" \
  --set=app_password="$PAM_APP_PASSWORD" \
  --set=reader_password="$PAM_READER_PASSWORD" <<'SQL'
CREATE ROLE pam_migrator LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD :'migrator_password';
CREATE ROLE pam_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD :'app_password';
CREATE ROLE pam_reader LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD :'reader_password';
REVOKE ALL ON DATABASE pam FROM PUBLIC;
GRANT CONNECT ON DATABASE pam TO pam_migrator, pam_app, pam_reader;
GRANT CREATE ON DATABASE pam TO pam_migrator;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CREATE ON SCHEMA public TO pam_migrator;
CREATE SCHEMA pam AUTHORIZATION pam_migrator;
GRANT USAGE ON SCHEMA pam TO pam_app, pam_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE pam_migrator IN SCHEMA pam GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pam_app;
ALTER DEFAULT PRIVILEGES FOR ROLE pam_migrator IN SCHEMA pam GRANT SELECT ON TABLES TO pam_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE pam_migrator IN SCHEMA pam GRANT USAGE, SELECT ON SEQUENCES TO pam_app;
-- 独立数据库内关闭默认可执行权限，避免只读账号调用带副作用的系统函数。
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA pg_catalog FROM PUBLIC;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA pg_catalog TO pam_migrator, pam_app;
-- 仅授权无数据库写入/文件/网络/锁副作用的内建函数；AST 仍执行第二层更窄的校验。
SELECT format('GRANT EXECUTE ON FUNCTION %s TO pam_reader', p.oid::regprocedure)
FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname='pg_catalog' AND p.provolatile IN ('i','s')
AND p.proname IN (
'count','sum','avg','min','max','bool_and','bool_or','every',
'array_agg','string_agg','json_agg','jsonb_agg','json_object_agg','jsonb_object_agg',
'abs','ceil','ceiling','floor','round','trunc','mod','power','sqrt','sign',
'lower','upper','initcap','length','char_length','character_length','octet_length',
'trim','btrim','ltrim','rtrim','substring','substr','left','right','position','strpos',
'replace','translate','concat','concat_ws','split_part','regexp_replace','regexp_match','regexp_matches',
'date_trunc','date_part','extract','age','to_char','to_date','to_timestamp',
'make_date','make_timestamp','make_timestamptz','timezone','now','transaction_timestamp','statement_timestamp',
'json_build_object','jsonb_build_object','json_build_array','jsonb_build_array',
'json_array_length','jsonb_array_length','json_extract_path','jsonb_extract_path',
'json_extract_path_text','jsonb_extract_path_text','json_typeof','jsonb_typeof','jsonb_pretty','jsonb_strip_nulls',
'jsonb_array_elements','jsonb_array_elements_text','json_each','jsonb_each','json_each_text','jsonb_each_text','json_object_keys','jsonb_object_keys',
'array_length','array_lower','array_upper','array_position','array_positions','array_to_string','cardinality','unnest','generate_series',
'row_number','rank','dense_rank','percent_rank','cume_dist','ntile','lag','lead','first_value','last_value','nth_value',
'percentile_cont','percentile_disc','mode','stddev','stddev_pop','stddev_samp','variance','var_pop','var_samp',
'encode','decode','md5',
'text','name','int2','int4','int8','float4','float8','numeric','date','timestamp','timestamptz',
'time','timetz','interval','uuid','varchar','bpchar','bool','json','jsonb','bytea',
'has_schema_privilege','has_database_privilege','has_table_privilege','current_database')
\gexec
-- PostgreSQL 运算符也检查其底层函数的 EXECUTE 权限；仅放行应用 SQL 策略允许的内建运算符。
-- 包含 name/text 比较以支持启动时的角色权限检查，不放行用户 schema 中的任何实现。
SELECT DISTINCT format('GRANT EXECUTE ON FUNCTION %s TO pam_reader',p.oid::regprocedure)
FROM pg_operator o JOIN pg_proc p ON p.oid=o.oprcode
JOIN pg_namespace n ON n.oid=o.oprnamespace
WHERE n.nspname='pg_catalog' AND p.provolatile IN ('i','s')
AND o.oprname IN ('+','-','*','/','%','^','=','<>','<','<=','>','>=','||',
'~~','!~~','~~*','!~~*','~','!~','~*','!~*','->','->>','#>','#>>','@>','<@','?','?|','?&','&&','&','|','#','<<','>>')
\gexec
-- clock_timestamp 虽为 volatile，但只读取时钟；与上面的纯分析函数一起显式授权。
GRANT EXECUTE ON FUNCTION pg_catalog.clock_timestamp() TO pam_reader;
ALTER ROLE pam_app SET search_path = pam, pg_catalog;
ALTER ROLE pam_reader SET search_path = pam, pg_catalog;
ALTER ROLE pam_reader SET default_transaction_read_only = on;
ALTER ROLE pam_reader SET statement_timeout = '3s';
ALTER ROLE pam_reader SET lock_timeout = '1s';
ALTER ROLE pam_reader SET idle_in_transaction_session_timeout = '5s';
ALTER ROLE pam_reader CONNECTION LIMIT 5;
SQL
