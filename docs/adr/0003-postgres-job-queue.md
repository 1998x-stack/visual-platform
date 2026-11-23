# ADR-0003：MVP 使用 PostgreSQL Job Queue

- Status: Accepted for MVP
- Date: 2026-11-23

## Context

上传后的 ZIP 校验、扫描、Manifest 生成和发布是异步任务。MVP 尚无证据表明必须引入 Kafka 或 Redis。内存 Channel 在进程重启后不能可靠恢复任务。

## Decision

1. Job 状态保存在 PostgreSQL。
2. Worker 使用 SELECT ... FOR UPDATE SKIP LOCKED 领取任务。
3. Job 具有 lease_until、worker_id、attempt_count、next_retry_at、idempotency_key。
4. Lease 过期允许其他 Worker 重新领取。
5. 业务状态与 Outbox Event 在同一数据库事务中提交。
6. Consumer 按 event_id 幂等，所有状态迁移写审计。

## Consequences

- PostgreSQL 是队列和业务元数据的共同依赖，需监控队列积压和锁竞争。
- 高吞吐、长任务和多消费者场景可能需要迁移到独立队列。
- 通过接口封装 JobRepository，避免业务模块绑定具体队列实现。

## Validation

- Worker 被 kill 后任务能在 Lease 过期后重新执行。
- 同一 Job 被重复消费不会重复发布或重复扣减配额。
- 数据库事务回滚时，不产生已提交的 Outbox Event。
