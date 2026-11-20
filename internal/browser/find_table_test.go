package browser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

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

func newFindTableEnv(t *testing.T, base string) *findTableEnv {
	t.Helper()
	f := newFindFixture(t, "https://example.test/alpha-beta")
	e := &findTableEnv{
		f: f, mounts: map[string]string{"A": string(f.mount)}, incs: map[uint64]uint64{},
		actions: map[string]int{}, lastStop: map[string]findStopRecord{},
		failed: map[string]map[uint64]bool{"A": {}, "B": {}}, undelivered: map[string]uint64{},
	}
	other, err := f.manager.NewCompanionPage(t.Context(), f.access, "")
	if err != nil {
		t.Fatal(err)
	}
	e.other = other.ID
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(f.pageID)); err != nil {
		t.Fatal(err)
	}
	e.shown = f.pageID
	f.attach(t)
	e.mounts["B"] = string(f.mount)
	f.mount = MountID(e.mounts["A"])
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
func (e *findTableEnv) livePages() map[PageID]*managedPage {
	pages := map[PageID]*managedPage{}
	for _, id := range []string{e.f.pageID, e.other} {
		if p, _, err := e.f.manager.lookupOwnedPage(e.f.access, PageID(id)); err == nil {
			pages[PageID(id)] = p
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
			e.lastStop[string(id)] = findStopRecord{at: e.actions[string(id)], failed: failed}
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
	p, _, err := e.f.manager.lookupOwnedPage(e.f.access, PageID(e.f.pageID))
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
	state, err := e.f.manager.Find(ctx, e.f.access, PageID(intent.page), intent.kind, intent.query, MountID(p.mount), intent.seq)
	e.settle()
	var owner findOwner
	if page, ok := e.livePages()[PageID(intent.page)]; ok {
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
		sessions[string(id)] = session
		fake := p.driver.(*fakePage)
		fake.mu.Lock()
		engine := fake.findQuery != ""
		fake.mu.Unlock()
		page := e.pageLabel(string(id))
		e.mu.Lock()
		actions, lastStop := e.actions[string(id)], e.lastStop[string(id)]
		e.mu.Unlock()
		if session.active && (!e.f.manager.pageShown(e.f.access.tb, id) || !e.f.manager.mountOf(e.f.access.tb, session.owner)) {
			t.Errorf("%s: %s: a search of %s is left on a page not shown, or for a pane that is gone", name, page, e.labels[string(session.owner)])
		}
		if !session.active && (session.owner != "" || session.seq != 0) {
			t.Errorf("%s: %s: no search runs, yet it names an owner %s/%d", name, page, e.labels[string(session.owner)], session.seq)
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
		held := p.showing != 0 && live && session.ownedBy(findOwner{mount: MountID(p.mount), seq: p.showing})
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
			if session.active && session.owner == MountID(p.mount) && (!held || id != p.page) {
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
			if MountID(answer.state.Mount) != answer.owner.mount || answer.state.Seq != answer.owner.seq {
				t.Errorf("%s: %s answered Foreign naming %s/%d, the owner was %s/%d", name, answer.op, e.labels[answer.state.Mount], answer.state.Seq, e.labels[string(answer.owner.mount)], answer.owner.seq)
			}
			continue
		}
		named := answer.state.Seq == answer.seq || (answer.kind == FindStop && answer.state.Seq <= answer.seq)
		if answer.state.Mount != answer.mount || !named {
			t.Errorf("%s: %s answered %s/%d to %s/%d unmarked", name, answer.op, e.labels[answer.state.Mount], answer.state.Seq, e.labels[answer.mount], answer.seq)
		}
	}
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
			if err := e.f.manager.SuspendThread(t.Context(), e.f.access.ThreadID()); err != nil {
				t.Fatal(err)
			}
			e.switchTo(t, e.f.pageID)
		}),
		other("detach A", func(t *testing.T, e *findTableEnv) {
			e.f.manager.DetachPane(MountID(e.mounts["A"]))
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
	if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, PageID(id)); err != nil {
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
