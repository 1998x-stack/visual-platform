# Visual Platform 实施状态与安全边界

更新日期：2026-11-24

## 阶段状态

| 阶段 | 状态 | 已实现 | 未实现 / 边界 |
|---|---|---|---|
| M0 工程骨架 | 已实现 | Go API、结构化日志、健康 / 就绪检查、开发配置、CI | Worker / Gateway 仍为显式失败的 Stub |
| M1 领域与权限 | 已实现 | Workspace、成员角色、Artifact 元数据、ACL、Authorizer、审计 | 企业 OIDC / 组织目录未实现 |
| M2 上传隔离 | 已实现（开发基线） | Upload Session、配额预留、流式 Quarantine 写入、SHA-256、会话租约、重试、状态审计 | 生产级 MinIO 部署、生命周期清理 Worker、断点续传、多段上传未实现 |
| M3 ZIP Validator | 进行中（M3-A 分支） | ZIP / standalone HTML Validation Core、无磁盘解压、中央目录预检、路径 / 类型 / 大小 / 压缩比 / 数据重叠检查、CRC / SHA-256、确定性 Manifest、攻击样本与 Fuzz 种子测试 | 尚未合并；尚未接入 Quarantine Reader、ClamAV、Job Worker、数据库状态和 Manifest 持久化 |
| M4 Job / Worker | 未实现 | — | SKIP LOCKED、Lease、Retry、Outbox、幂等消费 |
| M5 Publisher | 未实现 | — | 不可变对象版本、Manifest 校验、原子指针切换、回滚 |
| M6 Static Gateway | 未实现 | — | Host Mapping、每请求授权、路径规范化、响应安全头 |
| M7 SSO / Organization | 未实现 | — | OIDC、PKCE、Ticket Exchange、部门 / 项目同步 |
| M8 生产化 | 未实现 | — | 指标、告警、备份、PITR、恢复演练 |
| M9 OpenCode Web | 未实现 | — | 原生上传发布入口与状态回写 |
| M10 Dynamic Runtime | 未实现 | — | 独立动态应用沙箱；不属于静态 MVP |

## M2 已实现的行为

- 仅 Workspace Owner / Maintainer / Editor 能为该 Workspace 的 Draft Artifact 创建上传会话。
- 创建会话时锁定 Workspace Quota 行，原子预留预期字节数，并检查总容量、单文件上限和活动会话数量。
- 默认 Workspace 配额：1 GiB 总量、100 MiB 单文件、5 个活动会话；会话 30 分钟过期。
- 接收端要求明确 Content-Length，必须与创建会话时声明的大小一致；不接受未知长度 / chunked body。
- API 使用服务端生成的 UUID 对象键，客户端文件名只作为展示元数据，不进入对象路径。
- API 通过流式 Reader 将字节写入私有 Quarantine Bucket；对象 Content-Type 固定为 `application/octet-stream`。
- 服务端计算 SHA-256；不匹配则状态转为 rejected、释放预留配额并尝试删除隔离对象。
- 成功后在 PostgreSQL 事务中将状态置为 received、reserved_bytes 转为 used_bytes，并写入审计事件。
- 上传 Claim 使用 10 分钟 Lease；Worker / API 异常退出后，租约到期可以重新 Claim。
- API 不提供对象下载、公开 URL、预签名 URL、发布或执行能力。

## API 契约

### 创建上传会话

`POST /api/v1/artifacts/{artifact_id}/uploads`

必需请求头：`Idempotency-Key: upload-request-0001`。同一 Workspace / 创建者范围内，同 Key 同参数重试返回原 Session；同 Key 不同参数返回冲突。

```json
{
  "source_name": "dashboard.zip",
  "source_type": "zip",
  "expected_size": 1048576,
  "expected_sha256": "<64 lowercase hex characters>"
}
```

### 上传内容

`PUT /api/v1/uploads/{upload_id}/content`

Body 为原始二进制字节。必须带精确 Content-Length；客户端 Content-Type 不参与安全判断。

### 查询状态

`GET /api/v1/uploads/{upload_id}`

状态：`created`、`receiving`、`received`、`rejected`、`expired`。

## 配置与运行

API 只有在以下四个变量全部设置时才启用上传：
- `S3_ENDPOINT`：host:port，不带协议和路径
- `S3_ACCESS_KEY`
- `S3_SECRET_KEY`
- `S3_BUCKET`：预先创建的私有隔离 Bucket

可选：`S3_REGION`、`S3_USE_SSL`、`MAX_UPLOAD_BYTES`。默认上传上限 100 MiB；非 development 环境强制 S3_USE_SSL=true。数据库 Workspace Quota 仍然是额外约束；提高 API 上限并不会自动提高 Workspace 的 max_single_upload_bytes。

