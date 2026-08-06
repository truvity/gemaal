package engine

import "sync"

// History is a bounded, in-memory record of recent sweeps — what the
// panel's history page and an operator's "what did the last tick do?"
// read. In-memory on purpose: the ledger design keeps no store, the
// durable audit trail is the structured log, and a restart losing the
// page's scrollback loses nothing the logs do not still have.
type History struct {
	mu      sync.Mutex
	cap     int
	records []SweepRecord
}

// NewHistory bounds the buffer; capacity <= 0 defaults to 100.
func NewHistory(capacity int) *History {
	if capacity <= 0 {
		capacity = 100
	}

	return &History{cap: capacity}
}

// Add records one sweep, newest first, evicting the oldest past capacity.
func (h *History) Add(record SweepRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append([]SweepRecord{record}, h.records...)
	if len(h.records) > h.cap {
		h.records = h.records[:h.cap]
	}
}

// List returns the records, newest first.
func (h *History) List() []SweepRecord {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]SweepRecord, len(h.records))
	copy(out, h.records)

	return out
}
