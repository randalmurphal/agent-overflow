package browser

import (
	"fmt"
	"sort"
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
// one and two of A's actions; TestFindNamedSequences runs named orders.
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

// An end submitted inside an action, at every point between a check and
// what follows it, is applied strictly after the
// action: the action's answer is what it would be without the end, and the
// end is newer than it.
func TestFindEndInsideAnActionLandsAfterIt(t *testing.T) {
	ends := map[string]func(t *testing.T, e *findTableEnv){
		"commit": func(_ *testing.T, e *findTableEnv) { e.f.fake.commitDocument() },
		"switch away": func(t *testing.T, e *findTableEnv) {
			if err := e.f.manager.ActivateCompanionPage(t.Context(), e.f.access, PageID(e.other)); err != nil {
				t.Error(err)
			}
		},
		"detach A": func(_ *testing.T, e *findTableEnv) { e.f.manager.DetachPane(MountID(e.mounts["A"])) },
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

// Named orders: a stop made after a start left unanswered ends the search
// shown before it; a stop's failure, its retry's among them, never changes
// what the bar shows. A stop after a start the Manager refused is in the
// pairs table.
func TestFindNamedSequences(t *testing.T) {
	sequences := []struct {
		name, base string
		run        func(t *testing.T, e *findTableEnv)
	}{
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
