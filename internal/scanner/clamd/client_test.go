package clamd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1998x-stack/visual-platform/internal/scanner"
)

func fakeClamd(t *testing.T, reply string) (string, <-chan []byte) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "clamd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on fake clamd socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan []byte, 1)
	go func() {
		defer close(received)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		command, err := reader.ReadString(0)
		if err != nil {
			return
		}
		if command == "zPING\x00" {
			_, _ = io.WriteString(conn, reply)
			received <- nil
			return
		}
		if command != "zINSTREAM\x00" {
			return
		}
		var data bytes.Buffer
		for {
			var size uint32
			if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
				return
			}
			if size == 0 {
				break
			}
			if size > 1<<20 {
				return
			}
			if _, err := io.CopyN(&data, reader, int64(size)); err != nil {
				return
			}
		}
		received <- data.Bytes()
		_, _ = io.WriteString(conn, reply)
	}()
	return socket, received
}

func TestScanAcceptsOnlyExplicitCleanReply(t *testing.T) {
	socket, received := fakeClamd(t, "stream: OK\x00")
	client, err := New(Config{SocketPath: socket, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("safe", 50000))
	result, err := client.Scan(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Scan() error: %v", err)
	}
	if result.Verdict != scanner.VerdictClean || result.Signature != "" {
		t.Fatalf("unexpected clean verdict: %+v", result)
	}
	if got := <-received; !bytes.Equal(got, data) {
		t.Fatalf("stream bytes differ, got %d want %d", len(got), len(data))
	}
}

func TestScanReturnsInfectedVerdict(t *testing.T) {
	socket, received := fakeClamd(t, "stream: Eicar-Test-Signature FOUND\x00")
	client, err := New(Config{SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Scan(context.Background(), strings.NewReader("malicious"), 9)
	if err != nil {
		t.Fatalf("Scan() error: %v", err)
	}
	if result.Verdict != scanner.VerdictInfected || result.Signature != "Eicar-Test-Signature" {
		t.Fatalf("unexpected detection verdict: %+v", result)
	}
	<-received
}

func TestScanRejectsAllAmbiguousReplies(t *testing.T) {
	for _, reply := range []string{
		"stream: INSTREAM size limit exceeded. ERROR\x00",
		"stream: OK", "stream: OK\n", "OK\x00",
		"stream: \x00", "stream:  FOUND\x00",
	} {
		t.Run(strings.ReplaceAll(reply, "\x00", "NUL"), func(t *testing.T) {
			socket, received := fakeClamd(t, reply)
			client, err := New(Config{SocketPath: socket, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Scan(context.Background(), strings.NewReader("data"), 4)
			if err == nil || result.Verdict == scanner.VerdictClean {
				t.Fatalf("unexpectedly accepted reply %q: %+v, %v", reply, result, err)
			}
			<-received
		})
	}
}

func TestScanFailsClosedOnLengthMismatch(t *testing.T) {
	cases := []struct {
		name string
		body string
		size int64
	}{
		{name: "short", body: "abc", size: 4},
		{name: "extra", body: "abcd", size: 3},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			socket, _ := fakeClamd(t, "stream: OK\x00")
			client, err := New(Config{SocketPath: socket, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Scan(context.Background(), strings.NewReader(testCase.body), testCase.size)
			if err == nil || result.Verdict == scanner.VerdictClean {
				t.Fatalf("accepted bad length: %+v, %v", result, err)
			}
		})
	}
}

func TestScanRejectsOversizeBeforeConnection(t *testing.T) {
	client, err := New(Config{SocketPath: "/no/such/clamd.sock", MaxStreamBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Scan(context.Background(), strings.NewReader("hello"), 5); err == nil {
		t.Fatal("expected size limit rejection")
	}
}

func TestScanCancellationFailsClosed(t *testing.T) {
	client, err := New(Config{SocketPath: "/no/such/clamd.sock"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Scan(ctx, strings.NewReader("a"), 1); err == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestPingRequiresPONG(t *testing.T) {
	socket, received := fakeClamd(t, "PONG\x00")
	client, err := New(Config{SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping(): %v", err)
	}
	<-received
}

func TestNewRejectsNonLocalAndInvalidConfig(t *testing.T) {
	for _, cfg := range []Config{
		{SocketPath: "127.0.0.1:3310"},
		{SocketPath: "clamd.sock"},
		{SocketPath: "/tmp/clamd.sock", Timeout: -time.Second},
		{SocketPath: "/tmp/clamd.sock", MaxStreamBytes: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%+v) succeeded", cfg)
		}
	}
}
