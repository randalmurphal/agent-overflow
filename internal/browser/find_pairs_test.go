package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The find's operations run here in ordered sequences against two pane
// mounts' providers, A holding a search and B mounted beside it: what each
// pane's reader does (open the bar, type, step, empty the query, close the
// bar), starts the Manager refuses (a query too long, a page already gone),
// a document commit and a late one, a page switch, suspend and restore,
// detaching A, and engine failures (a start that fails, a stop that fails
// once or always). TestFindOutstandingCallAcrossLaterActions also leaves one
// call of A's outstanding, answered late or arriving late, across the next
// one and two of A's actions; TestFindNamedSequences runs orders found by
// review.
//
// Each provider is a port of browserFind.svelte.ts (findProvider): its
// queue, sequences, drop rules, answer rules and stop retries. It sends what that provider would; every end the Manager
// announces reaches both providers when it is announced, in flight or not,
// and the stop retries waiting when a sequence quiesces run then. The
// frontend replays the recorded scripts against the real provider
// (browserFindScripts.test.ts), which must send exactly the recorded calls,
// so the port cannot drift from it: regenerate them with
//
//	AO_UPDATE_FIND_SCRIPTS=1 make go-test GO_TEST_PKGS=./internal/browser GO_TEST_FLAGS='-run TestFind'
//
// After each sequence quiesces:
//
//  1. no call is left unanswered and no stop retry waits;
//  2. the engine holds a search exactly when a session is active or owes
//     its stop, and owes one only while the stop it is owed failed and no
//     find action has run since;
//  3. an answer names the asking mount and the action's sequence (a stop's
//     at or below its bound), or is marked Foreign and names the search's
//     real owner;
//  4. a search exists only on the shown active page, owned by a mounted
//     pane;
//  5. an action fails only for an engine failure, a start the page, the
//     mount or the query cannot take, or an action whose deadline passed
//     before it ran: never because its search already ended;
//  6. a provider's bar shows a count, a current match and enabled stepping
//     exactly when the Manager holds a search for its mount at the sequence
//     the bar shows, and then that search's count and current match; the
//     Manager holds no other search of the mount;
//  7. an open bar shows an error exactly when the last start or step of its
//     newest search failed, and the pane shows a find cleanup error exactly
//     when a stop the provider made failed and no stop through it or further
//     has been answered since, while the pane is mounted.

// updateFindScripts rewrites the scripts the frontend replays.
var updateFindScripts = os.Getenv("AO_UPDATE_FIND_SCRIPTS") == "1"

// findScriptsDir holds the scripts, one file per table.
const findScriptsDir = "../../frontend/src/test/fixtures/browserFind/"

// The port's copies of the provider's stop retry bound
// (BROWSER_FIND_STOP_ATTEMPTS).
const findPortStopAttempts = 3

var (
	errFakeFindStart = errors.New("fake engine: find start failed")
	errFakeFindStop  = errors.New("fake engine: find stop failed")
)

type findTableEnv struct {
	f      *findFixture
	mounts map[string]string
	labels map[string]string
	other  string
	// shown is the page the panes show.
	shown     string
	providers map[string]*findProvider
	answers   []findTableAnswer
	errs      []findTableError
	late      []func()
	incs      map[uint64]uint64

	mu        sync.Mutex
	stopFail  string
	startFail bool
	// expire makes the next call run past its deadline.
	expire bool
	// failed is, per provider and sequence, whether that search's last start
	// or step failed; undelivered is, per provider, the highest bound of a
	// failed stop no answered stop has covered since.
	failed      map[string]map[uint64]bool
	undelivered map[string]uint64
	// actions counts the find actions run on each page, and lastStop is
	// each page's last engine stop: at which action, and whether it failed.
	actions  map[string]int
	lastStop map[string]findStopRecord
	midEnd   func()
	midPoint string
}

type findStopRecord struct {
	at     int
	failed bool
}

type findTableError struct {
	op, kind string
	err      error
}

type findTableAnswer struct {
	op, kind, mount string
	seq             uint64
	owner           findOwner
	state           FindState
}

// findScriptStep is one thing that happened to a provider, in order: what
// the reader did (UI, with Query for "search" and "type"), a typed query's
// debounce running out (Debounce), the page the pane now shows (Shown), a
// call the provider sent (Send), the answer to its oldest unanswered call
// (Answer, or Error), an end the thread's providers were told of (Event), or
// the provider's stop retry wait running out (Retry).
type findScriptStep struct {
	UI       string          `json:"ui,omitempty"`
	Query    string          `json:"query,omitempty"`
	Debounce bool            `json:"debounce,omitempty"`
	Shown    string          `json:"shown,omitempty"`
	Send     *findScriptCall `json:"send,omitempty"`
	Answer   *FindState      `json:"answer,omitempty"`
	Error    bool            `json:"error,omitempty"`
	Event    *FindState      `json:"event,omitempty"`
	Retry    bool            `json:"retry,omitempty"`
}

