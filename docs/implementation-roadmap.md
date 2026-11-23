# Visual Platform · Engineering Roadmap

版本：v1.1 · 2026-11-24

## 产品交付目标

形成一个企业内部静态可视化发布闭环：员工上传 HTML / ZIP → 绑定 Workspace → 校验与扫描 → 不可变版本发布 → 生成稳定 URL → 按企业身份和 ACL 访问 → 可撤权、回滚和审计。

## 阶段与退出标准

| 阶段 | 研发范围 | 关键交付 | Exit Criteria |
|---|---|---|---|
| M0 工程骨架 | Go API / Worker / Gateway 入口、配置、日志、迁移、Compose | 可启动的工程骨架 | health / ready、DB、MinIO 连通；CI 通过 |
| M1 领域与权限 | User、Workspace、Member、Artifact、ACL、Authorizer | 数据模型与权限服务 | 完成 Owner / Maintainer / Editor / Viewer 正反向矩阵 |
| M2 上传隔离 | Upload Session、Workspace Quota、checksum、私有 Quarantine | 可恢复上传会话 | **已实现**：越权、配额、长度、checksum、lease 测试通过；M4 janitor 尚未实现 |
| M3 ZIP Validator | M3-A ZIP / standalone HTML 纯验证核心；M3-B Quarantine Reader / Scanner；M3-C 状态与 Manifest 集成 | 无磁盘解压的结构验证、CRC / SHA-256、确定性 Manifest、扫描结果 | 攻击样本与 Fuzz；扫描不可用 Fail Closed；验证结果可审计；任何未通过内容不能发布 |
| M4 Job / Worker | PostgreSQL Job、SKIP LOCKED、Lease、Retry、Outbox | 可恢复后台处理 | Worker kill/restart 后任务恢复；重复消费幂等 |
| M5 Publisher | 不可变对象前缀、Manifest 校验、current_version_id | 原子发布和回滚 | 故障注入不破坏旧版本；回滚只切指针 |
| M6 Static Gateway | Host Mapping、路径规范化、MIME、缓存、CSP | 安全静态访问 | 所有 GET / HEAD 请求授权；对象 Bucket 不公开 |
| M7 SSO / Organization | OIDC、PKCE、Ticket Exchange、部门 / 项目成员同步 | 企业登录和跨 Origin 会话 | Ticket 重放、跨 Artifact 兑换、跨租户访问测试通过 |
| M8 生产化 | 监控、告警、备份、PITR、配额、限流、恢复演练 | 生产部署基线 | P0 安全门槛通过；恢复演练完成 |
| M9 OpenCode Web | Publish action、Workspace picker、上传状态、URL 回写 | 原生 IDE 发布体验 | 不需要外部套壳；平台 API 是唯一权限真相源 |
| M10 Dynamic Runtime | Runtime Controller、容器 / microVM、网络隔离、资源配额 | 动态 App 托管 | 独立威胁模型、沙箱逃逸测试、成本和调度评估 |

## 推荐 PR 切分

1. PR-01：工程骨架、配置、结构化日志、健康检查、migration runner。
2. PR-02：Workspace / Artifact / Version / ACL schema 与 repository。
3. PR-03：统一 Authorizer、权限矩阵和负向测试。
4. PR-04：Upload Session、Quarantine、配额、幂等键（已实现，见 M2）。
5. PR-05A：无磁盘解压的 ZIP Validation Core、Manifest 与攻击样本（当前 M3-A）。
6. PR-05B：Worker 专用 Quarantine Reader、ClamAV Scanner Adapter 与扫描超时 / 递归 / 资源限制。
7. PR-05C：Validation State、Manifest 持久化、审计与失败 / 重试语义。
8. PR-06：Job Queue、Lease / Retry、Outbox、Worker，并将 M3-B/C 接入异步处理。
9. PR-07：不可变 Publisher、原子切换、回滚、孤儿对象清理。
10. PR-08：Static Gateway、Host Mapping、路径规范化、响应头。
11. PR-09：OIDC Broker、PKCE、Ticket Exchange、Host-only Cookie。
12. PR-10：审计、指标、备份、恢复与安全验收。
13. PR-11：OpenCode Web 发布入口与 CLI。
14. PR-12：动态运行时独立方案与 PoC（不与静态 MVP 混合）。

## P0 上线阻断项

- 任意跨 Workspace / 跨部门越权。
- MinIO Quarantine / Published Bucket 可匿名访问。
- ZIP Slip、Symlink、ZIP Bomb 未被正确限制。
- 用户 Artifact 可读取管理平台 Cookie、DOM 或 Web Storage。
- 未通过校验 / 扫描的文件能从正式 Host 访问。
- 发布失败导致 current_version_id 指向不完整版本。
- ACL 撤销后 Gateway 仍从共享缓存返回内容。
- Worker 重启导致任务丢失或重复发布产生不可控副作用。
- 数据库和对象存储无法恢复到一致状态。

## 容量与 SLO 方法

先建立真实样本集：单页 HTML、带图表 JS 的报告、含 10–50 个静态资源的 ZIP、最大尺寸 ZIP。按并发 10 / 50 / 100 逐级压测。至少记录：

- 上传成功率、上传 P95、Quarantine 对象写入延迟。
- ZIP 解压 CPU / RSS / 耗时 / 解压字节 / 文件数。
- Job queue lag、重试率、扫描失败率、发布成功率。
- Gateway 401 / 403 / 404 / 5xx、授权 P95、对象存储首字节延迟。
- 下载出口带宽、对象数量、租户配额使用率和孤儿对象字节数。

性能阈值必须由实际部署硬件、网络、用户规模和内容体量确定，不以未经压测的 QPS 作为承诺。

## 上线前运营准备

- 确定企业 OIDC Issuer、稳定 Subject 字段、部门与项目成员数据来源。
- 确定管理域与内容域的 DNS / TLS 归属；优先使用不同 eTLD+1。
- 确定 MinIO / AIStor 实际发行版、许可证、升级和备份支持。
- 确定部门 Owner、离职资产移交、保留期和删除审批流程。
- 确定安全扫描器、误报复核和违规内容处置流程。
- 完成 PITR / MinIO 恢复演练和权限撤销演练。


## M3-A 初始验证策略（2026-11-24）

- ZIP 不执行文件系统解压；从有大小上限的随机访问源逐项读取并生成 Manifest。单 HTML 直接以有大小上限的 ReaderAt 计算 SHA-256 并生成相同 Manifest 结构。
- 默认边界：ZIP 100 MiB、中央目录 8 MiB、条目 5,000、文件 4,000、单文件解压 64 MiB、总解压 512 MiB、压缩比 100、路径 1,024 bytes / 32 层。
- MVP 拒绝 ZIP64、多卷 ZIP、加密条目、Symlink / 特殊文件、Store / Deflate 以外压缩方式、服务端执行扩展名和嵌套归档扩展名。
- 根入口必须是精确小写 `index.html`；允许剥离唯一公共顶层目录。
- Manifest 只描述内容，不表示内容可信、扫描通过或获得发布权限。
- 真实阈值需通过常见 Vite / Next export / Python 静态报告等真实构建产物测试后再调整；不以未经测量的阈值承诺容量。
