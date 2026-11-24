package clamd

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func serveVersion(t *testing.T, reply string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clamd-version.sock")
	listener, err := net.Listen("unix", path)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil { return }
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3*time.Second))
		command, err := bufio.NewReader(conn).ReadString(0)
		if err != nil || command != "zVERSION\x00" { return }
		_, _ = io.WriteString(conn, reply)
	}()
	return path
}

func TestParseVersionAcceptsFreshDatabase(t *testing.T) {
	now := time.Date(2026,11,24,11,0,0,0,time.UTC)
	evidence,err:=parseVersion("ClamAV 1.4.3/27777/Tue Nov 24 10:00:00 2026",now)
	if err!=nil { t.Fatal(err) }
	if evidence.EngineVersion!="clamav-1.4.3" ||
		evidence.SignatureVersion!="db-27777" ||
		evidence.SignaturesUpdatedAt.UTC().Hour()!=10 {
		t.Fatalf("unexpected VERSION evidence: %+v",evidence)
	}
}

func TestParseVersionFailsClosed(t *testing.T) {
	now:=time.Date(2026,11,24,11,0,0,0,time.UTC)
	for _, version := range []string{
		"COMMAND UNAVAILABLE",
		"ClamAV 1.4.3/0/Tue Nov 24 10:00:00 2026",
		"ClamAV 1.4.3/27777/Fri Nov 20 10:00:00 2026",
		"ClamAV 1.4.3/27777/Tue Nov 24 12:00:00 2026",
		"ClamAV 1.4.3/27777/not-a-date",
		"ClamAV 1.4.3/27777/Tue Nov 24 10:00:00 2026\nextra",
		"other 1.4.3/27777/Tue Nov 24 10:00:00 2026",
		"",
	} {
		if _,err:=parseVersion(version,now);err==nil {
			t.Errorf("accepted invalid VERSION %q",version)
		}
	}
}

func TestProbeVersionUnixSocket(t *testing.T) {
	now:=time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	response:="ClamAV 1.4.3/27777/"+now.Format("Mon Jan _2 15:04:05 2006")+"\x00"
	socket:=serveVersion(t,response)
	c,err:=New(Config{SocketPath:socket,Timeout:time.Second})
	if err!=nil { t.Fatal(err) }
	evidence,err:=c.Probe(context.Background())
	if err!=nil { t.Fatal(err) }
	if evidence.EngineVersion!="clamav-1.4.3" || evidence.SignatureVersion!="db-27777" {
		t.Fatalf("unexpected daemon evidence %+v",evidence)
	}
}

func TestProbeRejectsMalformedOrTruncatedResponse(t *testing.T) {
	for _, reply:=range []string{"COMMAND UNAVAILABLE\x00","ClamAV 1.4.3/27777/invalid\x00","ClamAV 1.4.3/27777/"} {
		t.Run(strings.ReplaceAll(reply,"\x00","NUL"),func(t *testing.T) {
			socket:=serveVersion(t,reply)
			c,err:=New(Config{SocketPath:socket,Timeout:time.Second})
			if err!=nil { t.Fatal(err) }
			if _,err:=c.Probe(context.Background());err==nil { t.Fatalf("accepted %q",reply) }
		})
	}
}
