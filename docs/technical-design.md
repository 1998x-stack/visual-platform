# Visual Platform 技术设计

本文件是 Visual Platform v1.0 的研发基线。交互式设计见 index.html，PDF 由同一 HTML 使用 Playwright Chromium 生成。

## 1. 系统边界

Visual Platform 托管员工生成的 HTML 与静态站点 ZIP。它不是普通文件网盘：每个 Artifact 都有稳定 ID、Workspace 归属、Owner、访问策略、不可变版本和发布 URL。

MVP 只交付静态内容。Python、Node、PHP、CGI、Streamlit、Gradio 等服务端代码不得在静态文件网关中执行。动态应用必须进入后续独立 Runtime Controller 和受限容器 / microVM 环境。

## 2. 技术基线

| 层 | 技术 | 责任 |
|---|---|---|
| Portal | React / Vue | 工作区、上传、权限、版本 UI |
| API | Go modular monolith | 领域服务、权限入口、元数据 |
| Worker | Go | ZIP 校验、扫描、Manifest、发布 |
| Static Gateway | Go | Host 解析、会话、每请求授权、静态流 |
| Metadata | PostgreSQL | 用户、Workspace、Artifact、ACL、Version、Job、Audit |
| Object Store | 私有 MinIO / S3 | Quarantine 与 Published 对象 |
| Edge | Nginx / Ingress | TLS、Wildcard DNS、速率限制 |
| Identity | OIDC + PKCE | 企业身份认证 |
| Queue | PostgreSQL Job + Outbox | Lease、重试、幂等、故障恢复 |

## 3. 领域模型与权限

Workspace 类型：Personal、Department、Project、Company。Workspace 归属 Owner 与 Artifact 创建者 created_by 分开。部门与项目成员关系独立同步，调岗不应自动移除项目成员。

Artifact 是稳定资源；Version 是不可变内容快照；Artifact ACL 支持 user / group 级别直接授权。角色为 Owner、Maintainer、Editor、Viewer。默认拒绝，Workspace Role 提供继承权限，Artifact ACL 提供额外 Allow。平台管理员默认不自动获得所有业务内容读取权，Break-glass 必须单独审批、限时并审计。

## 4. 浏览器安全边界

管理平台与用户内容使用不同可注册域名，建议每个 Artifact 独立 Host。不同 Origin 隔离 DOM、Web Storage 和 IndexedDB，但 Cookie Domain 规则不同；不能用共享父域 Cookie 简化 SSO。

OIDC 使用 Authorization Code Flow + PKCE S256，验证 issuer、audience、签名、exp、nonce、state 和 redirect URI。内容域通过一次性 Ticket Exchange 建立自己的 Host-only Session。Ticket 绑定 user_id、artifact_id、canonical_host、expires_at，短 TTL、单次兑换，数据库仅存 hash。

Cookie 使用 __Host- 前缀、Secure、HttpOnly、SameSite=Lax、Path=/，不设置 Domain。禁止把管理 Bearer Token 放入 URL 或 localStorage。

## 5. 上传和安全处理

上传只进入 Quarantine。接收后核验对象存在、长度、checksum、上传者、Workspace 和配额。处理状态：

    CREATED -> UPLOADING -> RECEIVED -> VALIDATING -> SCANNING
    -> READY_TO_PUBLISH -> PUBLISHING -> PUBLISHED

失败进入 REJECTED / FAILED / REVIEW_REQUIRED。任务由 PostgreSQL Job 表驱动，使用 SKIP LOCKED、lease_until、attempt_count、next_retry_at 和幂等键。业务状态和 Outbox Event 在同一数据库事务中提交。

ZIP 必须拒绝绝对路径、盘符、反斜杠、NUL、越界段、symlink 和特殊文件；检测重复路径、大小写 / Unicode 规范化冲突；按实际解压流累计总字节；限制文件数、单文件大小、压缩比、目录深度、CPU、内存、磁盘和时限。MVP 不递归展开嵌套 ZIP。默认入口为根目录 index.html。MIME 由服务端 Manifest 生成。

## 6. 不可变版本与发布一致性

对象键由服务端生成，版本使用独立前缀：

    quarantine/{upload_id}/source.zip
    published/{workspace_id}/{artifact_id}/versions/{version_id}/...

Worker 先写完整对象和 Manifest，校验 SHA-256、文件数和总字节；最后在 PostgreSQL 事务中把 Version 标记 READY 并切换 Artifact.current_version_id。MinIO 与 PostgreSQL 不共享 ACID 事务，因此对象先写、DB 指针后切换。失败时线上旧版本保持不变，新前缀进入延迟清理。回滚只切换数据库指针。

