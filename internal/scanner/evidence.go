package scanner

import (
	"context"
	"time"
)

// Evidence identifies the engine and signature database checked by a scanner.
// It is metadata, not proof that a scan completed or detected every threat.
type Evidence struct {
	EngineVersion      string
	SignatureVersion   string
	SignaturesUpdatedAt time.Time
}

// EvidenceScanner provides authenticated local-daemon metadata as well as
// streaming per-file scan verdicts. A worker must check before and after a job.
type EvidenceScanner interface {
	Scanner
	Probe(ctx context.Context) (Evidence, error)
}
