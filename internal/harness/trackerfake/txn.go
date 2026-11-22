package trackerfake

import (
	"encoding/json"
	"fmt"
	"maps"
)

// A handler runs in a transaction the pipeline owns (Engine.run): before
// it runs, the engine's ID counter and clock are saved, and the tenant
// its credential names unless its route only reads (match.reads); when
// it answers unhandled, refuses (status 400 or more) or panics they are
// put back, so the request changes nothing a Snapshot, a later ID or a
// later stamp could show. A route that does not declare that it only
// reads is a write. A Linear document with a mutation is a write; the
// executor (Engine.execute) puts the workspace back through the same
// undo when any of its mutations fails with an error, though the answer
// is 200: Linear then applies none of them (measured). The cost is one
// JSON encoding of the tenant per write and one decoding for each roll
// back; a read costs no more for a larger tenant.

// save is the undo of what a handler run for x may change. Callers hold
// mu.
func (e *Engine) save(x *exchange, reads bool) (func(), error) {
	switch {
	case reads:
		return e.saveCounters(), nil
	case x.origin.Tracker == TrackerLinear:
		return e.saveLinear(e.linear[x.tenant])
	}
	return e.saveJira(e.jira[x.tenant])
}

// saveJira is the undo of changes to a Jira site, its Tempo account
// included. Callers hold mu.
func (e *Engine) saveJira(s *jiraSite) (func(), error) {
	raw, err := json.Marshal(s.JiraSite)
	if err != nil {
		return nil, fmt.Errorf("trackerfake: save Jira site %s: %w", s.Site, err)
	}
	private := *s
	private.nextKey = maps.Clone(s.nextKey)
	counters := e.saveCounters()
	return func() {
		private.JiraSite = JiraSite{}
		mustDecode(raw, &private.JiraSite)
		*s = private
		counters()
	}, nil
}

// saveLinear is the undo of changes to a Linear workspace. Callers hold
// mu.
func (e *Engine) saveLinear(w *linearWorkspace) (func(), error) {
	raw, err := json.Marshal(w.LinearWorkspace)
	if err != nil {
		return nil, fmt.Errorf("trackerfake: save Linear workspace %s: %w", w.key(), err)
	}
	private := *w
	counters := e.saveCounters()
	return func() {
		private.LinearWorkspace = LinearWorkspace{}
		mustDecode(raw, &private.LinearWorkspace)
		*w = private
		counters()
	}, nil
}

// saveCounters is the undo of changes to the ID counter and the clock.
// Callers hold mu.
func (e *Engine) saveCounters() func() {
	ids, last := e.ids, e.last
	return func() { e.ids, e.last = ids, last }
}

// mustDecode decodes JSON the engine itself encoded; failing is the
// fake's own defect, which the pipeline answers as unhandled.
func mustDecode(raw []byte, out any) {
	if err := json.Unmarshal(raw, out); err != nil {
		panic(fmt.Sprintf("trackerfake: restore saved state: %v", err))
	}
}