type findScriptCall struct {
	Kind  string `json:"kind"`
	Page  string `json:"page"`
	Seq   uint64 `json:"seq"`
	Query string `json:"query,omitempty"`
}

// findScriptView is what the provider's bar shows at the end: count is -1
// for an uncounted match. Error is whether the bar shows an error, Cleanup
// whether the pane shows a find cleanup error.
type findScriptView struct {
	Count   int  `json:"count"`
	Current int  `json:"current"`
	Pending bool `json:"pending"`
	Error   bool `json:"error"`
	Cleanup bool `json:"cleanup"`
}

type findScript struct {
	Name  string           `json:"name"`
	Mount string           `json:"mount"`
	Page  string           `json:"page"`
	Steps []findScriptStep `json:"steps"`
	// Bar is whether the provider's bar is open at the end, and View what
	// it shows: the real provider must leave both so.
	Bar  bool           `json:"bar"`
	View findScriptView `json:"view"`
}

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
	if intent.attempt+1 >= findPortStopAttempts {
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
}

func newFindTableEnv(t *testing.T, base string) *findTableEnv {
	t.Helper()
	f := newFindFixture(t, "https://example.test/alpha-beta")
	e := &findTableEnv{
		f: f, mounts: map[string]string{"A": f.mount}, incs: map[uint64]uint64{},
		actions: map[string]int{}, lastStop: map[string]findStopRecord{},
		failed: map[string]map[uint64]bool{"A": {}, "B": {}}, undelivered: map[string]uint64{},
	}
	other, err := f.manager.NewCompanionPage(t.Context(), f.access)
	if err != nil {
		t.Fatal(err)
	}
	e.other = other.ID
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	e.shown = f.pageID
	f.attach(t)
	e.mounts["B"] = f.mount
	f.mount = e.mounts["A"]
	e.labels = map[string]string{e.mounts["A"]: "mount-A", e.mounts["B"]: "mount-B"}
	e.providers = map[string]*findProvider{}
	for _, label := range []string{"A", "B"} {
		e.providers[label] = &findProvider{e: e, label: label, mount: e.mounts[label], current: -1}
	}
	e.install(t)
	setFindPoint(f.manager, func(point string) {
		e.mu.Lock()
		fire := e.midEnd
		if fire != nil && (e.midPoint == "" || e.midPoint == point) {
			e.midEnd = nil
		} else {
			fire = nil
		}
		e.mu.Unlock()
		if fire != nil {
			fire()
		}
	})
	a := e.providers["A"]
	switch base {
	case "search":
		a.uiOpen(t)
		a.uiSearch(t, "alpha")
	case "bar":
		a.uiOpen(t)
	}
	e.quiesce(t)
	e.answers = nil
	e.check(t, "base")
	return e
}

// setFindPoint installs the Manager's find point seam.
func setFindPoint(m *Manager, hook func(point string)) {
	if hook == nil {
		m.findPointHook.Store(nil)
		return
	}
	m.findPointHook.Store(&hook)
}

// livePages are the table's pages that are live, by id.
func (e *findTableEnv) livePages() map[string]*managedPage {
	pages := map[string]*managedPage{}
	for _, id := range []string{e.f.pageID, e.other} {
		if p, _, err := e.f.manager.lookupOwnedPage(e.f.access, id); err == nil {
			pages[id] = p
		}
	}
	return pages
}

// install gives every live page's fake engine the table's failure
// injection.
func (e *findTableEnv) install(t *testing.T) {
	t.Helper()
	e.f.fake = e.page(t).driver.(*fakePage)
	for id, p := range e.livePages() {
		fake := p.driver.(*fakePage)
		fake.mu.Lock()
		fake.findHook = nil
		fake.findAnswer = nil
		fake.findStopErr = func(context.Context) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			failed := e.stopFail != ""
			if e.stopFail == "once" {
				e.stopFail = ""
			}
			e.lastStop[id] = findStopRecord{at: e.actions[id], failed: failed}
			if failed {
				return errFakeFindStop
			}
			return nil
		}
		e.mu.Lock()
		if e.startFail {
			fake.findStartErr = errFakeFindStart
		} else {
			fake.findStartErr = nil
		}
		e.mu.Unlock()
		fake.mu.Unlock()
	}
}

