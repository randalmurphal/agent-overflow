package trackerfake

import (
	"encoding/json"
	"fmt"
	"maps"
)

// A handler runs in a transaction the pipeline owns (Engine.run): before
// it runs, the tenant its credential names and the engine's ID counter
// and clock are saved, and when it answers unhandled, refuses (status 400
// or more) or panics they are put back, so the request changes nothing a
// Snapshot, a later ID or a later stamp could show. A Linear mutation
// runs in one of its own, since Linear resolves each root mutation on
// its own. The cost is one JSON encoding of the tenant per handler run,
// and one decoding when it is rolled back.

// save is the undo of what a handler run for x may change. Callers hold
// mu.
func (e *Engine) save(x *exchange) (func(), error) {
	if x.origin.Tracker == TrackerLinear {
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
	return e.counters(func() {
		private.JiraSite = JiraSite{}
		mustDecode(raw, &private.JiraSite)
		*s = private
	}), nil
}

// saveLinear is the undo of changes to a Linear workspace. Callers hold
// mu.
func (e *Engine) saveLinear(w *linearWorkspace) (func(), error) {
	raw, err := json.Marshal(w.LinearWorkspace)
	if err != nil {
		return nil, fmt.Errorf("trackerfake: save Linear workspace %s: %w", w.key(), err)
	}
	private := *w
	return e.counters(func() {
		private.LinearWorkspace = LinearWorkspace{}
		mustDecode(raw, &private.LinearWorkspace)
		*w = private
	}), nil
}

// counters wraps restore to put back the ID counter and the clock too.
func (e *Engine) counters(restore func()) func() {
	ids, last := e.ids, e.last
	return func() {
		restore()
		e.ids, e.last = ids, last
	}
}

// mustDecode decodes JSON the engine itself encoded; failing is the
// fake's own defect, which the pipeline answers as unhandled.
func mustDecode(raw []byte, out any) {
	if err := json.Unmarshal(raw, out); err != nil {
		panic(fmt.Sprintf("trackerfake: restore saved state: %v", err))
	}
}
