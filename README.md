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
npm ci
npm run check
npm run build:pdf
```

生成的 PDF 位于 `docs/visual-platform-core-technical-design.pdf`。

## GitHub Pages

在仓库 Settings → Pages → Build and deployment → Source 中选择 **GitHub Actions**。之后推送到 `main` 会运行校验、生成 PDF 并发布 `docs/`。

> GitHub Pages 是公开静态托管，不是 Visual Platform 的企业私有内容运行环境。真实业务中的用户 Artifact 必须由私有部署的 Static Gateway 鉴权交付，不能直接放到 GitHub Pages。

## 项目状态

当前仓库用于沉淀产品方案、架构决策、接口契约、威胁模型、测试标准与可复现的设计文档构建流程。本文档不声称已有生产代码、企业 SSO、对象存储或访问控制已经部署。
