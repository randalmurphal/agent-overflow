package browser

import (
	"fmt"
	"sync"
	"testing"
)

// The find's operations that queue behind the page lock, or land while one
// waits for it, run here in every ordered pair from a find mount A holds,
// with mount B mounted beside it. Run one after another, they are the
// orders the page lock can give them: an action that waited behind another
// runs after it, whatever it was asked before. The engine stop an end owes
// is its own operation, taken from the Manager instead of a goroutine, so
// it can run before or after the action that overtook it.
//
// Each pair is judged against a model of each mount's provider
// (browserFind.svelte.ts): an action's answer is taken before an end that
// landed while it was in flight, which is the order that could mask the
// end. After each pair, with every owed stop made:
//
//  1. a provider shows a bar exactly when the Manager holds a bar for its
//     mount, and the Manager searches for a mount only if its provider
//     knows of the search;
//  2. m.finding claims the page's Escape exactly when a bar is open;
//  3. the engine holds a search exactly when a session is active;
//  4. an answer names the asking mount, or is marked Foreign and names the
//     find's real owner.

type findPairEnv struct {
	f       *findFixture
	mounts  map[string]string
	other   string
	mu      sync.Mutex
	stops   []func()
	late    []func()
	models  map[string]*findModel
	answers []findPairAnswer
}

type findPairAnswer struct {
	op, mount string
	state     FindState
	owner     string
}

// findModel is what one mount's provider believes, by its own rules.
type findModel struct {
	bar, search bool
	known       uint64
	errored     bool
	gone        bool
	pending     []FindState
}

type findPairOp struct {
	name string
	// mount is the mount a find action runs for, "" for an operation that
	// is not one.
	mount string
	run   func(t *testing.T, e *findPairEnv)
}

func (e *findPairEnv) act(t *testing.T, name, label, kind, query string) {
	t.Helper()
	mount := e.mounts[label]
	model := e.models[label]
	switch kind {
	case FindOpen:
		model.bar, model.errored = true, false
	case FindStart:
		model.search, model.errored = true, false
	case FindStop:
		model.search = false
	case FindClose:
		model.bar, model.search = false, false
	}
	state, err := e.f.manager.Find(t.Context(), e.f.access, e.f.pageID, kind, query, mount)
	p := e.page(t)
	p.findMu.Lock()
	owner := p.find.owner
	p.findMu.Unlock()
	if err != nil {
		if kind == FindOpen || kind == FindStart {
			model.errored = true
		}
		return
	}
	e.answers = append(e.answers, findPairAnswer{op: name, mount: mount, state: state, owner: owner})
	if !state.Foreign && state.Mount == mount && state.PageID == e.f.pageID {
		model.known = max(model.known, state.Session)
	}
}

