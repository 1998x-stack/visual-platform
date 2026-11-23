package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/1998x-stack/visual-platform/internal/domain"
)

func TestM1TransactionalIsolation(t *testing.T) {
	url:=os.Getenv("DB_TEST_URL")
	if url=="" {t.Skip("DB_TEST_URL not set; PostgreSQL integration test skipped")}
	db,err:=sql.Open("pgx",url)
	if err!=nil {t.Fatal(err)}
	defer db.Close()
	ctx,cancel:=context.WithTimeout(context.Background(),20*time.Second)
	defer cancel()
	if err:=db.PingContext(ctx);err!=nil{t.Fatal(err)}
	s:=&Store{DB:db}
	seedUser:=func(name string)string{
		t.Helper()
		var id string
		subject:=fmt.Sprintf("integration:%s:%d",name,time.Now().UnixNano())
		err:=db.QueryRowContext(ctx,
			`INSERT INTO users(oidc_subject,display_name) VALUES($1,$2) RETURNING id::text`,
			subject,name).Scan(&id)
		if err!=nil{t.Fatal(err)}
		return id
	}
	alice:=seedUser("alice")
	bob:=seedUser("bob")
	other:=seedUser("other")

	w,err:=s.CreatePersonalWorkspace(ctx,alice,"Research")
	if err!=nil{t.Fatal(err)}
	if w.Kind!="personal"||w.OwnerRef!=alice{t.Fatalf("incorrect workspace ownership: %+v",w)}
	if role,err:=s.Role(ctx,w.ID,alice);err!=nil||role!=domain.Owner{t.Fatalf("owner role=%s err=%v",role,err)}
	if got,err:=s.ListWorkspaces(ctx,bob);err!=nil||len(got)!=0{t.Fatalf("bob should have no workspace: %+v err=%v",got,err)}

	a,err:=s.CreateArtifact(ctx,w.ID,alice,"report-2026")
	if err!=nil{t.Fatal(err)}
	if a.CreatedBy!=alice||a.WorkspaceID!=w.ID||a.Status!="draft"{t.Fatalf("invalid artifact: %+v",a)}
	if _,err:=s.CreateArtifact(ctx,w.ID,bob,"intrusion");!errors.Is(err,ErrDenied){
		t.Fatalf("cross-workspace artifact creation must fail, got %v",err)
	}
	if _,err:=s.CreateArtifact(ctx,w.ID,alice,"report-2026");!errors.Is(err,ErrConflict){
		t.Fatalf("duplicate slug should be conflict, got %v",err)
	}
	if err:=s.AddMember(ctx,w.ID,bob,other,domain.Editor);!errors.Is(err,ErrDenied){
		t.Fatalf("non-owner cannot invite, got %v",err)
	}
	if err:=s.AddMember(ctx,w.ID,alice,bob,domain.Viewer);err!=nil{t.Fatal(err)}
	if _,err:=s.CreateArtifact(ctx,w.ID,bob,"viewer-cannot-create");!errors.Is(err,ErrDenied){
		t.Fatalf("viewer created an artifact: %v",err)
	}
	if err:=s.AddMember(ctx,w.ID,alice,bob,domain.Editor);err!=nil{t.Fatal(err)}
	if _,err:=s.CreateArtifact(ctx,w.ID,bob,"viewer-now-editor");err!=nil{t.Fatal(err)}
	if err:=s.AddMember(ctx,w.ID,alice,alice,domain.Viewer);!errors.Is(err,ErrDenied){
		t.Fatalf("owner membership must not be demoted by invite API, got %v",err)
	}
	if err:=s.UpsertUserGrant(ctx,a.ID,bob,other,domain.Viewer,nil);!errors.Is(err,ErrDenied){
		t.Fatalf("non-owner ACL write must fail, got %v",err)
	}
	exp:=time.Now().UTC().Add(time.Hour)
	if err:=s.UpsertUserGrant(ctx,a.ID,alice,other,domain.Viewer,&exp);err!=nil{t.Fatal(err)}
	grants,err:=s.Grants(ctx,a.ID)
	if err!=nil||len(grants)!=1||grants[0].SubjectID!=other||grants[0].ExpiresAt==nil{
		t.Fatalf("ACL mismatch: %+v %v",grants,err)
	}
	// A different personal workspace remains invisible to Alice/Bob.
	w2,err:=s.CreatePersonalWorkspace(ctx,other,"Other Research")
	if err!=nil{t.Fatal(err)}
	if role,err:=s.Role(ctx,w2.ID,alice);err!=nil||role!=""{
		t.Fatalf("cross workspace role leaked: %q %v",role,err)
	}
}