func (e *findTableEnv) page(t *testing.T) *managedPage {
	t.Helper()
	p, _, err := e.f.manager.lookupOwnedPage(e.f.access, e.f.pageID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *findTableEnv) pageLabel(id string) string {
	switch id {
	case e.f.pageID:
		return "page-1"
	case e.other:
		return "page-2"
	case findGonePage:
		return "page-gone"
	}
	return id
}

// settle waits until every submitted end is applied.
func (e *findTableEnv) settle() { e.f.manager.findWork.Wait() }

// run is the Manager answering a provider's call; the ends it announced
// reach the providers before the answer does.
func (e *findTableEnv) run(t *testing.T, p *findProvider, call *findCall) {
	t.Helper()
	intent := call.intent
	e.mu.Lock()
	e.actions[intent.page]++
	e.mu.Unlock()
	ctx := t.Context()
	e.mu.Lock()
	if e.expire {
		e.expire = false
		expired, cancel := context.WithCancel(ctx)
		cancel()
		ctx = expired
	}
	e.mu.Unlock()
	state, err := e.f.manager.Find(ctx, e.f.access, intent.page, intent.kind, intent.query, p.mount, intent.seq)
	e.settle()
	var owner findOwner
	if page, ok := e.livePages()[intent.page]; ok {
		page.findMu.Lock()
		owner = findOwner{mount: page.find.owner, seq: page.find.seq}
		page.findMu.Unlock()
	}
	call.ran, call.state, call.err = true, state, err
	switch {
	case intent.kind != FindStop:
		e.failed[p.label][intent.seq] = err != nil
	case err != nil:
		e.undelivered[p.label] = max(e.undelivered[p.label], intent.seq)
	case intent.seq >= e.undelivered[p.label]:
		e.undelivered[p.label] = 0
	}
	name := p.label + " " + intent.kind
	if err != nil {
		e.errs = append(e.errs, findTableError{op: name, kind: intent.kind, err: err})
	} else {
		e.answers = append(e.answers, findTableAnswer{op: name, kind: intent.kind, mount: p.mount, seq: intent.seq, owner: owner, state: state})
	}
	call.events = e.deliver()
}

// deliver hands every announced end to each provider still registered, as
// applyBrowserFindEvent does, and answers the ends.
func (e *findTableEnv) deliver() []FindState {
	var ends []FindState
	for {
		select {
		case event := <-e.f.events:
			if event.Find == nil || event.Find.Active {
				continue
			}
			ends = append(ends, *event.Find)
			normal := e.normalize(*event.Find)
			for _, label := range []string{"A", "B"} {
				p := e.providers[label]
				if p.disposed {
					continue
				}
				p.record(findScriptStep{Event: &normal})
				p.ended(*event.Find)
			}
		default:
			return ends
		}
	}
}

// showPage makes id the page the panes show.
func (e *findTableEnv) showPage(id string) {
	if e.shown == id {
		return
	}
	e.shown = id
	for _, label := range []string{"A", "B"} {
		if p := e.providers[label]; !p.disposed {
			p.record(findScriptStep{Shown: e.pageLabel(id)})
		}
	}
}

// normalize names identities as the frontend's replay does, so scripts do
// not depend on generated ids or on how many pages the process made.
func (e *findTableEnv) normalize(state FindState) FindState {
	if label, ok := e.labels[state.Mount]; ok {
		state.Mount = label
	}
	state.PageID = e.pageLabel(state.PageID)
	if state.Incarnation != 0 {
		n, ok := e.incs[state.Incarnation]
		if !ok {
			n = uint64(len(e.incs) + 1)
			e.incs[state.Incarnation] = n
		}
		state.Incarnation = n
	}
	return state
}

func (e *findTableEnv) quiesce(t *testing.T) {
	t.Helper()
	for _, deliver := range e.late {
		deliver()
	}
	e.late = nil
	e.settle()
	e.deliver()
	// Each provider's typed query and waiting stops run out, again while the
	// engine keeps refusing, until none waits.
	for _, label := range []string{"A", "B"} {
		p := e.providers[label]
		if p.inFlight == nil {
			p.runDebounce(t)
		}
		for p.inFlight == nil && p.runRetries(t) {
		}
	}
}

func (e *findTableEnv) check(t *testing.T, name string) {
	t.Helper()
	pages := e.livePages()
	sessions := map[string]findSession{}
	for id, p := range pages {
		p.findMu.Lock()
		session := p.find
		p.findMu.Unlock()
		sessions[id] = session
		fake := p.driver.(*fakePage)
		fake.mu.Lock()
		engine := fake.findQuery != ""
		fake.mu.Unlock()
		page := e.pageLabel(id)
		e.mu.Lock()
		actions, lastStop := e.actions[id], e.lastStop[id]
		e.mu.Unlock()
		if session.active && (!e.f.manager.pageShown(e.f.access.ThreadID, id) || !e.f.manager.mountOf(e.f.access.ThreadID, session.owner)) {
			t.Errorf("%s: %s: a search of %s is left on a page not shown, or for a pane that is gone", name, page, e.labels[session.owner])
		}
		if !session.active && (session.owner != "" || session.seq != 0) {
			t.Errorf("%s: %s: no search runs, yet it names an owner %s/%d", name, page, e.labels[session.owner], session.seq)
		}
		if engine != (session.active || session.engine) {
			t.Errorf("%s: %s: engine holds a search %v; the Manager's session active %v, engine owed %v", name, page, engine, session.active, session.engine)
		}
		if engine && !session.active && (!lastStop.failed || lastStop.at != actions) {
			t.Errorf("%s: %s: engine holds a search no session wants after a find action could have stopped it", name, page)
		}
	}
	for _, label := range []string{"A", "B"} {
		p := e.providers[label]
		if p.inFlight != nil {
			t.Errorf("%s: provider %s has a call left unanswered", name, label)
		}
		if len(p.retries) != 0 {
			t.Errorf("%s: provider %s has a stop retry left waiting", name, label)
		}
		session, live := sessions[p.page]
		held := p.showing != 0 && live && session.ownedBy(findOwner{mount: p.mount, seq: p.showing})
		shows := p.showing != 0 && !p.pending && p.answered
		stepping := shows && p.count != 0
		if shows != held {
			t.Errorf("%s: provider %s shows a count %v (count %d, current %d, stepping %v) for sequence %d; the Manager holds that search %v", name, label, shows, p.count, p.current, stepping, p.showing, held)
		}
		if shows && held {
			want := session.state.Matches
			if !session.state.Found {
				want = 0
			}
			if p.count != want || p.current != session.state.Current {
				t.Errorf("%s: provider %s shows count %d at %d; the Manager's search has %d at %d", name, label, p.count, p.current, want, session.state.Current)
			}
		}
		wantError := p.open && p.query != "" && !p.pending && e.failed[label][p.issued]
		if p.errored != wantError {
			t.Errorf("%s: provider %s's bar shows an error %v; its newest search %d last failed %v", name, label, p.errored, p.issued, wantError)
		}
		wantCleanup := !p.disposed && e.undelivered[label] != 0
		if p.cleanup != wantCleanup {
			t.Errorf("%s: provider %s's pane shows a cleanup error %v; a stop through %d is undelivered", name, label, p.cleanup, e.undelivered[label])
		}
		for id, session := range sessions {
			if session.active && session.owner == p.mount && (!held || id != p.page) {
				t.Errorf("%s: provider %s: the Manager searches %s for its mount at sequence %d; the bar shows sequence %d on %s", name, label, e.pageLabel(id), session.seq, p.showing, e.pageLabel(p.page))
			}
		}
	}
	for _, failed := range e.errs {
		refusedStart := failed.kind == FindStart && (errors.Is(failed.err, errFindNotShown) || errors.Is(failed.err, errFindNoMount) ||
			errors.Is(failed.err, errFindQueryTooLong) || errors.Is(failed.err, errPageNotFound))
		allowed := errors.Is(failed.err, errFakeFindStart) || errors.Is(failed.err, errFakeFindStop) || refusedStart ||
			errors.Is(failed.err, context.Canceled)
		if !allowed {
			t.Errorf("%s: %s failed: %v", name, failed.op, failed.err)
		}
	}
	for _, answer := range e.answers {
		if answer.state.Foreign {
			if answer.state.Mount != answer.owner.mount || answer.state.Seq != answer.owner.seq {
				t.Errorf("%s: %s answered Foreign naming %s/%d, the owner was %s/%d", name, answer.op, e.labels[answer.state.Mount], answer.state.Seq, e.labels[answer.owner.mount], answer.owner.seq)
			}
			continue
		}
		named := answer.state.Seq == answer.seq || (answer.kind == FindStop && answer.state.Seq <= answer.seq)
		if answer.state.Mount != answer.mount || !named {
			t.Errorf("%s: %s answered %s/%d to %s/%d unmarked", name, answer.op, e.labels[answer.state.Mount], answer.state.Seq, e.labels[answer.mount], answer.seq)
		}
	}
}

// scripts are the providers' recorded views of the sequence.
func (e *findTableEnv) scripts(t *testing.T, name string) []findScript {
	t.Helper()
	var out []findScript
	for _, label := range []string{"A", "B"} {
		p := e.providers[label]
		if len(p.steps) == 0 {
			continue
		}
		out = append(out, findScript{
			Name: name + " / " + label, Mount: e.labels[p.mount], Page: "page-1", Steps: p.steps, Bar: p.open,
			View: findScriptView{Count: p.count, Current: p.current, Pending: p.pending, Error: p.errored, Cleanup: p.cleanup},
		})
	}
	return out
}

type findTableOp struct {
	name string
	run  func(t *testing.T, e *findTableEnv)
}

func findTableOps() []findTableOp {
	ui := func(name, label string, act func(p *findProvider, t *testing.T)) findTableOp {
		return findTableOp{name: name, run: func(t *testing.T, e *findTableEnv) { act(e.providers[label], t) }}
	}
	other := func(name string, run func(t *testing.T, e *findTableEnv)) findTableOp {
		return findTableOp{name: name, run: func(t *testing.T, e *findTableEnv) {
			run(t, e)
			e.settle()
			e.deliver()
		}}
	}
	var ops []findTableOp
	for _, label := range []string{"A", "B"} {
		ops = append(ops,
			ui("open "+label, label, (*findProvider).uiOpen),
			ui("start "+label, label, func(p *findProvider, t *testing.T) { p.uiSearch(t, "beta") }),
			ui("step "+label, label, (*findProvider).uiNext),
			ui("stop "+label, label, func(p *findProvider, t *testing.T) { p.uiSearch(t, "") }),
			ui("close "+label, label, (*findProvider).uiClear),
		)
	}
	ops = append(ops,
		ui("oversized A", "A", func(p *findProvider, t *testing.T) { p.uiSearch(t, findOversizedQuery) }),
		findTableOp{name: "gone page A", run: func(t *testing.T, e *findTableEnv) {
			e.goneStart(t, e.providers["A"])
			e.settle()
			e.deliver()
		}},
	)
	return append(ops,
		other("commit", func(_ *testing.T, e *findTableEnv) { e.f.fake.commitDocument() }),
		other("late commit", func(_ *testing.T, e *findTableEnv) {
			e.late = append(e.late, e.f.fake.commitDocumentLate())
		}),
		other("switch away", func(t *testing.T, e *findTableEnv) { e.switchTo(t, e.other) }),
		other("switch back", func(t *testing.T, e *findTableEnv) { e.switchTo(t, e.f.pageID) }),
		other("suspend and restore", func(t *testing.T, e *findTableEnv) {
			if err := e.f.manager.SuspendThread(t.Context(), e.f.access.ThreadID); err != nil {
				t.Fatal(err)
			}
			e.switchTo(t, e.f.pageID)
		}),
		other("detach A", func(t *testing.T, e *findTableEnv) {
			e.f.manager.DetachPane(e.mounts["A"])
			e.settle()
			e.deliver()
			// The pane's provider closes what it held as it goes.
			e.providers["A"].dispose(t)
		}),
		other("start fails", func(t *testing.T, e *findTableEnv) {
			e.mu.Lock()
			e.startFail = true
			e.mu.Unlock()
			e.install(t)
		}),
		other("stop fails once", func(_ *testing.T, e *findTableEnv) {
			e.mu.Lock()
			e.stopFail = "once"
			e.mu.Unlock()
		}),
		other("stop fails always", func(_ *testing.T, e *findTableEnv) {
			e.mu.Lock()
			e.stopFail = "always"
			e.mu.Unlock()
		}),
		other("engine recovers", func(t *testing.T, e *findTableEnv) {
			e.mu.Lock()
			e.stopFail, e.startFail = "", false
			e.mu.Unlock()
			e.install(t)
		}),
	)
}

// findGonePage is a page id the Manager has no page for: one that closed
// before the pane learned of it.
const findGonePage = "page-closed-before-the-pane-knew"

// findOversizedQuery is a query the Manager refuses.
var findOversizedQuery = strings.Repeat("a", maxFindQueryBytes+1)

// goneStart is the reader typing while the pane still shows a page that
// already closed: the start is refused, and then the pane shows the page it
// showed before.
func (e *findTableEnv) goneStart(t *testing.T, p *findProvider) {
	if p.disposed || !p.open {
		return
	}
	shown := e.shown
	e.showPage(findGonePage)
	query := "gamma"
	if p.query == query {
		query = "delta"
	}
	p.uiSearch(t, query)
	e.showPage(shown)
}

// switchTo activates a page, restoring it if suspended, and the panes show
// it.
func (e *findTableEnv) switchTo(t *testing.T, id string) {
	t.Helper()
	if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, id); err != nil {
		t.Fatal(err)
	}
	e.settle()
	e.deliver()
	e.showPage(id)
	e.install(t)
}