func (e *findPairEnv) page(t *testing.T) *managedPage {
	t.Helper()
	p, _, err := e.f.manager.lookupOwnedPage(e.f.access, e.f.pageID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// collect hands every announced end to its mount's provider, unheard yet.
func (e *findPairEnv) collect() {
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

// hear lets a provider take the ends it was told of.
func (e *findPairEnv) hear(label string) {
	model := e.models[label]
	for _, end := range model.pending {
		if (model.bar || model.search) && end.Session > model.known {
			model.bar, model.search = false, false
		}
	}
	model.pending = nil
}

func (e *findPairEnv) settle(t *testing.T) {
	t.Helper()
	for range 4 {
		e.mu.Lock()
		stops, late := e.stops, e.late
		e.stops, e.late = nil, nil
		e.mu.Unlock()
		for _, deliver := range late {
			deliver()
		}
		for _, stop := range stops {
			stop()
		}
	}
	e.collect()
	for label := range e.models {
		e.hear(label)
	}
}

func (e *findPairEnv) check(t *testing.T) {
	t.Helper()
	p := e.page(t)
	p.findMu.Lock()
	session := p.find
	p.findMu.Unlock()
	_, claimed := e.f.manager.finding.Load(e.f.fake.Handle())
	e.f.fake.mu.Lock()
	engine := e.f.fake.findQuery != ""
	e.f.fake.mu.Unlock()
	for label, model := range e.models {
		if model.gone {
			continue
		}
		mount := e.mounts[label]
		mine := session.owner == mount
		if !model.errored && model.bar != (mine && session.bar) {
			t.Errorf("mount %s: provider bar %v, Manager bar %v owned by %q", label, model.bar, session.bar, session.owner)
		}
		if mine && session.active && !model.search {
			t.Errorf("mount %s: the Manager searches for a provider that holds no search", label)
		}
	}
	if claimed != session.bar {
		t.Errorf("Escape claimed %v with a bar open %v", claimed, session.bar)
	}
	if engine != session.active {
		t.Errorf("engine holds a search %v with a session active %v", engine, session.active)
	}
	for _, answer := range e.answers {
		if answer.state.Mount != answer.mount && !answer.state.Foreign {
			t.Errorf("%s answered mount %q to mount %q unmarked", answer.op, answer.state.Mount, answer.mount)
		}
		if answer.state.Foreign && answer.state.Mount != answer.owner {
			t.Errorf("%s answered Foreign naming %q, the owner was %q", answer.op, answer.state.Mount, answer.owner)
		}
	}
}

func findPairOps() []findPairOp {
	action := func(name, label, kind, query string) findPairOp {
		return findPairOp{name: name, mount: label, run: func(t *testing.T, e *findPairEnv) {
			e.act(t, name, label, kind, query)
		}}
	}
	return []findPairOp{
		action("open A", "A", FindOpen, ""),
		action("open B", "B", FindOpen, ""),
		action("start A", "A", FindStart, "beta"),
		action("start B", "B", FindStart, "beta"),
		action("step A", "A", FindNext, ""),
		action("stop A", "A", FindStop, ""),
		action("close A", "A", FindClose, ""),
		action("close B", "B", FindClose, ""),
		{name: "engine stop", run: func(_ *testing.T, e *findPairEnv) {
			e.mu.Lock()
			stops := e.stops
			e.stops = nil
			e.mu.Unlock()
			for _, stop := range stops {
				stop()
			}
		}},
		{name: "commit", run: func(_ *testing.T, e *findPairEnv) { e.f.fake.commitDocument() }},
		{name: "late commit", run: func(_ *testing.T, e *findPairEnv) {
			deliver := e.f.fake.commitDocumentLate()
			e.mu.Lock()
			e.late = append(e.late, deliver)
			e.mu.Unlock()
		}},
		{name: "hide", run: func(t *testing.T, e *findPairEnv) {
			hide := false
			if _, err := e.f.manager.Visibility(t.Context(), e.f.access, &hide, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "switch away", run: func(t *testing.T, e *findPairEnv) {
			if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, e.other); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "detach A", run: func(_ *testing.T, e *findPairEnv) {
			e.f.manager.DetachPane(e.mounts["A"])
			e.models["A"].gone = true
		}},
	}
}

func newFindPairEnv(t *testing.T, base string) *findPairEnv {
	t.Helper()
	f := newFindFixture(t, "https://example.test/alpha-beta")
	e := &findPairEnv{f: f, mounts: map[string]string{"A": f.mount}, models: map[string]*findModel{"A": {}, "B": {}}}
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
	f.fake.mu.Lock()
	f.fake.findHook = nil
	f.fake.mu.Unlock()
	f.manager.deferEngineStop = func(stop func()) {
		e.mu.Lock()
		e.stops = append(e.stops, stop)
		e.mu.Unlock()
	}
	e.act(t, "base open", "A", FindOpen, "")
	if base == "search" {
		e.act(t, "base start", "A", FindStart, "alpha")
	}
	e.settle(t)
	e.answers = nil
	e.check(t)
	return e
}

func TestFindEveryOrderedPairKeepsTheInvariants(t *testing.T) {
	ops := findPairOps()
	for _, base := range []string{"search", "bar"} {
		for _, first := range ops {
			for _, second := range ops {
				t.Run(fmt.Sprintf("%s/%s then %s", base, first.name, second.name), func(t *testing.T) {
					e := newFindPairEnv(t, base)
					first.run(t, e)
					e.collect()
					// An end lands on a provider at once unless its next
					// action is already in flight.
					for label := range e.models {
						if second.mount != label {
							e.hear(label)
						}
					}
					second.run(t, e)
					e.settle(t)
					e.check(t)
				})
			}
		}
	}
}
