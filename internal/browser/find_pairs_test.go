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
	"sync"
	"testing"
)

// The find's operations run here in ordered sequences against two pane
// mounts' providers, A holding a search and B mounted beside it: what each
// pane's reader does (open the bar, type, step, empty the query, close the
// bar), a document commit and a late one, a page switch, suspend and
// restore, detaching A, and engine failures (a start that fails, a stop that
// fails once or always). TestFindOutstandingCallAcrossLaterActions also
// leaves one call of A's outstanding, answered late or arriving late, across
// the next one and two of A's actions.
//
// Each provider is a port of browserFind.svelte.ts (findProvider): its
// queue, display generation, find ids, drop rules, answer rules and stop
// retries. It sends what that provider would; every end the Manager
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
//  1. the Manager's search state matches what its end events told each
//     provider: a provider holds a search exactly when the Manager has one
//     owned by its mount and find id, and the Manager has none of its mount
//     under another id; no call is left unanswered and no stop retry waits;
//  2. the engine holds a search exactly when a session is active or owes
//     its stop, and owes one only while the stop it is owed failed and no
//     find action has run since;
//  3. an answer names the asking mount and find id, or is marked Foreign
//     and names the search's real owner;
//  4. a search exists only on the shown active page, owned by a mounted
//     pane;
//  5. an action fails only for an engine failure, or a start the page or
//     the mount cannot take: never because its search already ended;
//  6. a provider showing results has a backend search it owns.

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
	op, mount, id string
	owner         findOwner
	state         FindState
}

// findScriptStep is one thing that happened to a provider, in order: what
// the reader did (UI, with Query for "search"), the page the pane now shows
// (Shown), a call the provider sent (Send), the answer to its oldest
// unanswered call (Answer, or Error), an end the thread's providers were
// told of (Event), or the provider's stop retry wait running out (Retry).
type findScriptStep struct {
	UI     string          `json:"ui,omitempty"`
	Query  string          `json:"query,omitempty"`
	Shown  string          `json:"shown,omitempty"`
	Send   *findScriptCall `json:"send,omitempty"`
	Answer *FindState      `json:"answer,omitempty"`
	Error  bool            `json:"error,omitempty"`
	Event  *FindState      `json:"event,omitempty"`
	Retry  bool            `json:"retry,omitempty"`
}

type findScriptCall struct {
	Kind  string `json:"kind"`
	Page  string `json:"page"`
	ID    string `json:"id"`
	Query string `json:"query,omitempty"`
}

type findScript struct {
	Name  string           `json:"name"`
	Mount string           `json:"mount"`
	Page  string           `json:"page"`
	Steps []findScriptStep `json:"steps"`
	// Bar is whether the provider's bar is open at the end: the real
	// provider must leave it so.
	Bar bool `json:"bar"`
}

// findIntent is the port's Intent.
type findIntent struct {
	kind, page, id, query string
	seq, attempt          int
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
// mirrors the function of the same name there; shown and query are the
// pane-find store's open state and query for the pane.
type findProvider struct {
	e            *findTableEnv
	label, mount string
	seq          int
	queue        []findIntent
	inFlight     *findCall
	page, id     string
	search       bool
	session      uint64
	// retries are the stops waiting on their retry timer, in the order
	// their timers were set.
	retries []findIntent
	// count and pending are view.count (-1 for an uncounted match) and
	// view.pending.
	count    int
	pending  bool
	shown    bool
	query    string
	disposed bool
	ids      int
	// hold, when set, keeps the next call outstanding until release:
	// "answer" runs it at once and answers it late, "arrive" runs it late.
	hold  string
	steps []findScriptStep
}

func (p *findProvider) record(step findScriptStep) {
	p.steps = append(p.steps, step)
}

func (p *findProvider) reset() { p.count, p.pending = 0, false }

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
	p.pending = false
}

func (p *findProvider) cancelRetry(page, id string) {
	p.retries = slices.DeleteFunc(p.retries, func(intent findIntent) bool { return intent.page == page && intent.id == id })
}

func (p *findProvider) drop(kinds ...string) {
	p.queue = slices.DeleteFunc(p.queue, func(intent findIntent) bool { return slices.Contains(kinds, intent.kind) })
}

func (p *findProvider) enqueue(t *testing.T, intent findIntent) {
	p.queue = append(p.queue, intent)
	p.drain(t)
}

func (p *findProvider) heldIntent(kind, query string) findIntent {
	return findIntent{kind: kind, page: p.page, id: p.id, query: query, seq: p.seq}
}

func (p *findProvider) enqueueStop(t *testing.T, of findIntent) {
	for _, intent := range p.queue {
		if intent.kind == FindStop && intent.page == of.page && intent.id == of.id {
			return
		}
	}
	p.enqueue(t, findIntent{kind: FindStop, page: of.page, id: of.id, seq: p.seq})
}

