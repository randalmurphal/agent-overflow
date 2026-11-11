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
// mounts' providers, A holding a find and B mounted beside it: what each
// pane's reader does (open, type, step, empty the query, close), a document
// commit and a late one, a page switch, suspend and restore, detaching A,
// and engine failures (a start that fails, a stop that fails once or
// always). TestFindOutstandingCallAcrossLaterActions also leaves one call of
// A's outstanding, answered late or arriving late, across the next one and
// two of A's actions.
//
// Each provider is a port of browserFind.svelte.ts (findProvider): its
// queue, display generation, open ids, drop rules and answer rules. It sends
// what that provider would; every end the Manager announces reaches both
// providers when it is announced, in flight or not. The frontend replays the
// recorded scripts against the real provider (browserFindScripts.test.ts),
// which must send exactly the recorded calls, so the port cannot drift from
// it: regenerate them with
//
//	AO_UPDATE_FIND_SCRIPTS=1 make go-test GO_TEST_PKGS=./internal/browser GO_TEST_FLAGS='-run TestFind'
//
// After each sequence quiesces:
//
//  1. a provider shows a bar exactly when the Manager holds a bar for its
//     mount and open, with the page's Escape claimed, and the Manager
//     searches for an open only if its provider knows of the search; no
//     call is left unanswered;
//  2. m.finding claims a page's Escape exactly when a bar is open on it;
//  3. the engine holds a search exactly when the Manager believes it may,
//     and holds one with no session only while the stop it is owed failed
//     and no find action has run since;
//  4. an answer names the asking mount and open, or is marked Foreign and
//     names the find's real owner;
//  5. a find exists only on the shown active page, owned by a mounted pane;
//  6. an action fails only for an engine failure, or an open the page or
//     the mount cannot take: never because its find already ended.

// updateFindScripts rewrites the scripts the frontend replays.
var updateFindScripts = os.Getenv("AO_UPDATE_FIND_SCRIPTS") == "1"

// findScriptsDir holds the scripts, one file per table.
const findScriptsDir = "../../frontend/src/test/fixtures/browserFind/"

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
	op, mount, open string
	owner           findOwner
	state           FindState
}

// findScriptStep is one thing that happened to a provider, in order: what
// the reader did (UI, with Query for "search"), the page the pane now shows
// (Shown), a call the provider sent (Send), the answer to its oldest
// unanswered call (Answer, or Error), or an end the thread's providers were
// told of (Event).
type findScriptStep struct {
	UI     string          `json:"ui,omitempty"`
	Query  string          `json:"query,omitempty"`
	Shown  string          `json:"shown,omitempty"`
	Send   *findScriptCall `json:"send,omitempty"`
	Answer *FindState      `json:"answer,omitempty"`
	Error  bool            `json:"error,omitempty"`
	Event  *FindState      `json:"event,omitempty"`
}

type findScriptCall struct {
	Kind  string `json:"kind"`
	Page  string `json:"page"`
	Open  string `json:"open"`
	Query string `json:"query,omitempty"`
}

type findScript struct {
	Name  string           `json:"name"`
	Mount string           `json:"mount"`
	Page  string           `json:"page"`
	Steps []findScriptStep `json:"steps"`
	// Bar is whether the Manager holds a bar for the provider's open at the
	// end: the provider must show one exactly then.
	Bar bool `json:"bar"`
}

