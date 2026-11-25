# 同源网页与身份代理

网页嵌入Go二进制，复用同源REST，提供账本、来源、资产、导入待核和SQL审计，无第三方图表/遥测服务。

需要可信HTTPS入口，例如https://pam.example.com。密钥仅在同源POST中提交，服务端发放随机HttpOnly/Secure/SameSite=Strict的__Host-pam_session并仅存摘要，12小时绝对到期，退出撤销，进程重启使旧会话失效。密钥不进入URL、HTML、localStorage或sessionStorage。写请求验证精确HTTPS Origin。

可选Pomerium SSO仅对/web/sso接受签名断言。配置PAM_WEB_SSO_JWKS_URL、PAM_WEB_SSO_ISSUER、PAM_WEB_SSO_AUDIENCE、PAM_WEB_SSO_USERNAME；公钥来源需受控，校验签名算法/kid/iss/aud/exp及配置用户名，不信任明文身份头。代理也需要对应的账号准入策略。

域名、账号白名单及挂载路径保存在部署者私有配置。不配置SSO时可使用密钥登录；Cookie只认证REST，MCP始终要求Bearer。空集合、失败和重试明确显示，不以合成值填充生产。
