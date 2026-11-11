package browser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
)

// The find's operations run here in ordered sequences from a find mount A
// holds, with mount B mounted beside it, for two mounts' providers: open,
// start, step, stop and close of each, a document commit and a late one,
// a page switch, suspend and restore, detaching A, and engine failures (a
// start that fails, a stop that fails once or always). Every operation
// that submits an end waits for it to be applied before the next runs; an
// end submitted inside an action is covered by
// TestFindEndInsideAnActionLandsAfterIt.
//
// Each mount's provider is modeled by the rules of browserFind.svelte.ts:
// it sends only what that provider would send from what it believes, an
// end that lands while its action is in flight is taken after the answer
// (the order that could mask the end), and a foreign answer to anything
// but a close, or a failed open, closes its bar. The frontend replays the
// recorded scripts against the real provider (browserFindScripts.test.ts),
// so the model cannot drift from it: regenerate them with
//
//	AO_UPDATE_FIND_SCRIPTS=1 make go-test GO_TEST_PKGS=./internal/browser GO_TEST_FLAGS='-run TestFindEveryOrderedPairKeepsTheInvariants'
//
// After each sequence quiesces:
//
//  1. a provider shows a bar exactly when the Manager holds a bar for its
//     mount, with the page's Escape claimed, and the Manager searches for a
//     mount only if its provider knows of the search;
//  2. m.finding claims the page's Escape exactly when a bar is open;
//  3. the engine holds a search exactly when the Manager believes it may,
//     and holds one with no session only while the stop it is owed failed
//     and no find action has run since;
//  4. an answer names the asking mount, or is marked Foreign and names the
//     find's real owner;
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
	f       *findFixture
	mounts  map[string]string
	labels  map[string]string
	other   string
	models  map[string]*findModel
	answers []findTableAnswer
	errs    []findTableError
	late    []func()
	baseInc uint64

	mu        sync.Mutex
	stopFail  string
	startFail bool
	actions   int
	lastStop  struct {
		at     int
		failed bool
	}
	midEnd   func()
	midPoint string
}

type findTableError struct {
	op, kind string
	err      error
}

type findTableAnswer struct {
	op, mount, owner string
	state            FindState
}

// findModel is what one mount's provider believes, by its own rules.
type findModel struct {
	bar, search, gone bool
	known             uint64
	incarnation       uint64
	count             int
	pending           []FindState
	steps             []findScriptStep
}

// findScriptStep is one thing a provider saw: an action it sent with the
// ends that landed while it was in flight and its answer, or ends that
// landed while it was idle.
type findScriptStep struct {
	Act    string      `json:"act,omitempty"`
	Query  string      `json:"query,omitempty"`
	Answer *FindState  `json:"answer,omitempty"`
	Error  bool        `json:"error,omitempty"`
	Events []FindState `json:"events,omitempty"`
}

type findScript struct {
	Name  string           `json:"name"`
	Mount string           `json:"mount"`
	Page  string           `json:"page"`
	Steps []findScriptStep `json:"steps"`
	// Bar is whether the Manager holds a bar for the mount at the end: the
	// provider must show one exactly then.
	Bar bool `json:"bar"`
}

type findTableOp struct {
	name  string
	mount string
	run   func(t *testing.T, e *findTableEnv)
}

