// Package scanner provides fail-closed interfaces for untrusted artifact scans.
package scanner

import (
	"context"
	"io"
)

// Verdict describes an explicit scanner response. The zero value is never clean.
type Verdict string

const (
	VerdictClean    Verdict = "clean"
	VerdictInfected Verdict = "infected"
)

// Result is returned only after a scanner has delivered a complete terminal reply.
type Result struct {
	Verdict   Verdict
	Signature string
}

// Scanner reads exactly the declared number of bytes and rejects short or
// oversized streams. Callers must not interpret a nil error alone as clean.
type Scanner interface {
	Scan(ctx context.Context, source io.Reader, declaredSize int64) (Result, error)
}
