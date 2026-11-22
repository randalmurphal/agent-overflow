package browser

import (
	"slices"
	"testing"
)

// The port's copies of the provider's stop retry bound
// (BROWSER_FIND_STOP_ATTEMPTS).
const findPortStopAttempts = 3

// findIntent is the port's Intent: seq is a start's sequence, the sequence
// a step steps, or a stop's bound.
type findIntent struct {
	kind, page, query string
	seq               uint64
	attempt           int
}

// findCall is a call a provider sent; ran once the Manager ran it.
type findCall struct {
	intent findIntent
	ran    bool
	state  FindState
	err    error
	events []FindState
}

// findProvider is a port of createBrowserFind in bar mode. Each method
// mirrors the function of the same name there; open and query are the
// pane-find store's open state and query for the pane.
type findProvider struct {
	e            *findTableEnv
	label, mount string
	queue        []findIntent
	inFlight     *findCall
	// page is bound.pageId; showing, issued and stopped are showing, issued
	// and stoppedThrough. The port numbers its own starts from 1, as the
	// provider does with the module's counter reset.
	page                     string
	showing, issued, stopped uint64
	// retries are the stops waiting on their retry timer, in the order
	// their timers were set.
	retries []findIntent
	// count, current and pending are view.count (-1 for an uncounted
	// match), view.current and view.pending; answered is whether the view
	// holds an answer of the search it shows.
	count, current    int
	pending, answered bool
	// errored is whether view.error is set; undelivered and cleanup are
	// undelivered for the provider's mount and whether onCleanupError last
	// reported a failure.
	errored     bool
	undelivered uint64
	cleanup     bool
	open        bool
	query       string
	// debounce is a typed query whose debounce has not run out, "" for none.
	debounce string
	disposed bool
	// hold, when set, keeps the next call outstanding until release:
	// "answer" runs it at once and answers it late, "arrive" runs it late.
	hold  string
	steps []findScriptStep
}

func (p *findProvider) record(step findScriptStep) {
	p.steps = append(p.steps, step)
}

func (p *findProvider) reset() {
	p.count, p.current, p.pending, p.answered, p.errored = 0, -1, false, false, false
}

func (p *findProvider) show(state FindState) {
	if !state.Active {
		p.reset()
		return
	}
	switch {
	case !state.Found:
		p.count = 0
	case state.Matches < 0:
		p.count = -1
	default:
		p.count = state.Matches
	}
	p.current, p.pending, p.answered = state.Current, false, true
}

func (p *findProvider) drop(kinds ...string) {
	p.queue = slices.DeleteFunc(p.queue, func(intent findIntent) bool { return slices.Contains(kinds, intent.kind) })
}

func (p *findProvider) enqueue(t *testing.T, intent findIntent) {
	p.queue = append(p.queue, intent)
	p.drain(t)
}

// queueStop is enqueueStop without the drain: a stop of the mount's search
// through the bound, unless one through it or later is queued already.
func (p *findProvider) queueStop(page string, through uint64) {
	p.stopped = max(p.stopped, through)
	for _, intent := range p.queue {
		if intent.kind == FindStop && intent.seq >= through {
			return
		}
	}
	p.queue = append(p.queue, findIntent{kind: FindStop, page: page, seq: through})
}

// stopIssued queues the stop of every search the provider started, unless
// one was made since its last start.
func (p *findProvider) stopIssued() {
	if p.issued > p.stopped {
		p.queueStop(p.page, p.issued)
	}
}

func (p *findProvider) retryStop(intent findIntent) {
	if p.disposed || intent.attempt+1 >= findPortStopAttempts {
		return
	}
	p.retries = append(p.retries, intent)
}

// settleRetries drops the retries an answered stop made needless: those
// bounded at or below it.
func (p *findProvider) settleRetries(stop findIntent) {
	p.retries = slices.DeleteFunc(p.retries, func(intent findIntent) bool { return intent.seq <= stop.seq })
}

// runRetries is the stop retry wait running out: every waiting stop is
// sent again, in the order its timer was set.
func (p *findProvider) runRetries(t *testing.T) bool {
	if len(p.retries) == 0 {
		return false
	}
	p.record(findScriptStep{Retry: true})
	waiting := p.retries
	p.retries = nil
	for _, intent := range waiting {
		intent.attempt++
		p.enqueue(t, intent)
	}
	return true
}

func (p *findProvider) drain(t *testing.T) {
	for p.inFlight == nil && len(p.queue) > 0 {
		intent := p.queue[0]
		p.queue = p.queue[1:]
		call := &findCall{intent: intent}
		p.inFlight = call
		p.record(findScriptStep{Send: &findScriptCall{Kind: intent.kind, Page: p.e.pageLabel(intent.page), Seq: intent.seq, Query: intent.query}})
		mode := p.hold
		p.hold = ""
		if mode != "arrive" {
			p.e.run(t, p, call)
		}
		if mode != "" {
			return
		}
		p.complete(call)
	}
}

// release answers the outstanding call, running it first if it has not
// arrived yet, and lets the provider drain on.
func (p *findProvider) release(t *testing.T) {
	t.Helper()
	call := p.inFlight
	if call == nil {
		t.Fatalf("provider %s has no call outstanding", p.label)
	}
	if !call.ran {
		p.e.run(t, p, call)
	}
	p.complete(call)
	p.drain(t)
}