func runFindSequence(t *testing.T, e *findTableEnv, ops []findTableOp) {
	t.Helper()
	for _, op := range ops {
		op.run(t, e)
	}
	e.quiesce(t)
}

func TestFindEveryOrderedPairKeepsTheInvariants(t *testing.T) {
	ops := findTableOps()
	var scripts []findScript
	for _, base := range []string{"search", "bar"} {
		for _, first := range ops {
			for _, second := range ops {
				name := fmt.Sprintf("%s/%s then %s", base, first.name, second.name)
				t.Run(name, func(t *testing.T) {
					e := newFindTableEnv(t, base)
					runFindSequence(t, e, []findTableOp{first, second})
					e.check(t, name)
					scripts = append(scripts, e.scripts(t, name)...)
				})
			}
		}
	}
	checkFindScripts(t, "pairs.json", scripts)
}

// The operations that interact most, in every ordered triple from a mount
// holding a bar and a search.
func TestFindEveryOrderedTripleKeepsTheInvariants(t *testing.T) {
	if testing.Short() {
		t.Skip("the triple table runs in full test runs")
	}
	keep := map[string]bool{
		"open A": true, "open B": true, "start A": true, "start B": true, "step A": true, "step B": true,
		"stop A": true, "stop B": true, "close A": true, "close B": true, "commit": true, "late commit": true,
		"switch away": true, "suspend and restore": true, "detach A": true, "stop fails once": true,
		"oversized A": true, "gone page A": true,
	}
	var ops []findTableOp
	for _, op := range findTableOps() {
		if keep[op.name] {
			ops = append(ops, op)
		}
	}
	// Each sequence has a Manager of its own, so they run in parallel; the
	// group returns once all have.
	var mu sync.Mutex
	var scripts []findScript
	t.Run("sequences", func(t *testing.T) {
		for _, first := range ops {
			for _, second := range ops {
				for _, third := range ops {
					name := fmt.Sprintf("%s, %s, %s", first.name, second.name, third.name)
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						e := newFindTableEnv(t, "search")
						runFindSequence(t, e, []findTableOp{first, second, third})
						e.check(t, name)
						recorded := e.scripts(t, name)
						mu.Lock()
						scripts = append(scripts, recorded...)
						mu.Unlock()
					})
				}
			}
		}
	})
	checkFindScripts(t, "triples.json", scripts)
}

