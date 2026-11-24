package postgres

import (
	"context"
	"errors"
	"fmt"
)

// EnqueueReceivedBatch reconciles M2's durable received status with the M3-C
// validation queue. It is safe after API crashes, concurrency and retries.
// This does NOT change upload_sessions or public artifact status.
func (s *Store) EnqueueReceivedBatch(ctx context.Context, limit int) (int, error) {
	if s == nil || s.DB == nil || ctx == nil {
		return 0, errors.New("validation database is not configured")
	}
	if limit <= 0 || limit > 100 {
		return 0, errors.New("validation batch size must be in 1..100")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		WITH candidate AS (
		  SELECT u.id,u.workspace_id,u.artifact_id,u.source_type,
		    u.actual_size,u.actual_sha256
		  FROM upload_sessions u
		  LEFT JOIN validation_runs v ON v.upload_id=u.id
		  WHERE u.status='received' AND v.id IS NULL
		    AND u.actual_size=u.expected_size
		    AND u.actual_sha256=u.expected_sha256
		  ORDER BY u.received_at,u.id
		  FOR UPDATE OF u SKIP LOCKED LIMIT $1
		)
		INSERT INTO validation_runs(upload_id,workspace_id,artifact_id,
		  source_type,source_size,source_sha256)
		SELECT id,workspace_id,artifact_id,source_type,actual_size,actual_sha256
		FROM candidate
		ON CONFLICT(upload_id) DO NOTHING
		RETURNING id::text`, limit)
	if err != nil {
		return 0, fmt.Errorf("reconcile received uploads: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range ids {
		if err := appendValidationAudit(ctx, tx, "validation.enqueue", id,
			map[string]any{"source": "received_reconciliation"}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

