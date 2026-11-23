package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
)

type fakeRepo struct{ active bool }
func(f *fakeRepo)Ping(ctx context.Context)error{return nil}
func(f *fakeRepo)User(ctx context.Context,id string)(domain.User,error){
	status:="disabled";if f.active{status="active"}
	return domain.User{ID:id,DisplayName:"Developer",Status:status},nil
}
func(f *fakeRepo)CreatePersonalWorkspace(context.Context,string,string)(domain.Workspace,error){return domain.Workspace{},nil}
func(f *fakeRepo)ListWorkspaces(context.Context,string)([]domain.Workspace,error){return nil,nil}
func(f *fakeRepo)Workspace(context.Context,string)(domain.Workspace,error){return domain.Workspace{},nil}
func(f *fakeRepo)Role(context.Context,string,string)(domain.Role,error){return "",nil}
func(f *fakeRepo)Members(context.Context,string)([]domain.Member,error){return nil,nil}
func(f *fakeRepo)AddMember(context.Context,string,string,string,domain.Role)error{return nil}
func(f *fakeRepo)CreateArtifact(context.Context,string,string,string)(domain.Artifact,error){return domain.Artifact{},nil}
func(f *fakeRepo)Artifact(context.Context,string)(domain.Artifact,error){return domain.Artifact{},nil}
func(f *fakeRepo)ListArtifacts(context.Context,string,bool)([]domain.Artifact,error){return nil,nil}
func(f *fakeRepo)Grants(context.Context,string)([]domain.Grant,error){return nil,nil}
func(f *fakeRepo)UpsertUserGrant(context.Context,string,string,string,domain.Role,*time.Time)error{return nil}

func TestBusinessRoutesFailClosedWithoutSSO(t *testing.T) {
	h:=(&Server{Repo:&fakeRepo{active:true}}).Handler()
	req:=httptest.NewRequest(http.MethodGet,"/api/v1/me",nil)
	req.RemoteAddr="127.0.0.1:1234"
	rec:=httptest.NewRecorder()
	h.ServeHTTP(rec,req)
	if rec.Code!=http.StatusServiceUnavailable {t.Fatalf("status=%d",rec.Code)}
	if !strings.Contains(rec.Body.String(),"IDENTITY_PROVIDER_NOT_CONFIGURED") {t.Fatal(rec.Body.String())}
}

func TestDevAuthOnlyDirectLoopback(t *testing.T) {
	h:=(&Server{Repo:&fakeRepo{active:true},DevAuth:true}).Handler()
	const id="00000000-0000-4000-8000-000000000001"
	for _,tc:=range []struct{name,remote,forwarded string;want int}{
		{"local","127.0.0.1:1111","",http.StatusOK},
		{"remote","10.0.0.2:1111","",http.StatusForbidden},
		{"proxy","127.0.0.1:1111","203.0.113.1",http.StatusForbidden},
	}{
		t.Run(tc.name,func(t *testing.T){
			req:=httptest.NewRequest(http.MethodGet,"/api/v1/me",nil)
			req.RemoteAddr=tc.remote
			req.Header.Set("X-Dev-User-ID",id)
			if tc.forwarded!="" {req.Header.Set("X-Forwarded-For",tc.forwarded)}
			rec:=httptest.NewRecorder()
			h.ServeHTTP(rec,req)
			if rec.Code!=tc.want {t.Fatalf("status=%d body=%s",rec.Code,rec.Body.String())}
		})
	}
}
func TestInvalidWorkspaceKindNeverCreated(t *testing.T) {
	h:=(&Server{Repo:&fakeRepo{active:true},DevAuth:true}).Handler()
	req:=httptest.NewRequest(http.MethodPost,"/api/v1/workspaces",strings.NewReader(`{"kind":"company","name":"All Staff"}`))
	req.RemoteAddr="127.0.0.1:1111"
	req.Header.Set("X-Dev-User-ID","00000000-0000-4000-8000-000000000001")
	req.Header.Set("Content-Type","application/json")
	rec:=httptest.NewRecorder()
	h.ServeHTTP(rec,req)
	if rec.Code!=http.StatusForbidden {t.Fatal(rec.Code,rec.Body.String())}
	var data map[string]any
	if err:=json.Unmarshal(rec.Body.Bytes(),&data);err!=nil{t.Fatal(err)}
}