func newFindTableEnv(t *testing.T, base string) *findTableEnv {
	t.Helper()
	f := newFindFixture(t, "https://example.test/alpha-beta")
	e := &findTableEnv{f: f, mounts: map[string]string{"A": f.mount}, models: map[string]*findModel{"A": {}, "B": {}}}
	other, err := f.manager.NewCompanionPage(t.Context(), f.access)
	if err != nil {
		t.Fatal(err)
	}
	e.other = other.ID
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	f.attach(t)
	e.mounts["B"] = f.mount
	f.mount = e.mounts["A"]
	e.labels = map[string]string{e.mounts["A"]: "mount-A", e.mounts["B"]: "mount-B"}
	e.install()
	e.baseInc = e.page(t).incarnation
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
	e.act(t, "base open", "A", FindOpen, "")
	if base == "search" {
		e.act(t, "base start", "A", FindStart, "alpha")
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

// install gives the shown fake page the table's failure injection.
func (e *findTableEnv) install() {
	fake := e.f.fake
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.findHook = nil
	fake.findAnswer = nil
	fake.findStopErr = func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		failed := e.stopFail != ""
		if e.stopFail == "once" {
			e.stopFail = ""
		}
		e.lastStop.at, e.lastStop.failed = e.actions, failed
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
}

func (e *findTableEnv) page(t *testing.T) *managedPage {
	t.Helper()
	p, _, err := e.f.manager.lookupOwnedPage(e.f.access, e.f.pageID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// settle waits until every submitted end is applied.
func (e *findTableEnv) settle() { e.f.manager.findWork.Wait() }

// collect hands every announced end to its mount's provider, unheard yet.
func (e *findTableEnv) collect() {
	for {
		select {
		case event := <-e.f.events:
			for label, mount := range e.mounts {
				if event.Find != nil && event.Find.Mount == mount {
					e.models[label].pending = append(e.models[label].pending, *event.Find)
				}
			}
		default:
			return
		}
	}
}

func (e *findTableEnv) endsHeld(model *findModel, label string, end FindState) bool {
	if (!model.bar && !model.search) || end.Mount != e.mounts[label] || end.PageID != e.f.pageID {
		return false
	}
	if end.Incarnation != model.incarnation {
		return end.Incarnation > model.incarnation
	}
	return end.Session > model.known
}

// take is the provider taking ends: a bar showing a find they end closes.
func (e *findTableEnv) take(label string, ends []FindState) {
	model := e.models[label]
	for _, end := range ends {
		if e.endsHeld(model, label, end) {
			model.bar, model.search, model.count = false, false, 0
		}
	}
}

// hear lets an idle provider take the ends it was told of.
func (e *findTableEnv) hear(label string) {
	model := e.models[label]
	if len(model.pending) == 0 {
		return
	}
	if !model.gone {
		model.steps = append(model.steps, findScriptStep{Events: e.normalize(model.pending)})
	}
	e.take(label, model.pending)
	model.pending = nil
}

// act sends one action from a mount's provider, if that provider would.
func (e *findTableEnv) act(t *testing.T, name, label, kind, query string) {
	t.Helper()
	model := e.models[label]
	if model.gone {
		return
	}
	switch kind {
	case FindOpen:
		if model.bar {
			return
		}
		model.bar = true
	case FindStart:
		if !model.bar {
			return
		}
		model.search = true
	case FindNext:
		if !model.search || model.count == 0 {
			return
		}
	case FindStop:
		if !model.search {
			return
		}
		model.search, model.count = false, 0
	case FindClose:
		if !model.bar && !model.search {
			return
		}
		model.bar, model.search, model.count = false, false, 0
	}
	mount := e.mounts[label]
	e.mu.Lock()
	e.actions++
	e.mu.Unlock()
	state, err := e.f.manager.Find(t.Context(), e.f.access, e.f.pageID, kind, query, mount)
	e.settle()
	owner := ""
	if p, _, lookErr := e.f.manager.lookupOwnedPage(e.f.access, e.f.pageID); lookErr == nil {
		p.findMu.Lock()
		owner = p.find.owner
		p.findMu.Unlock()
	}
	e.collect()
	inFlight := model.pending
	model.pending = nil
	step := findScriptStep{Act: kind, Query: query, Events: e.normalize(inFlight)}
	switch {
	case err != nil:
		e.errs = append(e.errs, findTableError{op: name, kind: kind, err: err})
		step.Error = true
		switch kind {
		case FindOpen:
			// A bar that could not open closes.
			model.bar, model.search, model.count = false, false, 0
		case FindStart, FindNext:
			model.count = 0
		}
	default:
		e.answers = append(e.answers, findTableAnswer{op: name, mount: mount, owner: owner, state: state})
		normal := e.normalize([]FindState{state})[0]
		step.Answer = &normal
		switch {
		case state.Foreign:
			if kind != FindClose {
				model.bar, model.search, model.count = false, false, 0
			}
		case state.Mount == mount && state.PageID == e.f.pageID && state.Incarnation >= model.incarnation:
			if state.Incarnation > model.incarnation {
				model.incarnation, model.known = state.Incarnation, state.Session
			} else {
				model.known = max(model.known, state.Session)
			}
		}
		if kind == FindStart || kind == FindNext {
			switch {
			case !state.Active || !state.Found:
				model.count = 0
			case state.Matches < 0:
				model.count = -1
			default:
				model.count = state.Matches
			}
		}
	}
	model.steps = append(model.steps, step)
	e.take(label, inFlight)
}

// normalize names identities as the frontend's replay does, so scripts do
// not depend on generated ids or on how many pages the process made.
func (e *findTableEnv) normalize(states []FindState) []FindState {
	out := make([]FindState, 0, len(states))
	for _, state := range states {
		if label, ok := e.labels[state.Mount]; ok {
			state.Mount = label
		}
		if state.PageID == e.f.pageID {
			state.PageID = "page-1"
		}
		if state.Incarnation != 0 {
			state.Incarnation = state.Incarnation - e.baseInc + 1
		}
		out = append(out, state)
	}
	return out
}

func (e *findTableEnv) quiesce(t *testing.T) {
	t.Helper()
	for _, deliver := range e.late {
		deliver()
	}
	e.late = nil
	e.settle()
	e.collect()
	for label := range e.models {
		e.hear(label)
	}
}

func (e *findTableEnv) check(t *testing.T, name string) {
	t.Helper()
	p := e.page(t)
	p.findMu.Lock()
	session := p.find
	p.findMu.Unlock()
	_, claimed := e.f.manager.finding.Load(p.driver.Handle())
	fake := p.driver.(*fakePage)
	fake.mu.Lock()
	engine := fake.findQuery != ""
	fake.mu.Unlock()
	e.mu.Lock()
	actions, lastStop := e.actions, e.lastStop
	e.mu.Unlock()
	for label, model := range e.models {
		if model.gone {
			continue
		}
		mine := session.owner == e.mounts[label]
		if model.bar != (mine && session.bar && claimed) {
			t.Errorf("%s: mount %s: provider bar %v, Manager bar %v owned by %s, Escape claimed %v", name, label, model.bar, session.bar, e.labels[session.owner], claimed)
		}
		if mine && session.active && !model.search {
			t.Errorf("%s: mount %s: the Manager searches for a provider that holds no search", name, label)
		}
	}
	if claimed != session.bar {
		t.Errorf("%s: Escape claimed %v with a bar open %v", name, claimed, session.bar)
	}
	if !session.idle() && (!e.f.manager.pageShown(e.f.access.ThreadID, e.f.pageID) || !e.f.manager.mountOf(e.f.access.ThreadID, session.owner)) {
		t.Errorf("%s: a find of %s is left on a page not shown, or for a pane that is gone", name, e.labels[session.owner])
	}
	for _, failed := range e.errs {
		allowed := errors.Is(failed.err, errFakeFindStart) || errors.Is(failed.err, errFakeFindStop) ||
			(failed.kind == FindOpen && (errors.Is(failed.err, errFindNotShown) || errors.Is(failed.err, errFindNoMount)))
		if !allowed {
			t.Errorf("%s: %s failed: %v", name, failed.op, failed.err)
		}
	}
	if engine != (session.active || session.engine) {
		t.Errorf("%s: engine holds a search %v; the Manager's session active %v, engine owed %v", name, engine, session.active, session.engine)
	}
	if engine && !session.active && (!lastStop.failed || lastStop.at != actions) {
		t.Errorf("%s: engine holds a search no session wants after a find action could have stopped it", name)
	}
	for _, answer := range e.answers {
		if answer.state.Mount != answer.mount && !answer.state.Foreign {
			t.Errorf("%s: %s answered mount %s to mount %s unmarked", name, answer.op, e.labels[answer.state.Mount], e.labels[answer.mount])
		}
		if answer.state.Foreign && answer.state.Mount != answer.owner {
			t.Errorf("%s: %s answered Foreign naming %s, the owner was %s", name, answer.op, e.labels[answer.state.Mount], e.labels[answer.owner])
		}
	}
}

// scripts are the providers' recorded views of the sequence.
func (e *findTableEnv) scripts(t *testing.T, name string) []findScript {
	t.Helper()
	p := e.page(t)
	p.findMu.Lock()
	session := p.find
	p.findMu.Unlock()
	var out []findScript
	for _, label := range []string{"A", "B"} {
		model := e.models[label]
		if len(model.steps) == 0 {
			continue
		}
		bar := !model.gone && session.owner == e.mounts[label] && session.bar
		out = append(out, findScript{Name: name + " / " + label, Mount: e.labels[e.mounts[label]], Page: "page-1", Steps: model.steps, Bar: bar})
	}
	return out
}

func findTableOps() []findTableOp {
	action := func(name, label, kind, query string) findTableOp {
		return findTableOp{name: name, mount: label, run: func(t *testing.T, e *findTableEnv) {
			e.act(t, name, label, kind, query)
		}}
	}
	other := func(name string, run func(t *testing.T, e *findTableEnv)) findTableOp {
		return findTableOp{name: name, run: func(t *testing.T, e *findTableEnv) {
			run(t, e)
			e.settle()
		}}
	}
	return []findTableOp{
		action("open A", "A", FindOpen, ""),
		action("open B", "B", FindOpen, ""),
		action("start A", "A", FindStart, "beta"),
		action("start B", "B", FindStart, "beta"),
		action("step A", "A", FindNext, ""),
		action("step B", "B", FindNext, ""),
		action("stop A", "A", FindStop, ""),
		action("stop B", "B", FindStop, ""),
		action("close A", "A", FindClose, ""),
		action("close B", "B", FindClose, ""),
		other("commit", func(_ *testing.T, e *findTableEnv) { e.f.fake.commitDocument() }),
		other("late commit", func(_ *testing.T, e *findTableEnv) {
			e.late = append(e.late, e.f.fake.commitDocumentLate())
		}),
		other("switch away", func(t *testing.T, e *findTableEnv) {
			if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, e.other); err != nil {
				t.Fatal(err)
			}
		}),
		other("switch back", func(t *testing.T, e *findTableEnv) {
			if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, e.f.pageID); err != nil {
				t.Fatal(err)
			}
			e.f.fake = e.page(t).driver.(*fakePage)
			e.install()
		}),
		other("suspend and restore", func(t *testing.T, e *findTableEnv) {
			if err := e.f.manager.SuspendThread(t.Context(), e.f.access.ThreadID); err != nil {
				t.Fatal(err)
			}
			if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, e.f.pageID); err != nil {
				t.Fatal(err)
			}
			e.f.fake = e.page(t).driver.(*fakePage)
			e.install()
		}),
		{name: "detach A", mount: "A", run: func(t *testing.T, e *findTableEnv) {
			e.f.manager.DetachPane(e.mounts["A"])
			e.settle()
			// The pane's provider closes what it held as it goes.
			e.act(t, "detach A close", "A", FindClose, "")
			e.models["A"].gone = true
		}},
		other("start fails", func(_ *testing.T, e *findTableEnv) {
			e.mu.Lock()
			e.startFail = true
			e.mu.Unlock()
			e.install()
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
		other("engine recovers", func(_ *testing.T, e *findTableEnv) {
			e.mu.Lock()
			e.stopFail, e.startFail = "", false
			e.mu.Unlock()
			e.install()
		}),
	}
}

// runFindSequence runs ops in order. An end lands on an idle provider at
// once; one whose next action is already in flight takes it after.
func runFindSequence(t *testing.T, e *findTableEnv, ops []findTableOp) {
	t.Helper()
	for i, op := range ops {
		op.run(t, e)
		e.collect()
		next := ""
		if i+1 < len(ops) {
			next = ops[i+1].mount
		}
		for label := range e.models {
			if label != next {
				e.hear(label)
			}
		}
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

// The operations involved in rounds 2 to 4 of review, in every ordered
// triple from a mount holding a bar and a search.
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
		label, kind, query string
		points             []string
		// idle closes A's find first, so the page holds none when the
		// action starts.
		idle bool
	}{
		{"A", FindStart, "beta", []string{"checked", "engine"}, false},
		{"A", FindNext, "", []string{"checked", "engine"}, false},
		{"A", FindStop, "", []string{"checked"}, false},
		{"A", FindClose, "", []string{"checked"}, false},
		{"B", FindOpen, "", []string{"checked"}, false},
		{"A", FindOpen, "", []string{"checked"}, true},
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
				name := fmt.Sprintf("%s %s with %s at %s", action.kind, action.label, endName, point)
				if action.idle {
					name += " on an idle page"
				}
				t.Run(name, func(t *testing.T) {
					e := newFindTableEnv(t, "search")
					if action.idle {
						e.act(t, "close A", "A", FindClose, "")
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
					before := len(e.answers)
					e.act(t, name, action.label, action.kind, action.query)
					if !fired {
						t.Fatalf("the action never reached %s", point)
					}
					if len(e.answers) != before+1 {
						t.Fatal("the action failed: the end landed before it finished")
					}
					answer := e.answers[before].state
					if answer.Foreign {
						t.Fatalf("the action answered foreign: the end landed inside it: %+v", answer)
					}
					// Every end of the asking mount's find that landed while
					// its action was in flight is newer than its answer.
					steps := e.models[action.label].steps
					for _, end := range steps[len(steps)-1].Events {
						if end.Session <= answer.Session {
							t.Fatalf("end %d is not newer than the action's answer %d", end.Session, answer.Session)
						}
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
