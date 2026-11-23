package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/1998x-stack/visual-platform/internal/httpapi"
	"github.com/1998x-stack/visual-platform/internal/store/postgres"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout,nil))
	var listen string
	flag.StringVar(&listen,"listen",getenv("LISTEN_ADDR","127.0.0.1:8071"),"HTTP bind address")
	flag.Parse()
	dbURL:=os.Getenv("DATABASE_URL")
	if strings.TrimSpace(dbURL)=="" {
		log.Error("DATABASE_URL is required");os.Exit(2)
	}
	devAuth:=os.Getenv("DEV_AUTH")=="true"
	if devAuth {
		host,_,err:=net.SplitHostPort(listen)
		ip:=net.ParseIP(host)
		if err!=nil||ip==nil||!ip.IsLoopback()||os.Getenv("APP_ENV")!="development" {
			log.Error("DEV_AUTH requires APP_ENV=development and an explicit loopback LISTEN_ADDR")
			os.Exit(2)
		}
	}
	db,err:=sql.Open("pgx",dbURL)
	if err!=nil {log.Error("opening database","error",err);os.Exit(1)}
	defer db.Close()
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30*time.Minute)
	ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
	err=db.PingContext(ctx)
	cancel()
	if err!=nil {log.Error("database not ready","error",err);os.Exit(1)}
	handler:=(&httpapi.Server{Repo:&postgres.Store{DB:db},Logger:log,DevAuth:devAuth}).Handler()
	srv:=&http.Server{
		Addr:listen,Handler:handler,
		ReadHeaderTimeout:5*time.Second,ReadTimeout:10*time.Second,
		WriteTimeout:15*time.Second,IdleTimeout:60*time.Second,
		MaxHeaderBytes:1<<20,
	}
	stop,stopSignals:=signal.NotifyContext(context.Background(),syscall.SIGINT,syscall.SIGTERM)
	defer stopSignals()
	go func(){
		<-stop.Done()
		ctx,cancel:=context.WithTimeout(context.Background(),10*time.Second)
		defer cancel()
		if err:=srv.Shutdown(ctx);err!=nil {log.Error("shutdown","error",err)}
	}()
	log.Info("api starting","listen",listen,"development_auth",devAuth)
	if err:=srv.ListenAndServe();err!=nil&&!errors.Is(err,http.ErrServerClosed){
		log.Error("api server","error",err);os.Exit(1)
	}
	log.Info("api stopped")
}
func getenv(k,def string)string{if v:=os.Getenv(k);v!=""{return v};return def}