## 7. Static Gateway

每个 GET / HEAD 请求都执行：Host allowlist → Host 到 Artifact ID 映射 → Session → 用户状态 → Artifact 状态 → Authorizer → URL path 规范化 → Manifest 映射 → 私有对象读取。不得把 URL path 直接拼成 S3 Key。304 响应也必须先授权。

敏感内容默认 private, no-store。安全响应头包括 nosniff、no-referrer、Permissions-Policy、CSP 和 frame-ancestors。CSP 先 Report-Only，再根据静态页面兼容性逐步 enforce。CSP 是纵深防御，不替代 Origin 隔离。

## 8. 数据与服务账号

核心表：users、workspaces、workspace_members、artifacts、artifact_versions、artifact_acl、domain_mappings、upload_sessions、upload_jobs、outbox_events、audit_events、auth_tickets、sessions、quota_usage、share_links。

API 只写 Quarantine；Worker 读 Quarantine 并创建 Published 新版本；Gateway 只读 Published；Backup 按策略只读。PostgreSQL RLS 作为纵深防御，应用角色不能是表 Owner 或 BYPASSRLS。

## 9. Go 工程结构

    cmd/api
    cmd/worker
    cmd/gateway
    internal/identity
    internal/organization
    internal/workspace
    internal/artifact
    internal/permission
    internal/upload
    internal/archive
    internal/scanning
    internal/publishing
    internal/storage
    internal/gateway
    internal/jobs
    internal/audit
    internal/config
    migrations/
    deploy/
    tests/
    docs/

Handler 只处理 HTTP 与输入校验；Service 执行业务规则与事务；Repository 访问 PostgreSQL；Storage Adapter 访问 MinIO。Gateway 不拥有正式对象写权限。

## 10. API 范围

核心接口：创建 Workspace、创建 Artifact、创建 Upload Session、上传内容、完成上传、查询任务状态、发布、回滚、查询和更新 ACL、Owner 移交、归档。所有修改接口使用幂等键或 If-Match。错误响应统一 request_id，不暴露对象 Key、内部堆栈或其他租户资源是否存在。

## 11. 运维与测试

MVP Docker Compose 包含 Ingress、Portal、API、Worker、Gateway、PostgreSQL、MinIO、OIDC Broker 和 Scanner。数据库与对象存储位于私有网络。PostgreSQL 使用 WAL / PITR；MinIO 进行故障域备份；周期性核对 Manifest 与对象。

测试包括权限矩阵、ZIP Slip、Symlink、ZIP Bomb、Host 混淆、Cookie 隔离、Ticket 重放、发布故障注入、Worker 重启、DB / MinIO 恢复、并发上传与静态下载。P0 越权、Bucket 公共访问、路径穿越、管理会话泄漏、未发布内容可访问和失败发布覆盖均阻断上线。

## 12. 研发路线

M0 工程骨架；M1 Workspace / Artifact / ACL；M2 Quarantine / ZIP Validator；M3 Job / Worker / Manifest / Publisher；M4 Static Gateway / Host / TLS；M5 OIDC / Ticket / 审计；M6 监控 / 备份 / 安全验收；M7 OpenCode Web 原生发布入口；M8 独立动态 App Runtime。

## 13. 参考资料

- OWASP File Upload: https://cheatsheetseries.owasp.org/cheatsheets/File_Upload_Cheat_Sheet.html
- OWASP Authorization: https://cheatsheetseries.owasp.org/cheatsheets/Authorization_Cheat_Sheet.html
- MDN Same-origin: https://developer.mozilla.org/en-US/docs/Web/Security/Defenses/Same-origin_policy
- MDN CSP: https://developer.mozilla.org/en-US/docs/Web/Security/Practical_implementation_guides/CSP
- OpenID Connect Core: https://openid.net/specs/openid-connect-core-1_0.html
- RFC 7636 PKCE: https://www.rfc-editor.org/rfc/rfc7636.html
- PostgreSQL RLS: https://www.postgresql.org/docs/current/ddl-rowsecurity.html
- Go archive/zip: https://pkg.go.dev/archive/zip
- MinIO Versioning: https://docs.min.io/aistor/administration/objects-and-versioning/versioning/
- S3 Presigned URL: https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html
- GitHub Pages Actions: https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site

> 本仓库当前交付的是方案和文档构建流程，不代表生产代码、SSO、MinIO 集群或授权控制已经部署。部署时必须核验具体 MinIO / AIStor 版本、支持模式和许可证。
