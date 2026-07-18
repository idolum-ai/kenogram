package proxy

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultDiagnosticLimit = 64
	MaxDiagnosticLimit     = 256
	DefaultDiagnosticBytes = 16 << 10
	MaxDiagnosticBytes     = 64 << 10
)

// NetworkDiagnostic is deliberately limited to the metadata needed to tell a
// policy refusal from an admitted connection that failed upstream.
type NetworkDiagnostic struct {
	Timestamp  string `json:"timestamp"`
	Generation int64  `json:"generation"`
	Outcome    string `json:"outcome"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
}

// DiagnosticSnapshot is an ephemeral view of the current proxy process. It is
// neither a cursor nor durable evidence. EncodedBytes counts the JSON encoding
// of Events, excluding the response envelope.
type DiagnosticSnapshot struct {
	Generation   int64               `json:"generation"`
	Events       []NetworkDiagnostic `json:"events"`
	Truncated    bool                `json:"truncated"`
	Omitted      uint64              `json:"omitted"`
	EncodedBytes int                 `json:"encoded_bytes"`
}

type storedDiagnostic struct {
	event NetworkDiagnostic
	size  int
}

// diagnosticBuffer never makes a proxy connection wait. A contended writer is
// counted as omitted; a diagnostic reader may wait because it is an explicitly
// invoked operator action outside the traffic path.
type diagnosticBuffer struct {
	mu         sync.Mutex
	generation int64
	now        func() time.Time
	events     []storedDiagnostic
	bytes      int
	omitted    uint64
	contended  atomic.Uint64
}

func newDiagnosticBuffer(generation int64, now func() time.Time) *diagnosticBuffer {
	if now == nil {
		now = time.Now
	}
	return &diagnosticBuffer{generation: generation, now: now}
}

func (b *diagnosticBuffer) record(outcome, host string, port int) {
	if b == nil {
		return
	}
	if !b.mu.TryLock() {
		b.contended.Add(1)
		return
	}
	defer b.mu.Unlock()
	event := NetworkDiagnostic{
		Timestamp:  b.now().UTC().Format(time.RFC3339Nano),
		Generation: b.generation,
		Outcome:    outcome,
		Host:       host,
		Port:       port,
	}
	raw, err := json.Marshal(event)
	if err != nil || len(raw) > MaxDiagnosticBytes {
		b.omitted++
		return
	}
	b.events = append(b.events, storedDiagnostic{event: event, size: len(raw)})
	b.bytes += len(raw)
	for len(b.events) > MaxDiagnosticLimit || b.bytes > MaxDiagnosticBytes {
		b.bytes -= b.events[0].size
		b.events = b.events[1:]
		b.omitted++
	}
}

func (b *diagnosticBuffer) snapshot(limit, maxBytes int) DiagnosticSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := DiagnosticSnapshot{Generation: b.generation, Events: []NetworkDiagnostic{}}
	queryOmitted := uint64(0)
	selected := make([]NetworkDiagnostic, 0, min(limit, len(b.events)))
	for index := len(b.events) - 1; index >= 0; index-- {
		entry := b.events[index]
		if len(selected) >= limit || result.EncodedBytes+entry.size > maxBytes {
			queryOmitted += uint64(index + 1)
			break
		}
		selected = append(selected, entry.event)
		result.EncodedBytes += entry.size
	}
	for index := len(selected) - 1; index >= 0; index-- {
		result.Events = append(result.Events, selected[index])
	}
	result.Omitted = b.omitted + b.contended.Load() + queryOmitted
	result.Truncated = result.Omitted > 0
	return result
}
