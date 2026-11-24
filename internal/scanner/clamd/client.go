// Package clamd implements ClamAV's INSTREAM protocol over a local Unix socket.
package clamd

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/1998x-stack/visual-platform/internal/scanner"
)

const (
	defaultMaxStreamBytes int64 = 64 << 20
	defaultTimeout              = 45 * time.Second
	chunkBytes                  = 64 << 10
	maxReplyBytes               = 1024
)

// Config limits the data sent to clamd. MaxStreamBytes must not exceed the
// daemon's StreamMaxLength; all clamd scan limits must be enforced separately.
type Config struct {
	SocketPath     string
	Timeout        time.Duration
	MaxStreamBytes int64
}

// Client is a deliberately small clamd client: no TCP or remote socket support.
type Client struct {
	socketPath     string
	timeout        time.Duration
	maxStreamBytes int64
}

// New validates the scanner's local-only connection and resource configuration.
func New(cfg Config) (*Client, error) {
	if !filepath.IsAbs(cfg.SocketPath) || strings.ContainsRune(cfg.SocketPath, 0) {
		return nil, errors.New("clamd requires an absolute Unix socket path")
	}
	if cfg.Timeout < 0 || cfg.MaxStreamBytes < 0 {
		return nil, errors.New("clamd scanner limits must not be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxStreamBytes == 0 {
		cfg.MaxStreamBytes = defaultMaxStreamBytes
	}
	return &Client{
		socketPath:     cfg.SocketPath,
		timeout:        cfg.Timeout,
		maxStreamBytes: cfg.MaxStreamBytes,
	}, nil
}

// Scan sends exactly declaredSize bytes to clamd and accepts only an explicit
// "stream: OK" terminal response as clean. Incomplete or ambiguous replies fail
// closed. FOUND is reported as an infected verdict, not a clean result.
func (c *Client) Scan(ctx context.Context, source io.Reader, declaredSize int64) (scanner.Result, error) {
	if c == nil || source == nil {
		return scanner.Result{}, errors.New("clamd scanner or source is missing")
	}
	if ctx == nil {
		return scanner.Result{}, errors.New("clamd scan context is required")
	}
	if err := ctx.Err(); err != nil {
		return scanner.Result{}, err
	}
	if declaredSize < 0 || declaredSize > c.maxStreamBytes {
		return scanner.Result{}, fmt.Errorf("clamd stream size exceeds configured limit")
	}

	dialer := &net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return scanner.Result{}, fmt.Errorf("connect to local clamd: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(c.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return scanner.Result{}, fmt.Errorf("set clamd deadline: %w", err)
	}

	// Closing the socket interrupts a blocked Read or Write on cancellation.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	if _, err := io.WriteString(conn, "zINSTREAM\x00"); err != nil {
		return scanner.Result{}, fmt.Errorf("send clamd command: %w", err)
	}

	chunk := make([]byte, chunkBytes)
	var header [4]byte
	remaining := declaredSize
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return scanner.Result{}, err
		}
		next := int64(len(chunk))
		if remaining < next {
			next = remaining
		}
		n, readErr := io.ReadFull(source, chunk[:int(next)])
		if readErr != nil {
			return scanner.Result{}, fmt.Errorf("incomplete scan input: %w", readErr)
		}
		binary.BigEndian.PutUint32(header[:], uint32(n))
		if _, err := conn.Write(header[:]); err != nil {
			return scanner.Result{}, fmt.Errorf("send clamd chunk length: %w", err)
		}
		if _, err := conn.Write(chunk[:n]); err != nil {
			return scanner.Result{}, fmt.Errorf("send clamd chunk: %w", err)
		}
		remaining -= int64(n)
	}

	// A manifest length mismatch must not turn into a clean scan.
	var extra [1]byte
	n, readErr := source.Read(extra[:])
	if n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return scanner.Result{}, errors.New("scan input does not match its declared size")
	}
	if readErr == nil {
		return scanner.Result{}, errors.New("scan input did not confirm EOF")
	}

	// INSTREAM is terminated by a zero-length chunk, never the last chunk's length.
	binary.BigEndian.PutUint32(header[:], 0)
	if _, err := conn.Write(header[:]); err != nil {
		return scanner.Result{}, fmt.Errorf("finish clamd stream: %w", err)
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, maxReplyBytes)).ReadString(0)
	if err != nil {
		return scanner.Result{}, fmt.Errorf("read clamd terminal reply: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return scanner.Result{}, err
	}
	if reply == "stream: OK\x00" {
		return scanner.Result{Verdict: scanner.VerdictClean}, nil
	}
	if strings.HasPrefix(reply, "stream: ") && strings.HasSuffix(reply, " FOUND\x00") {
		signature := strings.TrimSuffix(strings.TrimPrefix(reply, "stream: "), " FOUND\x00")
		if signature != "" && !strings.ContainsRune(signature, 0) {
			return scanner.Result{Verdict: scanner.VerdictInfected, Signature: signature}, nil
		}
	}
	return scanner.Result{}, fmt.Errorf("clamd returned a non-clean or malformed terminal response")
}

// Ping checks whether the local clamd daemon responds to the documented PING
// protocol. It does not establish signature freshness or scan completeness.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || ctx == nil {
		return errors.New("clamd client or context is missing")
	}
	conn, err := (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("connect to clamd: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(c.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "zPING\x00"); err != nil {
		return err
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, maxReplyBytes)).ReadString(0)
	if err != nil || reply != "PONG\x00" {
		return errors.New("clamd did not return PONG")
	}
	return ctx.Err()
}

var _ scanner.Scanner = (*Client)(nil)