// complete is drain's handling of one answer. The intents it queues run
// once the call in flight is cleared, from drain.
func (p *findProvider) complete(call *findCall) {
	intent := call.intent
	p.inFlight = nil
	if call.err != nil {
		p.record(findScriptStep{Error: true})
		if intent.kind == FindStop {
			p.undelivered = max(p.undelivered, intent.seq)
			if !p.disposed {
				p.cleanup = true
			}
			p.retryStop(intent)
			return
		}
		if intent.seq == p.showing {
			p.reset()
			p.showing = 0
			p.errored = true
			p.drop(FindNext, FindPrev)
		}
		p.queueStop(intent.page, intent.seq)
		return
	}
	normal := p.e.normalize(call.state)
	p.record(findScriptStep{Answer: &normal})
	switch {
	case intent.kind == FindStop:
		p.settleRetries(intent)
		if p.undelivered != 0 && p.undelivered <= intent.seq {
			p.undelivered = 0
			if !p.disposed {
				p.cleanup = false
			}
		}
	case intent.seq != p.showing:
		// A search the bar no longer shows.
	case call.state.Foreign:
		p.accept()
	default:
		p.show(call.state)
	}
}

func (p *findProvider) ended(state FindState) {
	if p.showing == 0 || state.PageID != p.page || state.Mount != p.mount || state.Seq != p.showing {
		return
	}
	p.accept()
}

// accept is the shown search's end: the bar closes, which is closeBar, and
// through it the store's close calling clear, which has nothing left to
// stop.
func (p *findProvider) accept() {
	p.stopped = max(p.stopped, p.issued)
	p.closeLocal()
	p.open = false
}

// closeLocal is close without its drain.
func (p *findProvider) closeLocal() {
	p.debounce = ""
	p.drop(FindStart, FindNext, FindPrev)
	p.showing = 0
	p.reset()
	p.stopIssued()
}

func (p *findProvider) close(t *testing.T) {
	p.closeLocal()
	p.drain(t)
}

func (p *findProvider) begin(t *testing.T, query string) {
	p.page = p.e.shown
	p.issued++
	p.showing = p.issued
	p.enqueue(t, findIntent{kind: FindStart, page: p.page, query: query, seq: p.issued})
}

// The reader's actions, as the pane makes them: each only when the pane's
// bar is in the state the reader could do it in.

// uiOpen is openPaneFind: the bar opens with the query it kept, which
// searches.
func (p *findProvider) uiOpen(t *testing.T) {
	if p.disposed || p.open {
		return
	}
	p.record(findScriptStep{UI: "open"})
	p.open = true
	if p.query != "" {
		p.searchQuery(t, p.query, true)
	}
}

// uiSearch is setPaneFindQuery, with its debounce run out at once.
func (p *findProvider) uiSearch(t *testing.T, query string) {
	if p.disposed || !p.open || query == p.query {
		return
	}
	p.record(findScriptStep{UI: "search", Query: query})
	p.query = query
	p.searchQuery(t, query, true)
}

// uiType is setPaneFindQuery with its debounce still running.
func (p *findProvider) uiType(t *testing.T, query string) {
	if p.disposed || !p.open || query == p.query {
		return
	}
	p.record(findScriptStep{UI: "type", Query: query})
	p.query = query
	p.searchQuery(t, query, false)
}

// runDebounce is a typed query's debounce running out.
func (p *findProvider) runDebounce(t *testing.T) {
	if p.debounce == "" {
		return
	}
	p.record(findScriptStep{Debounce: true})
	query := p.debounce
	p.debounce = ""
	p.begin(t, query)
}

func (p *findProvider) searchQuery(t *testing.T, query string, debounced bool) {
	p.debounce = ""
	p.drop(FindStart, FindNext, FindPrev)
	p.showing = 0
	if query == "" {
		p.reset()
		p.stopIssued()
		p.drain(t)
		return
	}
	p.pending = true
	p.errored = false
	if !debounced {
		p.debounce = query
		return
	}
	p.begin(t, query)
}

func (p *findProvider) uiNext(t *testing.T) {
	if p.disposed || !p.open {
		return
	}
	p.record(findScriptStep{UI: "next"})
	if p.showing == 0 || p.pending || p.count == 0 {
		return
	}
	if n := len(p.queue); n > 0 && p.queue[n-1].kind == FindPrev {
		p.queue = p.queue[:n-1]
		return
	}
	p.enqueue(t, findIntent{kind: FindNext, page: p.page, seq: p.showing})
}

// uiClear is closePaneFind: the bar closes and keeps its query.
func (p *findProvider) uiClear(t *testing.T) {
	if p.disposed || !p.open {
		return
	}
	p.record(findScriptStep{UI: "clear"})
	p.open = false
	p.close(t)
}

// uiReopen closes a shown bar and opens it again.
func (p *findProvider) uiReopen(t *testing.T) {
	p.uiClear(t)
	p.uiOpen(t)
}

func (p *findProvider) dispose(t *testing.T) {
	if p.disposed {
		return
	}
	p.record(findScriptStep{UI: "dispose"})
	p.open = false
	p.close(t)
	if p.undelivered != 0 {
		p.cleanup = false
	}
	p.disposed = true
	p.retries = nil
}
