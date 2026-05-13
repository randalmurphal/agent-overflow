package provider

// DefaultApprovalDedupCap is the soft cap both bundled providers use for
// their per-session "already answered" dedup set. 1000 entries is a
// generous-but-bounded window for the hot path; hitting the cap at
// worst admits a duplicate response for an ancient request, which the
// provider discards.
const DefaultApprovalDedupCap = 1000

// ApprovalDeduper bounds the per-session "request ID already answered"
// set so long-running sessions don't accumulate one entry per answered
// approval for the life of the process. Callers must hold the lock
// that guards the surrounding pending-approval map — the deduper does
// not lock internally because the lookup is always part of a wider
// state transition (check pending, claim dedup, delete pending).
type ApprovalDeduper struct {
	resolved map[string]struct{}
	softCap  int
}

// NewApprovalDeduper returns a deduper that resets its set once it
// reaches softCap entries. A non-positive softCap falls back to
// DefaultApprovalDedupCap so a zero-value ApprovalDeduper (e.g. a
// session struct field without explicit initialization) still behaves
// correctly.
func NewApprovalDeduper(softCap int) ApprovalDeduper {
	if softCap <= 0 {
		softCap = DefaultApprovalDedupCap
	}
	return ApprovalDeduper{softCap: softCap}
}

// IsResolved reports whether the request id has already been answered.
func (d *ApprovalDeduper) IsResolved(id string) bool {
	_, ok := d.resolved[id]
	return ok
}

// MarkResolved records the request id as answered. If the set has grown
// past the soft cap, it is reset first.
func (d *ApprovalDeduper) MarkResolved(id string) {
	limit := d.softCap
	if limit <= 0 {
		limit = DefaultApprovalDedupCap
	}
	if d.resolved == nil || len(d.resolved) >= limit {
		d.resolved = make(map[string]struct{})
	}
	d.resolved[id] = struct{}{}
}

// Reset drops the dedup set entirely. Called from the session-close
// path once no duplicate response can reach the provider.
func (d *ApprovalDeduper) Reset() {
	d.resolved = nil
}

// Forget removes a single id, used when a pending request is being
// re-registered after having been previously resolved.
func (d *ApprovalDeduper) Forget(id string) {
	delete(d.resolved, id)
}
