# ADR-0005：M2 上传隔离与流式 Quarantine

- 状态：Accepted
- 日期：2026-11-23

## 背景

M1 已建立 Workspace / Artifact / ACL，但尚无二进制上传能力。直接把用户文件写入 Web Root 或 Published Bucket 会把未验证 HTML / ZIP 暴露给浏览器，并将上传、发布和执行边界混在一起。

## 决策

1. M2 引入 Upload Session 和 Workspace Quota，上传前声明大小和 SHA-256。
2. 创建 Session 必须提供 Idempotency-Key；同 Workspace / 创建者下同 Key 同参数返回原 Session，参数不同返回冲突，避免客户端重试重复预留配额。
2. Quota reservation 与 Session 创建在同一 PostgreSQL 事务内完成；使用 Workspace Quota 行锁防止并发超额预留。
3. 文件内容由 API 代理流式写入单独的私有 Quarantine Bucket。M2 不返回预签名 URL，也不提供对象读取接口。
4. 对象 Key 由服务端生成 UUID，不使用用户文件名；写入 Content-Type 固定为 application/octet-stream。
5. API 要求精确 Content-Length，流式计算 SHA-256；不匹配则拒绝 Session、释放 reservation 并尝试删除对象。
6. Session 30 分钟过期，上传 Claim Lease 10 分钟；租约过期允许重新 Claim。
7. `received` 仅代表字节完整且 checksum 匹配，不代表内容安全。任何对象在 M3 验证与扫描前不得发布或浏览器读取。
8. Quarantine Bucket 必须预先创建且无公开 Bucket Policy；生产 API 使用只限该 Bucket 的专用凭据。

## 后果与取舍

- API 代理上传实现简单、能严格验证请求长度和哈希，但会消耗 API 网络带宽；未来如改为 Presigned POST / Multipart，必须验证 S3 policy 对 key、content-length、expiry 的约束并保留完成后 Stat / checksum 校验。
- 过期 Session 的 reservation 目前在同一 Workspace 下一次创建上传会话时惰性释放；定时清理、孤儿对象对账放在 M4 Worker。
- M2 不做 ZIP 解压或病毒扫描，避免在 API 请求进程解析不可信压缩包。
- MinIO / S3 Bucket 生命周期、版本、加密、备份和恢复由部署环境负责，尚未形成生产部署模板。

## 拒绝的方案

- 直接上传到 Published Bucket。
- 使用客户端提供的文件名作为对象键。
- 仅信任客户端 Content-Type / Content-Length / ETag。
- 在 API 进程同步解压 ZIP。
- 将 MinIO Root Access Key 放入 API 环境变量。
