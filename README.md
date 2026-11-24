# Visual Platform

企业内部 HTML / ZIP 可视化产物托管、权限治理与安全发布平台。

> 目标：员工把本地生成的 HTML 或静态站点 ZIP 上传到统一平台，平台完成安全校验、权限绑定和版本发布，生成稳定 URL，支持跨部门协作、访问审计与回滚。

## 文档入口

- **[交互式技术方案](docs/index.html)**：系统架构、数据流、权限模型、安全边界、工程路线。
- **[核心技术实施方案 PDF](docs/visual-platform-core-technical-design.pdf)**：可下载、可打印的完整技术设计。
- [技术设计源文档](docs/technical-design.md)
- [研发实施路线图](docs/implementation-roadmap.md)
- [架构决策记录 ADR](docs/adr/)

GitHub Pages（启用 Actions 发布后）：  
`https://1998x-stack.github.io/visual-platform/`

## 核心架构

```text
Employee Browser
   | OIDC / SSO
   v
Management Portal ---- Go API ---- PostgreSQL
   |                     |
   | upload              | jobs / audit / metadata
   v                     v
Quarantine Bucket <--- Upload Worker
                          |
                    validate / scan
                          |
                    immutable publish
                          v
                    Published Bucket
                          ^
                          | private read
Artifact Browser --> Ingress --> Go Static Gateway
                                   |
                            host + session + ACL
```

## 技术基线

| Concern | MVP choice |
|---|---|
| API / Gateway / Worker | Go modular monolith, split runtime processes |
| Metadata / authorization | PostgreSQL |
| Binary objects | Private MinIO / S3-compatible storage |
| Edge routing | Nginx / ingress + wildcard DNS + TLS |
| Identity | Enterprise OIDC Authorization Code Flow + PKCE |
| Authorization | Workspace RBAC + Artifact ACL, deny by default |
| Static serving | Go Static Gateway, authorize every request |
| Background jobs | PostgreSQL queue with leases, retries and transactional outbox |
| Release model | Immutable version prefix + atomic DB pointer switch |

## MVP 边界

- 支持单个 HTML 文件和静态站点 ZIP。
- 支持 Personal / Department / Project / Company Workspace。
- 支持 Owner、Maintainer、Editor、Viewer 和单资源 ACL。
- 上传进入私有隔离区；验证、扫描、生成 Manifest 后才能发布。
- 每个 Artifact 独立 Host / Origin；内容域与管理域使用不同可注册域名。
- MinIO Bucket 私有；浏览器只访问 Static Gateway。
- 支持不可变版本、发布、回滚、归档、权限撤销和审计。
- **不执行用户上传的 Python、Node、PHP 或其他服务端代码。** 动态应用运行时是独立的后续阶段。

## 安全不变量

1. Default deny；每个管理 API 和静态资源请求都必须授权。
2. 文件名、MIME、ZIP 路径、解压大小和压缩比均由服务端重新验证。
3. Quarantine 与 Published 使用不同 Bucket / Service Account。
4. 内容 Origin 不共享管理域 Cookie；使用 Host-only `__Host-` Cookie。
5. 发布版本不可变；失败发布不能覆盖当前线上版本。
6. 预签名 URL 只用于短时上传，不用于长期阅读授权。
7. 所有发布、回滚、ACL、Owner 转移和拒绝访问都留下审计记录。

## 本地预览与构建

需要 Node.js 22+。

```bash
npm install
npm run check
npm run build:pdf
```

生成的 PDF 位于 `docs/visual-platform-core-technical-design.pdf`。

## GitHub Pages

在仓库 Settings → Pages → Build and deployment → Source 中选择 **GitHub Actions**。之后推送到 `main` 会运行校验、生成 PDF 并发布 `docs/`。

> GitHub Pages 是公开静态托管，不是 Visual Platform 的企业私有内容运行环境。真实业务中的用户 Artifact 必须由私有部署的 Static Gateway 鉴权交付，不能直接放到 GitHub Pages。

## 工程状态

**M0 / M1 / M2 已有 Go 代码和 PostgreSQL 集成测试；M3-A 验证核心正在独立 PR 中，尚未合并或接入 Worker。项目仍不是生产可用的文件托管服务。**

