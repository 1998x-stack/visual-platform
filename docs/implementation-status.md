# M0/M1 实施状态与安全边界

日期：2026-11-23

## 本次范围

- M0：Go API 主进程、显式失败的 Worker / Gateway 入口、配置与结构化日志、健康与就绪检查、数据库迁移、开发环境及 CI。
- M1：Personal Workspace 创建/查询、成员角色授权、Artifact 创建/查询、用户 ACL、统一 Authorizer、数据库事务内二次授权和审计写入。
- 部门、项目和公司 Workspace 的 Schema 已预留；不允许普通用户通过 API 自行创建此类高权限范围。它们需要受控的企业组织配置流程。

## 功能验收

| 功能 | 状态 | 备注 |
|---|---|---|
| Go API 启动 /healthz /readyz | 实现 | 启动时必须连接 PostgreSQL |
| 企业 OIDC 登录 | 未实现 | 无配置时所有业务 API 返回 503，fail closed |
| 本地开发用户模拟 | 实现 | 仅 APP_ENV=development 且绑定 127.0.0.1，禁止透传代理 |
| 创建个人 Workspace | 实现 | Owner 与成员身份在同一事务内建立 |
| 部门 / 项目 / 公司工作区 | Schema 预留 | 普通用户 API 创建被拒绝；后续对接组织目录 |
| Workspace 成员与角色 | 实现 | Owner 可授予 Viewer / Editor / Maintainer；不能通过成员接口新增 Owner |
| Artifact 元数据与 Slug 唯一性 | 实现 | 当前只创建 draft，不发布静态资源 |
| Artifact user ACL | 实现 | M1 允许 user grants；group 模型预留，尚未接组织目录 |
| 权限决策引擎 | 实现 | 角色矩阵、过期 ACL、资源状态、停用用户、跨工作区 |
| 发布 / 回滚 | 未实现 | M3/M5 |
| ZIP 上传与恶意内容扫描 | 未实现 | M2/M3 |
| MinIO 正式对象写入 | 未实现 | M3 |
| Static Gateway | 未实现 | 进程直接退出，不提供不安全的文件服务 |
| 每个 Artifact 独立域名与 OIDC Ticket | 未实现 | M4/M5 |
| PDF / HTML 技术设计 | 已存在 | GitHub Pages 仍需独立确认 |
| Go 单元与 DB 集成 CI | 已配置 | 以 Actions 实际结果为准 |

## 特别注意

开发身份模拟使用 X-Dev-User-ID，仅适用于本机直连 API。任何外部用户都不应该能通过反向代理访问该机制。正式上线前必须实现企业 OIDC、撤权与会话管理；不要把此版本用于存储员工的真实敏感文件。

PostgreSQL RLS 本轮未开启。原因：没有成熟的事务级 app actor session context，贸然启用可能导致服务锁死，也可能因 DB owner 角色绕过而产生虚假安全感。本轮以服务器统一 Authorizer + 写事务内重新检查为主要控制。部署前必须使用独立非 owner、非 BYPASSRLS 的数据库身份进行安全审查。

## 开发启动

Go 1.27.4、Docker Compose、psql 客户端。

    make dev-db
    make migrate
    make seed-dev
    go mod tidy
    make api-dev

本机查询示例：

    curl -H 'X-Dev-User-ID: 00000000-0000-4000-8000-000000000001' http://127.0.0.1:8071/api/v1/me

创建个人 Workspace：

    curl -X POST http://127.0.0.1:8071/api/v1/workspaces \
      -H 'Content-Type: application/json' \
      -H 'X-Dev-User-ID: 00000000-0000-4000-8000-000000000001' \
      -d '{"kind":"personal","name":"Research"}'

API 的所有业务端点在 DEV_AUTH 未显式开启时拒绝使用。