对象存储 SDK：MinIO Go SDK v7.4.1（Apache-2.0）。启动时检查 Bucket 存在，并拒绝带有显式 Bucket Policy 的隔离 Bucket。API 不自动创建 Bucket。

## M2 尚未解决的运维事项

1. 过期会话的预留配额在该 Workspace 下次创建上传会话时惰性释放；定期 janitor 属于 M4 Worker。
2. API 在写入对象后、提交数据库状态前发生不确定故障时，对象保留在私有 Quarantine；M4 reconciliation Worker 负责对账和孤儿对象清理。
3. 当前通过 API 代理上传，不生成预签名 URL；大文件与高并发场景需基于真实带宽和 S3 policy 能力重新评估 multipart / presigned POST。
4. MinIO / S3 Bucket 必须独立于 Published Bucket，私有网络访问。生产环境应使用独立专用凭据，权限限制在单一 Quarantine Bucket。
5. M2 不验证文件内容是否真的是 HTML / ZIP；声明类型只是上传意图。所有对象在 M3 Validator 和扫描完成前都不能发布。
6. ZIP 不会在 API 中解压；不要把 Quarantine 对象挂载到 Web Root 或直接交给浏览器访问。

## 安全边界

- 企业 OIDC 未实现；无 DEV_AUTH 时业务 API fail closed。
- 开发身份模拟仅允许 APP_ENV=development + loopback listener，禁止经反向代理暴露。
- Quarantine Bucket 必须私有。应用没有下载 / 读取对象的方法，也没有向客户端返回对象键。
- 任何 `received` 状态都不代表安全、可信或可公开访问。
- 不执行用户上传的 Python、Node、PHP 或其他服务端代码。
- 不要将当前版本部署为企业生产文件托管服务。

## CI 验收

CI 在 PostgreSQL 18 上运行：
- Go 1.27.4 dependency / formatting / vet / build
- M1 migration + M2 migration，并重复执行迁移
- M0/M1 单元、HTTP 开发鉴权测试
- M1 / M2 PostgreSQL 集成测试
- `go test -race -count=1 ./...`



## M3-A ZIP Validation Core（独立实现，尚待 CI 验证）

当前分支：`feat/m3-zip-validation-core`。

### 已实现范围
- 新增 `internal/validator`，通过 `io.ReaderAt` 读取 ZIP；不把用户路径写入服务器文件系统，也不执行解压落盘。
- 在调用 `archive/zip.NewReader` 前检查 EOCD，限制中央目录大小和条目数；MVP 明确拒绝 ZIP64 与多卷 ZIP。
- 仅接受 Store / Deflate；拒绝加密条目、符号链接、特殊文件和服务端执行 / 嵌套归档扩展名。
- 校验 UTF-8 / NFC、绝对路径、盘符、反斜杠、点段、隐藏文件、控制字符、Windows 保留名、路径长度 / 深度。
- 以 Unicode case-fold 后的路径键检测重复项、大小写冲突和文件 / 目录前缀冲突。
- 检查单文件大小、总解压大小、压缩比、文件数、压缩数据区间重叠；读取每个文件到 EOF，触发 ZIP CRC 校验并计算 SHA-256。
- 要求根目录存在精确小写 `index.html`；若所有文件均处于同一个顶层目录且不存在根 index，则剥离这一层目录。
- ZIP 与单 HTML 均生成排序稳定的 Manifest v1；ZIP 条目同时保留发布路径和原始 archive source_path，包含大小、SHA-256、服务端 MIME、源大小和总解压大小。
- 加入 ZIP Slip、Symlink、重复路径、大小写冲突、压缩炸弹、加密、ZIP64、CRC、缺少 index、单 HTML 大小 / 源长度 / 取消上下文和 Fuzz seed 测试。

### 当前不代表什么
- M3-A 是纯验证核心，不会从 S3 读取对象，不会写数据库，不会触发异步任务。
- 当前没有 ClamAV 集成；不能把结构验证通过解释为恶意软件扫描通过。
- Manifest 不是发布授权；上传内容仍必须留在私有 Quarantine。
- HTML / JavaScript 仍是用户控制的浏览器代码，必须使用独立 Artifact Origin。
- 默认阈值是 MVP 起点，需用真实构建样本和受控压测校准。

### M3-A 验收门槛
- CI 必须通过 gofmt、go mod tidy 检查、go vet、go build、PostgreSQL 集成测试和 `go test -race -count=1 ./...`。
- 在本地完成 Fuzz 扩展运行，并记录运行时间 / 覆盖情况；CI 的 Fuzz seed 测试不等于完成长期 Fuzz。
- PR 合并前由本地 Codex/Pi 在真实 Go 1.27.4 环境复核边界条件；任何未执行项目标记 NOT RUN。