func (p *findProvider) retryStop(intent findIntent) {
	p.cancelRetry(intent.page, intent.id)
	if intent.attempt+1 >= findPortStopAttempts {
		return
	}
	p.retries = append(p.retries, intent)
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
		intent.seq = p.seq
		intent.attempt++
		p.enqueue(t, intent)
	}
	return true
}

func (p *findProvider) isHeld(intent findIntent) bool {
	// A provider's calls all name its own mount.
	return p.search && intent.page == p.page && intent.id == p.id
}

func (p *findProvider) drain(t *testing.T) {
	for p.inFlight == nil && len(p.queue) > 0 {
		intent := p.queue[0]
		p.queue = p.queue[1:]
		call := &findCall{intent: intent}
		p.inFlight = call
		p.record(findScriptStep{Send: &findScriptCall{Kind: intent.kind, Page: p.e.pageLabel(intent.page), ID: intent.id, Query: intent.query}})
		mode := p.hold
		p.hold = ""
		if mode != "arrive" {
			p.e.run(t, p, call)
		}
		if mode != "" {
			return
		}
		p.complete(t, call)
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
	p.complete(t, call)
	p.drain(t)
}

// complete is drain's handling of one answer. The intents it queues run
// once the call in flight is cleared, from drain.
func (p *findProvider) complete(t *testing.T, call *findCall) {
	intent := call.intent
	p.inFlight = nil
	if call.err != nil {
		p.record(findScriptStep{Error: true})
		if intent.seq == p.seq {
			p.reset()
		}
		if intent.kind == FindStop {
			p.retryStop(intent)
			return
		}
		if intent.seq == p.seq && p.isHeld(intent) {
			p.search = false
			p.drop(FindNext, FindPrev)
		}
		// A table's failure is the backend's answer, never an unknown
		// outcome: no stop follows.
		return
	}
	normal := p.e.normalize(call.state)
	p.record(findScriptStep{Answer: &normal})
	switch {
	case intent.kind == FindStop:
		// Answered: no retry follows.
	case call.state.Foreign:
		if p.isHeld(intent) {
			p.accept()
		}
	case !p.isHeld(intent):
		if call.state.Active {
			p.queueStopAfter(intent)
		}
	default:
		p.session = max(p.session, call.state.Session)
		if intent.seq == p.seq {
			p.show(call.state)
		}
	}
}

// queueStopAfter is enqueueStop from inside an answer: drain sends it next.
func (p *findProvider) queueStopAfter(of findIntent) {
	for _, intent := range p.queue {
		if intent.kind == FindStop && intent.page == of.page && intent.id == of.id {
			return
		}
	}
	p.queue = append(p.queue, findIntent{kind: FindStop, page: of.page, id: of.id, seq: p.seq})
}

func (p *findProvider) ended(state FindState) {
	if !p.search || state.PageID != p.page || state.Mount != p.mount || state.FindID != p.id {
		return
	}
	if state.Session <= p.session {
		return
	}
	p.accept()
}

// accept is the held search's end: the bar closes, which is closeBar, and
// through it the store's close calling clear.
func (p *findProvider) accept() {
	p.seq++
	p.drop(FindStart, FindNext, FindPrev)
	p.search = false
	p.id = ""
	p.reset()
	if p.shown {
		p.shown = false
		p.closeLocal()
	}
}

// closeLocal is close without a drain: the stop it could queue only when a
// search is held, and accept holds none.
func (p *findProvider) closeLocal() {
	p.seq++
	p.drop(FindStart, FindNext, FindPrev)
	p.reset()
	p.search = false
	p.id = ""
}

func (p *findProvider) close(t *testing.T) {
	p.seq++
	p.drop(FindStart, FindNext, FindPrev)
	p.reset()
	if p.search {
		p.enqueueStop(t, p.heldIntent(FindStop, ""))
	}
	p.search = false
	p.id = ""
}

func (p *findProvider) begin(t *testing.T, query string) {
	p.page = p.e.shown
	if p.id == "" {
		p.ids++
		p.id = fmt.Sprintf("%s%d", p.label, p.ids)
		p.session = 0
	}
	p.search = true
	p.cancelRetry(p.page, p.id)
	p.enqueue(t, p.heldIntent(FindStart, query))
}

// The reader's actions, as the pane makes them: each only when the pane's
// bar is in the state the reader could do it in.

// uiOpen is openPaneFind: the bar opens with the query it kept, which
// searches.
func (p *findProvider) uiOpen(t *testing.T) {
	if p.disposed || p.shown {
		return
	}
	p.record(findScriptStep{UI: "open"})
	p.shown = true
	if p.query != "" {
		p.searchQuery(t, p.query)
	}
}

// uiSearch is setPaneFindQuery, with its debounce run out at once.
func (p *findProvider) uiSearch(t *testing.T, query string) {
	if p.disposed || !p.shown || query == p.query {
		return
	}
	p.record(findScriptStep{UI: "search", Query: query})
	p.query = query
	p.searchQuery(t, query)
}

func (p *findProvider) searchQuery(t *testing.T, query string) {
	p.seq++
	p.drop(FindStart, FindNext, FindPrev)
	if query == "" {
		p.reset()
		if p.search {
			p.enqueueStop(t, p.heldIntent(FindStop, ""))
		}
		p.search = false
		return
	}
	p.pending = true
	p.begin(t, query)
}

func (p *findProvider) uiNext(t *testing.T) {
	if p.disposed || !p.shown {
		return
	}
	p.record(findScriptStep{UI: "next"})
	if !p.search || p.pending || p.count == 0 {
		return
	}
	if n := len(p.queue); n > 0 && p.queue[n-1].kind == FindPrev {
		p.queue = p.queue[:n-1]
		return
	}
	p.enqueue(t, p.heldIntent(FindNext, ""))
}

// uiClear is closePaneFind: the bar closes and keeps its query.
func (p *findProvider) uiClear(t *testing.T) {
	if p.disposed || !p.shown {
		return
	}
	p.record(findScriptStep{UI: "clear"})
	p.shown = false
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
	p.shown = false
	p.close(t)
	p.disposed = true
}

func newFindTableEnv(t *testing.T, base string) *findTableEnv {
	t.Helper()
	f := newFindFixture(t, "https://example.test/alpha-beta")
	e := &findTableEnv{
		f: f, mounts: map[string]string{"A": f.mount}, incs: map[uint64]uint64{},
		actions: map[string]int{}, lastStop: map[string]findStopRecord{},
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
		e.providers[label] = &findProvider{e: e, label: label, mount: e.mounts[label]}
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
	state, err := e.f.manager.Find(t.Context(), e.f.access, intent.page, intent.kind, intent.query, p.mount, intent.id)
	e.settle()
	var owner findOwner
	if page, ok := e.livePages()[intent.page]; ok {
		page.findMu.Lock()
		owner = findOwner{mount: page.find.owner, id: page.find.id}
		page.findMu.Unlock()
	}
	call.ran, call.state, call.err = true, state, err
	name := p.label + " " + intent.kind
	if err != nil {
		e.errs = append(e.errs, findTableError{op: name, kind: intent.kind, err: err})
	} else {
		e.answers = append(e.answers, findTableAnswer{op: name, mount: p.mount, id: intent.id, owner: owner, state: state})
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
	// Each provider's waiting stops run out, again while the engine keeps
	// refusing, until none waits.
	for _, label := range []string{"A", "B"} {
		p := e.providers[label]
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
		if !session.active && (session.owner != "" || session.id != "") {
			t.Errorf("%s: %s: no search runs, yet it names an owner %s/%s", name, page, e.labels[session.owner], session.id)
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
		owner := findOwner{mount: p.mount, id: p.id}
		session, live := sessions[p.page]
		if p.search && !(live && session.ownedBy(owner)) {
			t.Errorf("%s: provider %s holds a search on %s the Manager has not for its find %s (%+v)", name, label, e.pageLabel(p.page), p.id, session)
		}
		if p.count != 0 && !(p.search && live && session.ownedBy(owner)) {
			t.Errorf("%s: provider %s shows results with no search it owns", name, label)
		}
		for id, session := range sessions {
			if !session.active || session.owner != p.mount {
				continue
			}
			if !p.search || id != p.page || session.id != p.id {
				t.Errorf("%s: provider %s: the Manager searches %s for its mount's find %s; the provider holds %v for %s on %s", name, label, e.pageLabel(id), session.id, p.search, p.id, e.pageLabel(p.page))
			}
		}
	}
	for _, failed := range e.errs {
		allowed := errors.Is(failed.err, errFakeFindStart) || errors.Is(failed.err, errFakeFindStop) ||
			(failed.kind == FindStart && (errors.Is(failed.err, errFindNotShown) || errors.Is(failed.err, errFindNoMount)))
		if !allowed {
			t.Errorf("%s: %s failed: %v", name, failed.op, failed.err)
		}
	}
	for _, answer := range e.answers {
		if !answer.state.Foreign && (answer.state.Mount != answer.mount || answer.state.FindID != answer.id) {
			t.Errorf("%s: %s answered %s/%s to %s/%s unmarked", name, answer.op, e.labels[answer.state.Mount], answer.state.FindID, e.labels[answer.mount], answer.id)
		}
		if answer.state.Foreign && (answer.state.Mount != answer.owner.mount || answer.state.FindID != answer.owner.id) {
			t.Errorf("%s: %s answered Foreign naming %s/%s, the owner was %s/%s", name, answer.op, e.labels[answer.state.Mount], answer.state.FindID, e.labels[answer.owner.mount], answer.owner.id)
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
		out = append(out, findScript{Name: name + " / " + label, Mount: e.labels[p.mount], Page: "page-1", Steps: p.steps, Bar: p.shown})
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
						if end.FindID == call.intent.id && end.Session <= call.state.Session {
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