- M0：Go API、健康检查、开发配置、CI。
- M1：Workspace / Artifact / RBAC / ACL。
- M2：Upload Session、Workspace Quota、流式上传、SHA-256 校验、私有 Quarantine 对象存储。
- M3-A：ZIP 与 standalone HTML Validation Core 已在 `feat/m3-zip-validation-core` 分支实现；Go / PostgreSQL CI 与 10 秒 Fuzz smoke 通过，PR #4 仍为 Draft；本地扩展 Fuzz 待执行。
- M3-B：独立 Quarantine Reader、ClamAV INSTREAM 扫描适配器及逐文件 Manifest 哈希复核已提交至 [Draft PR #5](https://github.com/1998x-stack/visual-platform/pull/5)，等待 CI 与真实 MinIO/ClamAV 集成验收；尚未接入 Worker。
- M3-C：验证状态数据库 Migration、source SHA-256 绑定、Manifest 与扫描证据入库、Worker 租约 fencing、失败分级重试和 PostgreSQL 并发测试已提交至 [Draft PR #6](https://github.com/1998x-stack/visual-platform/pull/6)，仍待独立验收，尚未连接生产 Worker。
- M4-A：真实 Go Worker 入口、Received Upload 补偿入队、私有 0700 Spool、下载 SHA-256 复核、HTML/ZIP 校验与逐文件 ClamAV 扫描、双重 VERSION 证据检测已提交至 [Draft PR #7](https://github.com/1998x-stack/visual-platform/pull/7)。CI 单元 / PostgreSQL 集成测试已运行；真实 MinIO/ClamAV 集成、安全限额与上线验收仍 **NOT RUN**。
- M4-B / M5+：真实存储/杀毒服务 E2E、租约续期/磁盘配额、不可变发布、Static Gateway、企业 OIDC 尚未实现。

详细状态见 [implementation-status.md](docs/implementation-status.md)。

## 本地开发

需要 Go 1.27.4、Docker Compose、PostgreSQL 客户端。上传功能还需要一个**预先创建、无公开 Bucket Policy** 的 S3 / MinIO Quarantine Bucket。

```bash
make dev-db
make migrate
make seed-dev
go mod tidy
cp .env.example .env
# 编辑 .env 中的 S3 endpoint / bucket / 专用凭据
set -a && source .env && set +a
make api-dev
```

如暂时没有 S3 兼容存储，API 的 M0/M1 功能仍可启动；上传接口会返回 `503 UPLOAD_STORAGE_NOT_CONFIGURED`。

## M2 上传接口

1. `POST /api/v1/artifacts/{artifact_id}/uploads`：创建上传会话，声明文件名、类型、大小和 SHA-256；服务端原子预留 Workspace 配额。
2. `PUT /api/v1/uploads/{upload_id}/content`：经 API 流式写入私有 Quarantine Bucket，服务端重新计算 SHA-256。
3. `GET /api/v1/uploads/{upload_id}`：查询上传状态。

创建会话必须带 `Idempotency-Key` 请求头（8–128 个 ASCII 字母、数字、点、下划线、冒号或连字符）。同一 Workspace / 创建者使用相同 Key 和请求参数重试时返回原 Session，不重复预留配额；复用 Key 但参数不同则返回冲突。

默认单文件上限 100 MiB；Workspace 默认容量 1 GiB、最多 5 个活动会话、会话 30 分钟过期。实际有效限制取 API `MAX_UPLOAD_BYTES` 与数据库 Workspace 配额中的较小值。

M2 只接收 HTML / ZIP 声明类型，**不信任客户端 MIME，也不解压 ZIP、不扫描、不生成发布 Manifest、不提供下载或公开 URL**。上传成功状态 `received` 仅表示字节已写入隔离区且 SHA-256 一致，不表示内容安全或可发布。

对象存储需配置 `S3_ENDPOINT`、`S3_ACCESS_KEY`、`S3_SECRET_KEY`、`S3_BUCKET`。Bucket 必须预先创建、保持私有且无显式 Bucket Policy；API 不会自动创建 Bucket，也不会生成预签名 URL。请给 API 配置仅限 Quarantine Bucket 的最小权限凭据。非 development 环境强制 S3_USE_SSL=true。

## M4-A 验证 Worker（实验性，尚未生产验收）

Worker 从数据库领取 `received` 上传的验证任务，下载到私有临时文件，复核完整源 SHA-256，运行 ZIP/HTML Validator 与 ClamAV INSTREAM 扫描。**任何扫描通过都不会自动发布或对外提供 URL。**

`make migrate` 已自动应用包括 0004 在内的所有非 Seed Migration；可运行 `make worker-once` 做一次迭代，或运行 `make worker-dev` 启动单消费者循环。运行前必须配置 `DATABASE_URL`、单独的 `WORKER_S3_ACCESS_KEY` / `WORKER_S3_SECRET_KEY`、`S3_ENDPOINT`、`S3_BUCKET`、`CLAMD_SOCKET` 和用户独占的 `WORKER_TEMP_DIR`（0700 权限）。完整配置和门槛见 [M4-A 技术设计](docs/m4a-worker-pipeline-spec.md) 与 [Worker 环境变量模板](deploy/worker.env.example)。

真实 MinIO Get-only 策略、真实 ClamAV 病毒与超限扫描、数据库签名时间解释及磁盘压力保护仍需独立验收。该入口**不得作为生产部署已完成的依据**。

## GitHub Pages

GitHub Pages 仅托管公开技术文档，不用于存储或发布企业员工 Artifact。真实用户内容只能由后续完成鉴权的私有 Static Gateway 提供。
