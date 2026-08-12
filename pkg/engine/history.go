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

// Record adds a loop tick's record, collapsing consecutive quiet ones.
// A quiet tick — nothing planned, no enumeration problems — refreshes
// the head quiet record (count + timestamp) instead of consuming a slot,
// so the bounded buffer keeps real sweeps instead of a wall of no-ops,
// and the panel still shows when the loop last ran. RPC-sourced records
// go through Add unconditionally: an operator's explicit sweep is an
// event even when it does nothing.
func (h *History) Record(record SweepRecord) {
	quiet := len(record.Results) == 0 && len(record.Problems) == 0
	if !quiet {
		h.Add(record)

		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.records) > 0 && h.records[0].QuietTicks > 0 {
		head := &h.records[0]
		head.QuietTicks++
		head.At = record.At
		head.DryRun = record.DryRun
		head.Kept = record.Kept

		return
	}

	record.QuietTicks = 1
	h.records = append([]SweepRecord{record}, h.records...)

	if len(h.records) > h.cap {
		h.records = h.records[:h.cap]
	}
}