// findIntent is the port's Intent.
type findIntent struct {
	kind, page, open, query string
	seq                     int
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
// mirrors the function of the same name there.
type findProvider struct {
	e            *findTableEnv
	label, mount string
	seq          int
	queue        []findIntent
	inFlight     *findCall
	page, open   string
	bar, search  bool
	// count and pending are view.count (-1 for an uncounted match) and
	// view.pending.
	count    int
	pending  bool
	shown    bool
	disposed bool
	opens    int
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

func (p *findProvider) drop(kinds ...string) {
	p.queue = slices.DeleteFunc(p.queue, func(intent findIntent) bool { return slices.Contains(kinds, intent.kind) })
}

func (p *findProvider) enqueue(t *testing.T, kind, query string) {
	p.queue = append(p.queue, findIntent{kind: kind, page: p.page, open: p.open, query: query, seq: p.seq})
	p.drain(t)
}

func (p *findProvider) enqueueClose(t *testing.T) {
	for _, intent := range p.queue {
		if intent.kind == FindClose && intent.open == p.open {
			return
		}
	}
	p.enqueue(t, FindClose, "")
}

func (p *findProvider) holdShown(t *testing.T) {
	if (p.bar || p.search) && p.page != p.e.shown {
		p.enqueueClose(t)
	}
	p.bar, p.search = false, false
	p.page = p.e.shown
	p.opens++
	p.open = fmt.Sprintf("%s%d", p.label, p.opens)
}

func (p *findProvider) holds(intent findIntent) bool {
	return intent.open == p.open && (p.bar || p.search)
}

func (p *findProvider) drain(t *testing.T) {
	for p.inFlight == nil && len(p.queue) > 0 {
		intent := p.queue[0]
		p.queue = p.queue[1:]
		call := &findCall{intent: intent}
		p.inFlight = call
		p.record(findScriptStep{Send: &findScriptCall{Kind: intent.kind, Page: p.e.pageLabel(intent.page), Open: intent.open, Query: intent.query}})
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

func (p *findProvider) complete(call *findCall) {
	intent := call.intent
	p.inFlight = nil
	if call.err != nil {
		p.record(findScriptStep{Error: true})
		if intent.seq == p.seq {
			p.reset()
		}
		if intent.kind == FindOpen && p.holds(intent) {
			p.accept()
		}
		return
	}
	normal := p.e.normalize(call.state)
	p.record(findScriptStep{Answer: &normal})
	switch {
	case call.state.Foreign:
		if intent.kind != FindClose && p.holds(intent) {
			p.accept()
		}
	case intent.seq == p.seq && (intent.kind == FindStart || intent.kind == FindNext || intent.kind == FindPrev):
		p.show(call.state)
	}
}

func (p *findProvider) ended(state FindState) {
	if (p.bar || p.search) && state.Open == p.open {
		p.accept()
	}
}

func (p *findProvider) accept() {
	p.seq++
	p.drop(FindOpen, FindStart, FindNext, FindPrev, FindStop)
	p.bar, p.search = false, false
	p.reset()
	p.shown = false
}

func (p *findProvider) close(t *testing.T) {
	p.seq++
	p.drop(FindOpen, FindStart, FindNext, FindPrev, FindStop)
	p.reset()
	if p.bar || p.search {
		p.enqueueClose(t)
	}
	p.bar, p.search = false, false
}

// The reader's actions, as the pane makes them: each only when the pane's
// bar is in the state the reader could do it in.

func (p *findProvider) uiOpen(t *testing.T) {
	if p.disposed || p.shown {
		return
	}
	p.record(findScriptStep{UI: "opened"})
	p.shown = true
	p.seq++
	p.reset()
	p.holdShown(t)
	p.bar = true
	p.enqueue(t, FindOpen, "")
}

// uiSearch is search(query) with its debounce run out at once.
func (p *findProvider) uiSearch(t *testing.T, query string) {
	if p.disposed || !p.shown {
		return
	}
	p.record(findScriptStep{UI: "search", Query: query})
	p.seq++
	p.drop(FindStart, FindNext, FindPrev)
	if query == "" {
		p.reset()
		if p.search {
			p.enqueue(t, FindStop, "")
		}
		p.search = false
		return
	}
	if !p.bar {
		p.reset()
		return
	}
	p.pending = true
	p.search = true
	p.enqueue(t, FindStart, query)
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
	p.enqueue(t, FindNext, "")
}

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
	state, err := e.f.manager.Find(t.Context(), e.f.access, intent.page, intent.kind, intent.query, p.mount, intent.open)
	e.settle()
	var owner findOwner
	if page, ok := e.livePages()[intent.page]; ok {
		page.findMu.Lock()
		owner = findOwner{mount: page.find.owner, open: page.find.open}
		page.findMu.Unlock()
	}
	call.ran, call.state, call.err = true, state, err
	name := p.label + " " + intent.kind
	if err != nil {
		e.errs = append(e.errs, findTableError{op: name, kind: intent.kind, err: err})
	} else {
		e.answers = append(e.answers, findTableAnswer{op: name, mount: p.mount, open: intent.open, owner: owner, state: state})
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
}

func (e *findTableEnv) check(t *testing.T, name string) {
	t.Helper()
	pages := e.livePages()
	sessions := map[string]findSession{}
	claims := map[string]bool{}
	for id, p := range pages {
		p.findMu.Lock()
		session := p.find
		p.findMu.Unlock()
		sessions[id] = session
		_, claimed := e.f.manager.finding.Load(p.driver.Handle())
		claims[id] = claimed
		fake := p.driver.(*fakePage)
		fake.mu.Lock()
		engine := fake.findQuery != ""
		fake.mu.Unlock()
		page := e.pageLabel(id)
		e.mu.Lock()
		actions, lastStop := e.actions[id], e.lastStop[id]
		e.mu.Unlock()
		if claimed != session.bar {
			t.Errorf("%s: %s: Escape claimed %v with a bar open %v", name, page, claimed, session.bar)
		}
		if !session.idle() && (!e.f.manager.pageShown(e.f.access.ThreadID, id) || !e.f.manager.mountOf(e.f.access.ThreadID, session.owner)) {
			t.Errorf("%s: %s: a find of %s is left on a page not shown, or for a pane that is gone", name, page, e.labels[session.owner])
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
		if p.disposed {
			continue
		}
		if p.shown != p.bar {
			t.Errorf("%s: provider %s shows a bar %v holding one %v", name, label, p.shown, p.bar)
		}
		if session, live := sessions[p.page]; p.bar && !(live && session.ownedBy(p.mount, p.open) && session.bar && claims[p.page]) {
			t.Errorf("%s: provider %s shows a bar on %s the Manager does not hold for its open %s (%+v)", name, label, e.pageLabel(p.page), p.open, session)
		}
		for id, session := range sessions {
			mine := session.ownedBy(p.mount, p.open)
			if mine && session.bar && !p.bar {
				t.Errorf("%s: provider %s shows no bar; the Manager holds one on %s for its open", name, label, e.pageLabel(id))
			}
			if !mine && session.owner == p.mount && !session.idle() {
				t.Errorf("%s: provider %s: the Manager holds a find on %s of open %s, the provider's is %s", name, label, e.pageLabel(id), session.open, p.open)
			}
			if mine && session.active && !p.search {
				t.Errorf("%s: provider %s: the Manager searches for an open that holds no search", name, label)
			}
		}
	}
	for _, failed := range e.errs {
		allowed := errors.Is(failed.err, errFakeFindStart) || errors.Is(failed.err, errFakeFindStop) ||
			(failed.kind == FindOpen && (errors.Is(failed.err, errFindNotShown) || errors.Is(failed.err, errFindNoMount)))
		if !allowed {
			t.Errorf("%s: %s failed: %v", name, failed.op, failed.err)
		}
	}
	for _, answer := range e.answers {
		if !answer.state.Foreign && (answer.state.Mount != answer.mount || answer.state.Open != answer.open) {
			t.Errorf("%s: %s answered %s/%s to %s/%s unmarked", name, answer.op, e.labels[answer.state.Mount], answer.state.Open, e.labels[answer.mount], answer.open)
		}
		if answer.state.Foreign && (answer.state.Mount != answer.owner.mount || answer.state.Open != answer.owner.open) {
			t.Errorf("%s: %s answered Foreign naming %s/%s, the owner was %s/%s", name, answer.op, e.labels[answer.state.Mount], answer.state.Open, e.labels[answer.owner.mount], answer.owner.open)
		}
	}
}

// scripts are the providers' recorded views of the sequence.
func (e *findTableEnv) scripts(t *testing.T, name string) []findScript {
	t.Helper()
	pages := e.livePages()
	var out []findScript
	for _, label := range []string{"A", "B"} {
		p := e.providers[label]
		if len(p.steps) == 0 {
			continue
		}
		bar := false
		if page, ok := pages[p.page]; ok && !p.disposed {
			page.findMu.Lock()
			bar = page.find.ownedBy(p.mount, p.open) && page.find.bar
			page.findMu.Unlock()
		}
		out = append(out, findScript{Name: name + " / " + label, Mount: e.labels[p.mount], Page: "page-1", Steps: p.steps, Bar: bar})
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
	var scripts []findScript
	for _, first := range ops {
		for _, second := range ops {
			for _, third := range ops {
				name := fmt.Sprintf("%s, %s, %s", first.name, second.name, third.name)
				t.Run(name, func(t *testing.T) {
					e := newFindTableEnv(t, "search")
					runFindSequence(t, e, []findTableOp{first, second, third})
					e.check(t, name)
					scripts = append(scripts, e.scripts(t, name)...)
				})
			}
		}
	}
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
		{"open", "none", (*findProvider).uiOpen},
		{"type", "search", func(p *findProvider, t *testing.T) { p.uiSearch(t, "beta") }},
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
		{"type", func(t *testing.T, e *findTableEnv) { e.providers["A"].uiSearch(t, "alpha") }},
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
		// idle closes A's find first, so the page holds none when the
		// action starts.
		idle bool
	}{
		{"A", "start", func(p *findProvider, t *testing.T) { p.uiSearch(t, "beta") }, []string{"checked", "engine"}, false},
		{"A", "next", (*findProvider).uiNext, []string{"checked", "engine"}, false},
		{"A", "stop", func(p *findProvider, t *testing.T) { p.uiSearch(t, "") }, []string{"checked"}, false},
		{"A", "close", (*findProvider).uiClear, []string{"checked"}, false},
		{"B", "open", (*findProvider).uiOpen, []string{"checked"}, false},
		{"A", "open", (*findProvider).uiOpen, []string{"checked"}, true},
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
					// Every end of the asking open's find announced while it
					// was in flight is newer than its answer.
					for _, end := range call.events {
						if end.Open == call.intent.open && end.Session <= call.state.Session {
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