// One call of A's stays outstanding, answered late (the Manager ran it at
// once) or arriving late (the Manager runs it when it is answered), across
// the next one and two of A's actions: close, reopen, type, step, and a
// page switch away or back. Then it is answered and A drains.
func TestFindOutstandingCallAcrossLaterActions(t *testing.T) {
	sends := []struct {
		name, base string
		act        func(p *findProvider, t *testing.T)
	}{
		{"type", "bar", func(p *findProvider, t *testing.T) { p.uiSearch(t, "beta") }},
		{"retype", "search", func(p *findProvider, t *testing.T) { p.uiSearch(t, "beta") }},
		{"step", "search", (*findProvider).uiNext},
		{"empty", "search", func(p *findProvider, t *testing.T) { p.uiSearch(t, "") }},
		// The stop fails once, so its answer, late, sets a retry going after
		// whatever the later actions started.
		{"empty, its stop refused", "search", func(p *findProvider, t *testing.T) {
			p.e.mu.Lock()
			p.e.stopFail = "once"
			p.e.mu.Unlock()
			p.uiSearch(t, "")
		}},
		{"close", "search", (*findProvider).uiClear},
	}
	later := []struct {
		name string
		run  func(t *testing.T, e *findTableEnv)
	}{
		{"close", func(t *testing.T, e *findTableEnv) { e.providers["A"].uiClear(t) }},
		{"reopen", func(t *testing.T, e *findTableEnv) { e.providers["A"].uiReopen(t) }},
		{"type", func(t *testing.T, e *findTableEnv) {
			p := e.providers["A"]
			query := "alpha"
			if p.query == query {
				query = "gamma"
			}
			p.uiSearch(t, query)
		}},
		{"step", func(t *testing.T, e *findTableEnv) { e.providers["A"].uiNext(t) }},
		{"oversized", func(t *testing.T, e *findTableEnv) { e.providers["A"].uiSearch(t, findOversizedQuery) }},
		{"switch", func(t *testing.T, e *findTableEnv) {
			if e.shown == e.f.pageID {
				e.switchTo(t, e.other)
			} else {
				e.switchTo(t, e.f.pageID)
			}
		}},
	}
	var sequences [][]int
	for i := range later {
		sequences = append(sequences, []int{i})
		for j := range later {
			sequences = append(sequences, []int{i, j})
		}
	}
	var scripts []findScript
	for _, send := range sends {
		for _, mode := range []string{"answer", "arrive"} {
			for _, sequence := range sequences {
				name := fmt.Sprintf("%s %s late, then", send.name, mode)
				for _, i := range sequence {
					name += " " + later[i].name
				}
				t.Run(name, func(t *testing.T) {
					e := newFindTableEnv(t, send.base)
					a := e.providers["A"]
					a.hold = mode
					send.act(a, t)
					if a.inFlight == nil {
						t.Fatalf("%s sent nothing", send.name)
					}
					for _, i := range sequence {
						later[i].run(t, e)
					}
					a.release(t)
					e.quiesce(t)
					e.check(t, name)
					scripts = append(scripts, e.scripts(t, name)...)
				})
			}
		}
	}
	checkFindScripts(t, "outstanding.json", scripts)
}

