# 部署、备份与恢复

公开deploy/compose.yaml提供PostgreSQL17、一次性pam-migrate及应用pam，默认只映射应用回环端口。个性化域名、白名单、挂载、代理/隧道、NAS维护入口和密码在仓库外的私有运维目录维护，通过私有Compose覆盖文件接入。

## 角色和升级

pam_admin仅初始化/受控备份恢复；pam_migrator执行版本化迁移；pam_app负责业务及审计写入；pam_reader仅运行受限SELECT。首次通过deploy/init-db.sh建立权限，常驻应用不持有管理员或迁移密码。不要用管理员角色替代测试中的受限账号。

先验证新版本和迁移，再备份当前一致性状态。升级只停止本应用写入者，运行匹配版本迁移后启动应用，保留上一镜像与全部新写入。不要删除数据卷、重写已发布迁移或用旧dump覆盖运行账本。应用与迁移使用同一已验证镜像。

## 备份

deploy/backup.sh在PostgreSQL客户端环境执行，使用PGHOST/PGDATABASE/PGUSER/PGPASSWORD。约定源码只读挂载/source，仓库外受限备份挂载/backups。参数为新快照名称，不覆盖已有目录，不自动删除历史。快照含pam.dump、清单、权限初始化脚本和构建依赖；摘要验证后才写COMPLETE。

dump不含集群密码或完整代理/运维环境，私有配置另行保护。要求停写比对的恢复演练应从备份开始保持相关写入者停止。

## 恢复与网络

deploy/restore-check.sh接收快照名及一个全新pam_restore_*数据库名；验证COMPLETE/摘要，拒绝原库或已存在目标，仅恢复到隔离新库并比对逐表行数、完整行JSON摘要及来源关联。失败目标保留排查，不覆盖生产。

可信HTTPS代理指向应用回环端口，保留Authorization、MCP请求头及响应流，不缓存金融响应。不要开放PostgreSQL。网页Cookie不认证MCP；可选SSO见[web.md](web.md)。共享构建器、NAS任务、生产浏览器验收和私人统计由独立私有运维项目管理。
