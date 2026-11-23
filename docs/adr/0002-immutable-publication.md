# ADR-0002：不可变版本与数据库指针发布

- Status: Accepted for MVP
- Date: 2026-11-23

## Context

PostgreSQL 与 MinIO / S3 不共享 ACID 事务。若先更新线上 URL 指针，再写对象，发布中断会让线上资源指向不完整内容。若直接覆盖同一对象 Key，回滚和审计也变得困难。

## Decision

1. 每次发布生成新的 version_id 和唯一 object_prefix。
2. Worker 先写入完整对象集与 Manifest，校验对象数量、字节和 SHA-256。
3. 只有对象完整后，才在一个 PostgreSQL 事务中把 Version 标记 READY 并切换 Artifact.current_version_id。
4. 旧版本保持不可变；回滚只切换数据库指针。
5. 发布失败时保留旧指针；新前缀标记为孤儿对象，延迟清理。

## Consequences

- 需要 Manifest、孤儿对象清理和版本保留策略。
- 发布需要额外对象存储空间；必须设置版本保留和生命周期。
- 对象存储 Bucket Versioning 是底层保护，不能替代应用级 Version 模型。

## Validation

- 故障注入：对象写入失败、Manifest 校验失败、DB commit 失败时，线上仍指向旧版本。
- Worker 重试不会创建重复的可见版本。
- 回滚不修改历史对象内容。