// An end submitted inside an action, at every point where one could once
// land between a check and what follows it, is applied strictly after the
// action: the action's answer is what it would be without the end, and the
// end is newer than it.
func TestFindEndInsideAnActionLandsAfterIt(t *testing.T) {
	ends := map[string]func(t *testing.T, e *findTableEnv){
		"commit": func(_ *testing.T, e *findTableEnv) { e.f.fake.commitDocument() },
		"switch away": func(t *testing.T, e *findTableEnv) {
			if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, e.other); err != nil {
				t.Error(err)
			}
		},
		"detach A": func(_ *testing.T, e *findTableEnv) { e.f.manager.DetachPane(e.mounts["A"]) },
	}
	actions := []struct {
		label, name string
		act         func(p *findProvider, t *testing.T)
		points      []string
		// idle closes A's bar first, so the page holds no search when the
		// action starts.
		idle bool
	}{
		{"A", "start", func(p *findProvider, t *testing.T) { p.uiSearch(t, "beta") }, []string{"checked", "engine"}, false},
		{"A", "next", (*findProvider).uiNext, []string{"checked", "engine"}, false},
		{"A", "stop", func(p *findProvider, t *testing.T) { p.uiSearch(t, "") }, []string{"checked"}, false},
		{"A", "close", (*findProvider).uiClear, []string{"checked"}, false},
		{"B", "start", func(p *findProvider, t *testing.T) {
			p.uiOpen(t)
			p.uiSearch(t, "beta")
		}, []string{"checked", "engine"}, false},
		{"A", "reopen", (*findProvider).uiOpen, []string{"checked", "engine"}, true},
	}
	names := make([]string, 0, len(ends))
	for name := range ends {
		names = append(names, name)
	}
	sort.Strings(names)
	var scripts []findScript
	for _, action := range actions {
		for _, endName := range names {
			for _, point := range action.points {
				name := fmt.Sprintf("%s %s with %s at %s", action.name, action.label, endName, point)
				if action.idle {
					name += " on an idle page"
				}
				t.Run(name, func(t *testing.T) {
					e := newFindTableEnv(t, "search")
					p := e.providers[action.label]
					if action.idle {
						p.uiClear(t)
						e.quiesce(t)
					}
					fired := false
					e.mu.Lock()
					e.midPoint = point
					e.midEnd = func() {
						fired = true
						ends[endName](t, e)
					}
					e.mu.Unlock()
					p.hold = "answer"
					action.act(p, t)
					call := p.inFlight
					if call == nil {
						t.Fatal("the action sent nothing")
					}
					if !fired {
						t.Fatalf("the action never reached %s", point)
					}
					if call.err != nil {
						t.Fatalf("the action failed: the end landed before it finished: %v", call.err)
					}
					if call.state.Foreign {
						t.Fatalf("the action answered foreign: the end landed inside it: %+v", call.state)
					}
					// Every end of the asking find's search announced while
					// it was in flight is newer than its answer.
					for _, end := range call.events {
						if end.Mount == p.mount && end.Seq == call.intent.seq && end.Session <= call.state.Session {
							t.Fatalf("end %d is not newer than the action's answer %d", end.Session, call.state.Session)
						}
					}
					p.release(t)
					if endName == "switch away" {
						e.showPage(e.other)
					}
					if endName == "detach A" {
						e.providers["A"].dispose(t)
					}
					e.quiesce(t)
					e.check(t, name)
					scripts = append(scripts, e.scripts(t, name)...)
				})
			}
		}
	}
	checkFindScripts(t, "inside.json", scripts)
}

