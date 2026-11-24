package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/1998x-stack/visual-platform/internal/domain"
)

// This exercises the M4 repair path for an M2 receipt that existed before
// a Worker started, without assuming that M2 writes to the validation table.
func TestM4ReconcileReceivedUploadsIdempotent(t *testing.T) {
	f := newValidationFixture(t)
	uploadID := f.received(t, "m4-reconcile-0001", []byte("<html>unqueued</html>"))
	var before int
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM validation_runs WHERE upload_id=$1::uuid`, uploadID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 { t.Fatalf("upload was pre-queued unexpectedly: %d",before) }
	first,err:=f.store.EnqueueReceivedBatch(f.ctx,100)
	if err!=nil || first<1 { t.Fatalf("no received upload reconciled: %d %v",first,err) }
	second,err:=f.store.EnqueueReceivedBatch(f.ctx,100)
	if err!=nil || second!=0 { t.Fatalf("reconcile must be idempotent: %d %v",second,err) }
	var status string
	var found int
	if err:=f.db.QueryRowContext(f.ctx,
		`SELECT status FROM validation_runs WHERE upload_id=$1::uuid`,uploadID).Scan(&status);err!=nil {
		t.Fatal(err)
	}
	if status!=string(domain.ValidationPending) { t.Fatalf("wrong reconciliation status %q",status) }
	if err:=f.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM validation_runs WHERE upload_id=$1::uuid`,uploadID).Scan(&found);err!=nil || found!=1 {
		t.Fatalf("upload duplicated on replay: %d %v",found,err)
	}
}

func TestM4ReconcileBatchRejectsInvalidLimits(t *testing.T) {
	f:=newValidationFixture(t)
	for _,limit:=range []int{-1,0,101} {
		if _,err:=f.store.EnqueueReceivedBatch(f.ctx,limit);err==nil {
			t.Fatalf("accepted batch limit %d",limit)
		}
	}
	if _,err:=f.store.EnqueueReceivedBatch(context.Background(),1);err!=nil {
		t.Fatalf("empty valid batch should work: %v",err)
	}
	if _,err:=f.store.ClaimNextValidation(f.ctx);err!=nil && !errors.Is(err,ErrNotFound) {
		t.Fatalf("invalid queue state after reconcile: %v",err)
	}
}
