package clamd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/1998x-stack/visual-platform/internal/scanner"
)

const (
	maxSignatureAge = 48 * time.Hour
	maxClockSkew    = 5 * time.Minute
)

var versionLine = regexp.MustCompile(`^ClamAV ([0-9][0-9A-Za-z.+-]{0,79})/([1-9][0-9]{0,9})/([A-Za-z0-9:,+ ._-]{10,80})$`)

// Probe checks clamd's VERSION response on the same Unix socket used for
// INSTREAM. Unknown reply formats or stale signatures must fail closed.
// The daemon's date has no timezone in common builds and is interpreted in
// UTC; deployments must confirm the daemon's timezone and DB metadata.
func (c *Client) Probe(ctx context.Context) (scanner.Evidence, error) {
	if c == nil || ctx == nil {
		return scanner.Evidence{}, errors.New("clamd client/context is missing")
	}
	if err := ctx.Err(); err != nil {
		return scanner.Evidence{}, err
	}
	conn, err := (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return scanner.Evidence{}, fmt.Errorf("connect clamd version socket: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(c.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return scanner.Evidence{}, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	if _, err := io.WriteString(conn, "zVERSION\x00"); err != nil {
		return scanner.Evidence{}, fmt.Errorf("request clamd version: %w", err)
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, maxReplyBytes)).ReadString(0)
	if err != nil || !strings.HasSuffix(reply, "\x00") {
		return scanner.Evidence{}, errors.New("incomplete clamd VERSION response")
	}
	if err := ctx.Err(); err != nil {
		return scanner.Evidence{}, err
	}
	return parseVersion(strings.TrimSuffix(reply, "\x00"), time.Now().UTC())
}

func parseVersion(line string, now time.Time) (scanner.Evidence, error) {
	parts := versionLine.FindStringSubmatch(line)
	if len(parts) != 4 {
		return scanner.Evidence{}, errors.New("unsupported clamd VERSION format")
	}
	// ClamAV's signature date is unzoned. Fail closed if no supported date
	// layout matches or the timestamp is outside the admission window.
	var databaseDate time.Time
	for _, layout := range []string{
		"Mon Jan _2 15:04:05 2006",
		"Mon Jan 02 15:04:05 2006",
		"Mon Jan _2 2006",
		"2006-01-02T15:04:05Z07:00",
	} {
		parsed, err := time.ParseInLocation(layout, strings.TrimSpace(parts[3]), time.UTC)
		if err == nil {
			databaseDate = parsed.UTC()
			break
		}
	}
	if databaseDate.IsZero() ||
		databaseDate.Before(now.Add(-maxSignatureAge)) ||
		databaseDate.After(now.Add(maxClockSkew)) {
		return scanner.Evidence{}, errors.New("clamd signature timestamp invalid or stale")
	}
	return scanner.Evidence{
		EngineVersion:       "clamav-" + parts[1],
		SignatureVersion:    "db-" + parts[2],
		SignaturesUpdatedAt: databaseDate,
	}, nil
}

var _ scanner.EvidenceScanner = (*Client)(nil)