// Orders review found, each named: a stop made after a start the Manager
// refused, or one dropped or left unanswered, ends the search shown before
// it; a stop's failure, its retry's among them, never changes what the bar
// shows.
func TestFindNamedSequences(t *testing.T) {
	sequences := []struct {
		name, base string
		run        func(t *testing.T, e *findTableEnv)
	}{
		{"a search, a replacement too long to start, then the bar closes", "search", func(t *testing.T, e *findTableEnv) {
			a := e.providers["A"]
			a.uiSearch(t, findOversizedQuery)
			a.uiClear(t)
		}},
		{"a search, a replacement on a page already gone, then the bar closes", "search", func(t *testing.T, e *findTableEnv) {
			a := e.providers["A"]
			e.goneStart(t, a)
			a.uiClear(t)
		}},
		{"a start answered late, its replacement answered, a query still debouncing, then the bar closes", "bar", func(t *testing.T, e *findTableEnv) {
			a := e.providers["A"]
			a.hold = "answer"
			a.uiSearch(t, "alpha")
			a.uiSearch(t, "beta")
			a.release(t)
			a.uiType(t, "gamma")
			a.uiClear(t)
		}},
		{"a query too long, then a short one that finds, then the bar closes and opens", "search", func(t *testing.T, e *findTableEnv) {
			a := e.providers["A"]
			a.uiSearch(t, findOversizedQuery)
			if !a.errored {
				t.Fatal("the bar shows no error for a query too long")
			}
			a.uiSearch(t, "beta")
			a.uiReopen(t)
		}},
		{"a stop refused after its retries, then a later stop answered", "search", func(t *testing.T, e *findTableEnv) {
			a := e.providers["A"]
			e.mu.Lock()
			e.stopFail = "always"
			e.mu.Unlock()
			a.uiSearch(t, "")
			for a.inFlight == nil && a.runRetries(t) {
			}
			if !a.cleanup {
				t.Fatal("the pane shows no cleanup error for an undelivered stop")
			}
			e.mu.Lock()
			e.stopFail = ""
			e.mu.Unlock()
			e.install(t)
			a.uiSearch(t, "beta")
			a.uiClear(t)
		}},
		{"a stop refused, the next search shown, then the stop's retry refused", "search", func(t *testing.T, e *findTableEnv) {
			a := e.providers["A"]
			e.mu.Lock()
			e.stopFail = "once"
			e.mu.Unlock()
			a.uiSearch(t, "")
			a.uiSearch(t, "beta")
			e.mu.Lock()
			e.expire = true
			e.mu.Unlock()
			if !a.runRetries(t) {
				t.Fatal("the refused stop set no retry")
			}
		}},
	}
	var scripts []findScript
	for _, sequence := range sequences {
		t.Run(sequence.name, func(t *testing.T) {
			e := newFindTableEnv(t, sequence.base)
			sequence.run(t, e)
			e.quiesce(t)
			e.check(t, sequence.name)
			scripts = append(scripts, e.scripts(t, sequence.name)...)
		})
	}
	checkFindScripts(t, "named.json", scripts)
}

// checkFindScripts compares the scripts with the copy the frontend replays,
// or rewrites it with AO_UPDATE_FIND_SCRIPTS=1.
func checkFindScripts(t *testing.T, file string, scripts []findScript) {
	t.Helper()
	// Sorted first, so the name kept for scripts alike does not depend on
	// the order the sequences ran in.
	sort.SliceStable(scripts, func(i, j int) bool { return scripts[i].Name < scripts[j].Name })
	seen := map[string]bool{}
	var unique []findScript
	for _, script := range scripts {
		key, err := json.Marshal(struct {
			Mount string
			Steps []findScriptStep
			Bar   bool
		}{script.Mount, script.Steps, script.Bar})
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		unique = append(unique, script)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].Name < unique[j].Name })
	encoded, err := json.MarshalIndent(unique, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	path := findScriptsDir + file
	if updateFindScripts {
		if err := os.MkdirAll(findScriptsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if t.Failed() {
		return
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the frontend's find scripts: %v (regenerate with AO_UPDATE_FIND_SCRIPTS=1)", err)
	}
	if !bytes.Equal(current, encoded) {
		t.Fatalf("%s is out of date with this table: regenerate with AO_UPDATE_FIND_SCRIPTS=1", path)
	}
}
