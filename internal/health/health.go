// Package health holds the bridge's lightweight in-memory operational
// counters (Step 20, Designv9 §3.3). Counters are process-lifetime:
// they never reset except by restart, and the start time is exposed
// alongside every reading so a reset is interpretable. Durable health
// evidence — purge history, source registration, retained briefing
// outcomes — stays in the store; publisher delivery stats are read
// from the publisher, never duplicated here. All methods are nil-safe
// so tests and partial wiring can leave the counters unset.
package health

import (
	"sync/atomic"
	"time"
)

// Counters is the operational health evidence. Fields increment only.
type Counters struct {
	startedAt         time.Time
	ingestAccepted    atomic.Int64
	ingestDuplicates  atomic.Int64
	ingestRejected    atomic.Int64
	ingestSaturated   atomic.Int64
	providerCalls     atomic.Int64
	providerFallbacks atomic.Int64
	redactionBlocked  atomic.Int64
	writerSaturated   atomic.Int64
}

// New returns counters anchored at now.
func New() *Counters {
	return &Counters{startedAt: time.Now().UTC()}
}

// Snapshot is the JSON-ready reading: start time plus cumulative counts.
type Snapshot struct {
	StartedAt         time.Time `json:"started_at"`
	IngestAccepted    int64     `json:"ingest_accepted"`
	IngestDuplicates  int64     `json:"ingest_duplicates"`
	IngestRejected    int64     `json:"ingest_rejected"`
	IngestSaturated   int64     `json:"ingest_saturated"`
	ProviderCalls     int64     `json:"provider_calls"`
	ProviderFallbacks int64     `json:"provider_fallbacks"`
	RedactionBlocked  int64     `json:"redaction_blocked"`
	WriterSaturated   int64     `json:"writer_saturated"`
}

// Snapshot reads every counter at once.
func (c *Counters) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	return Snapshot{
		StartedAt:         c.startedAt,
		IngestAccepted:    c.ingestAccepted.Load(),
		IngestDuplicates:  c.ingestDuplicates.Load(),
		IngestRejected:    c.ingestRejected.Load(),
		IngestSaturated:   c.ingestSaturated.Load(),
		ProviderCalls:     c.providerCalls.Load(),
		ProviderFallbacks: c.providerFallbacks.Load(),
		RedactionBlocked:  c.redactionBlocked.Load(),
		WriterSaturated:   c.writerSaturated.Load(),
	}
}

// StartedAt reports the process anchor for reset interpretation.
func (c *Counters) StartedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.startedAt
}

// The Add methods below are nil-safe: unset counters drop the event.

func (c *Counters) AddAccepted() {
	if c != nil {
		c.ingestAccepted.Add(1)
	}
}

func (c *Counters) AddDuplicate() {
	if c != nil {
		c.ingestDuplicates.Add(1)
	}
}

func (c *Counters) AddRejected() {
	if c != nil {
		c.ingestRejected.Add(1)
	}
}

func (c *Counters) AddSaturated() {
	if c != nil {
		c.ingestSaturated.Add(1)
	}
}

func (c *Counters) AddProviderCall() {
	if c != nil {
		c.providerCalls.Add(1)
	}
}

func (c *Counters) AddProviderFallback() {
	if c != nil {
		c.providerFallbacks.Add(1)
	}
}

func (c *Counters) AddRedactionBlocked() {
	if c != nil {
		c.redactionBlocked.Add(1)
	}
}

func (c *Counters) AddWriterSaturated() {
	if c != nil {
		c.writerSaturated.Add(1)
	}
}
