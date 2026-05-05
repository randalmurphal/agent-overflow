package design

import (
	"context"
	"strings"
	"sync"
	"time"
)

const (
	// DiagnosticRingCap is the per-thread bounded ring size. Set high
	// enough to absorb a noisy iframe (many console.warn lines from a
	// design library), low enough that history isn't memorable.
	DiagnosticRingCap = 100

	// diagnosticDrainDeadline caps how long Drain blocks waiting for
	// new diagnostics when the watcher fired recently but nothing has
	// landed yet. Solves the agent-edits-then-reads-stale-diagnostics
	// race: the iframe needs ~100-300ms to load+parse+execute and
	// post errors back; without the wait, the agent's get_design_diagnostics
	// returns empty between edit and load.
	diagnosticDrainDeadline = time.Second

	// diagnosticActivityWindow is how recent a watcher event has to be
	// for Drain to consider blocking. Outside this window the iframe
	// has had time to render and report, so Drain returns immediately
	// on an empty buffer.
	diagnosticActivityWindow = 500 * time.Millisecond
)

// DiagnosticBuffer is the per-thread ring used by both
// `get_design_diagnostics` (Codex/Claude MCP) and any auto-prepend
// path. Reads are token-keyed: callers pass the token they last saw
// and receive everything appended after.
//
// Each thread carries its own *sync.Cond so an Append on one thread
// doesn't broadcast-wake every blocked Drain across every thread —
// avoids the cross-thread thundering herd that would otherwise fire
// on a noisy iframe shared across many concurrent design sessions.
type DiagnosticBuffer struct {
	mu       sync.Mutex
	threads  map[string]*threadRing
	clockNow func() time.Time
}

type threadRing struct {
	// entries is a fixed-cap deque keyed by [head, head+len) modulo cap.
	// Using a circular index instead of slide-on-write avoids the
	// O(cap) memmove every Append once the ring is full.
	entries        [DiagnosticRingCap]Diagnostic
	head           int   // first valid index
	count          int   // number of valid entries
	nextToken      int64 // monotonic token assigned to the most recent append
	lastWriterPing time.Time

	// cond signals waiting Drain calls when an Append lands or the
	// session is torn down. Per-thread so we don't wake unrelated
	// drains on every diagnostic.
	cond     *sync.Cond
	tornDown bool
}

// NewDiagnosticBuffer constructs a buffer. clockNow is optional —
// tests can pass a deterministic clock; production passes nil and
// uses time.Now.
func NewDiagnosticBuffer(clockNow func() time.Time) *DiagnosticBuffer {
	if clockNow == nil {
		clockNow = time.Now
	}
	return &DiagnosticBuffer{
		threads:  make(map[string]*threadRing),
		clockNow: clockNow,
	}
}

// AppendBatch records a batch atomically so a Drain caller observes
// either all or none of them. Returns the batch with assigned tokens.
func (b *DiagnosticBuffer) AppendBatch(threadID string, batch []Diagnostic) []Diagnostic {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || len(batch) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ring := b.ensure(threadID)
	out := make([]Diagnostic, 0, len(batch))
	for _, diag := range batch {
		ring.nextToken++
		diag.Token = ring.nextToken
		if diag.CreatedAt == 0 {
			diag.CreatedAt = b.clockNow().UnixMilli()
		}
		ring.push(diag)
		out = append(out, diag)
	}
	ring.cond.Broadcast()
	return out
}

// MarkActivity records that the file watcher just fired for this
// thread. Used by Drain's settle-window logic to decide whether to
// wait for new diagnostics on an empty buffer.
func (b *DiagnosticBuffer) MarkActivity(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := b.ensure(threadID)
	ring.lastWriterPing = b.clockNow()
}

// Drain returns diagnostics with token > since for the thread, and the
// new highest token. If the buffer is empty AND the watcher fired in
// the last diagnosticActivityWindow, Drain blocks up to
// diagnosticDrainDeadline waiting for the iframe to emit before
// returning. The wait yields immediately on ctx cancellation or
// session teardown.
//
// Callers (the get_design_diagnostics MCP tool) pass `since_token`
// from their previous call so they only see new entries.
func (b *DiagnosticBuffer) Drain(ctx context.Context, threadID string, since int64) ([]Diagnostic, int64) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, since
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ring := b.ensure(threadID)
	if out, latest := ring.drain(since); len(out) > 0 {
		return out, latest
	}

	// Empty result. Block only if the watcher fired recently — that
	// means the iframe is mid-load and may yet report something.
	if b.clockNow().Sub(ring.lastWriterPing) > diagnosticActivityWindow {
		return nil, ring.nextToken
	}
	deadline := b.clockNow().Add(diagnosticDrainDeadline)

	// Single deadline timer for the entire wait, plus one ctx-watcher
	// goroutine. Both fire ring.cond.Broadcast so the inner Wait wakes;
	// we stop both on return.
	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			ring.cond.Broadcast()
		case <-stopWatcher:
		}
	}()
	timer := time.AfterFunc(deadline.Sub(b.clockNow()), func() {
		ring.cond.Broadcast()
	})
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			return nil, ring.nextToken
		}
		if ring.tornDown {
			return nil, ring.nextToken
		}
		if !b.clockNow().Before(deadline) {
			return nil, ring.nextToken
		}
		ring.cond.Wait()
		if out, latest := ring.drain(since); len(out) > 0 {
			return out, latest
		}
	}
}

// LatestToken returns the current highest token for a thread without
// claiming any diagnostics. Used by callers that want to set a
// since_token baseline at startup.
func (b *DiagnosticBuffer) LatestToken(threadID string) int64 {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ring, ok := b.threads[threadID]
	if !ok {
		return 0
	}
	return ring.nextToken
}

// TeardownThread drops a thread's ring on session close. Any blocked
// Drain wakes and returns nothing.
func (b *DiagnosticBuffer) TeardownThread(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}
	b.mu.Lock()
	ring, ok := b.threads[threadID]
	if ok {
		ring.tornDown = true
		ring.cond.Broadcast()
		delete(b.threads, threadID)
	}
	b.mu.Unlock()
}

func (b *DiagnosticBuffer) ensure(threadID string) *threadRing {
	ring, ok := b.threads[threadID]
	if !ok {
		ring = &threadRing{}
		ring.cond = sync.NewCond(&b.mu)
		b.threads[threadID] = ring
	}
	return ring
}

// push adds a diagnostic to the ring, evicting the oldest entry when
// at capacity. The token has already been stamped onto diag.
func (r *threadRing) push(diag Diagnostic) {
	if r.count < DiagnosticRingCap {
		r.entries[(r.head+r.count)%DiagnosticRingCap] = diag
		r.count++
		return
	}
	// Full: overwrite the head, advance head — drop oldest.
	r.entries[r.head] = diag
	r.head = (r.head + 1) % DiagnosticRingCap
}

// drain returns entries with token > since, plus the current latest
// token. Linear scan is fine for cap=100.
func (r *threadRing) drain(since int64) ([]Diagnostic, int64) {
	if r.count == 0 {
		return nil, r.nextToken
	}
	if since >= r.nextToken {
		return nil, r.nextToken
	}
	out := make([]Diagnostic, 0, r.count)
	for i := 0; i < r.count; i++ {
		d := r.entries[(r.head+i)%DiagnosticRingCap]
		if d.Token > since {
			out = append(out, d)
		}
	}
	return out, r.nextToken
}
