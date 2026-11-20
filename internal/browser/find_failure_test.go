package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFindEngineFailureEndsTheSession(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	f.fake.mu.Lock()
	f.fake.findErr = errors.New("engine went away")
	f.fake.mu.Unlock()
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "next", "", f.mount, f.seq); err == nil || !strings.Contains(err.Error(), "engine went away") {
		t.Fatalf("failed step error = %v", err)
	}
	f.wantCall(t, "step")
	f.fake.mu.Lock()
	f.fake.findErr = nil
	f.fake.mu.Unlock()
	if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "next", "", f.mount, f.seq); err != nil || state.Active {
		t.Fatalf("a failed step left the session running: %+v, %v", state, err)
	}
	if state := f.find(t, "start", "alpha"); !state.Active || state.Session <= started.Session {
		t.Fatalf("restart after a failure = %+v", state)
	}
}

// A find that resolved its page and then waited for the page's lock while
// the page closed never reaches the closed driver: the native view is gone.
func TestFindNeverCallsTheDriverOfAPageClosedWhileItWaited(t *testing.T) {
	for _, kind := range []string{"start", "next"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			if kind == "next" {
				f.find(t, "start", "alpha")
				f.wantCall(t, "start alpha")
			}
			p, scope, err := f.manager.lookupOwnedPage(f.access, PageID(f.pageID))
			if err != nil {
				t.Fatal(err)
			}
			waiting := findWaiting(f.manager)
			p.mu.Lock()
			answered := make(chan error, 1)
			go func() {
				_, err := f.manager.Find(context.Background(), f.access, PageID(f.pageID), kind, "alpha", f.mount, f.seqFor(kind))
				answered <- err
			}()
			// Once the find resolved the page and waits for its lock, tear
			// the page down under that lock, as closing does.
			<-waiting
			p.driver.Close()
			f.manager.mu.Lock()
			delete(scope.pages, p.id)
			f.manager.mu.Unlock()
			p.mu.Unlock()
			if err := <-answered; !errors.Is(err, errPageNotFound) {
				t.Fatalf("find on a page closed while it waited: %v", err)
			}
			f.noCall(t)
		})
	}
}

// A stop whose engine call fails reports it and ends the search; the engine
// stop stays owed and is made again as its transition ends, and then by
// each next stop from that bar, whose search is gone, which reports each
// failure while the engine keeps refusing.
func TestFindStopRetriesAStopTheEngineRefused(t *testing.T) {
	for _, always := range []bool{false, true} {
		t.Run(fmt.Sprintf("always %v", always), func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			// No end submitted before is still to be applied: every
			// transition that follows is the test's own.
			f.manager.findWork.Wait()
			failures := 0
			f.fake.mu.Lock()
			f.fake.findStopErr = func(context.Context) error {
				if always || failures == 0 {
					failures++
					return errors.New("engine busy")
				}
				return nil
			}
			f.fake.mu.Unlock()
			if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "stop", "", f.mount, f.seq); err == nil {
				t.Fatal("a stop whose engine call failed reported nothing")
			}
			// The stop, then the end of its transition making the stop it
			// is still owed.
			f.wantCall(t, "stop")
			f.wantCall(t, "stop")
			if session := f.session(t); session.active || session.engine != always {
				t.Fatalf("after a refused stop the page holds %+v", session)
			}
			if !always {
				// Nothing is owed any more: the next stop asks nothing.
				if state := f.find(t, "stop", ""); !state.Foreign {
					t.Fatalf("stop after the engine recovered = %+v", state)
				}
				f.noCall(t)
				return
			}
			for round := range 2 {
				state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "stop", "", f.mount, f.seq)
				f.wantCall(t, "stop")
				f.wantCall(t, "stop")
				f.fake.mu.Lock()
				query := f.fake.findQuery
				f.fake.mu.Unlock()
				if err == nil || !state.Foreign || query == "" {
					t.Fatalf("round %d: stop against a failing engine = %+v, %v; engine %q", round, state, err, query)
				}
			}
		})
	}
}

// A start or step the engine failed is cleared with it: the engine keeps no
// search the Manager does not hold.
func TestFindFailedEngineCallIsCleared(t *testing.T) {
	for _, kind := range []string{"start", "next"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			f.fake.mu.Lock()
			f.fake.findErr = errors.New("engine failed")
			// The failing call alone fails; the stop after it succeeds.
			f.fake.findHook = func(call string) {
				f.fake.mu.Lock()
				f.fake.findErr = nil
				f.fake.mu.Unlock()
				f.calls <- call
			}
			f.fake.mu.Unlock()
			if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), kind, "beta", f.mount, f.seqFor(kind)); err == nil {
				t.Fatalf("%s did not fail", kind)
			}
			f.fake.mu.Lock()
			query := f.fake.findQuery
			f.fake.mu.Unlock()
			if query != "" {
				t.Fatalf("engine search after a failed %s = %q", kind, query)
			}
		})
	}
}

// Ends submitted while the Manager closes, from the engine's commit callback
// and from presentation changes, are either joined by Close or never
// started: no find work runs once Close has returned.
func TestFindEndsSubmittedDuringCloseAreJoined(t *testing.T) {
	for round := range 50 {
		f := newFindFixture(t, "https://example.test/alpha")
		f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		p, _, err := f.manager.lookupOwnedPage(f.access, PageID(f.pageID))
		if err != nil {
			t.Fatal(err)
		}
		var closed atomic.Bool
		var late atomic.Int32
		setFindPoint(f.manager, func(point string) {
			if point == "apply" && closed.Load() {
				late.Add(1)
			}
		})
		var submitters sync.WaitGroup
		start := make(chan struct{})
		for i := range 8 {
			submitters.Add(1)
			go func() {
				defer submitters.Done()
				<-start
				for range 20 {
					if i%2 == 0 {
						f.fake.commitDocument()
					} else {
						f.manager.endHiddenFinds([]*managedPage{p})
					}
				}
			}()
		}
		close(start)
		if err := f.manager.Close(); err != nil {
			t.Fatalf("round %d: close: %v", round, err)
		}
		closed.Store(true)
		submitters.Wait()
		// Submitted after Close returned: refused, not started.
		f.fake.commitDocument()
		f.manager.endFind(p)
		// Join any find work those started; a refused submission started none.
		f.manager.findWork.Wait()
		if n := late.Load(); n != 0 {
			t.Fatalf("round %d: %d find applies ran after Close returned", round, n)
		}
	}
}

// A find action's deadline runs from before it waits for the page lock: an
// action that waited past it is refused and changes nothing, whatever its
// kind, and so is one whose caller went while it waited.
func TestFindActionThatWaitedPastItsDeadlineChangesNothing(t *testing.T) {
	for _, why := range []string{"deadline", "caller"} {
		for _, kind := range []string{"start", "next", "previous", "stop"} {
			t.Run(why+" "+kind, func(t *testing.T) {
				f := newFindFixtureWith(t, "https://example.test/alpha", func(m *Manager) {
					if why == "deadline" {
						m.findTimeout = 200 * time.Millisecond
					}
				})
				if kind != "start" {
					f.find(t, "start", "alpha")
					f.wantCall(t, "start alpha")
				}
				p, _, err := f.manager.lookupOwnedPage(f.access, PageID(f.pageID))
				if err != nil {
					t.Fatal(err)
				}
				p.findMu.Lock()
				before := p.find
				p.findMu.Unlock()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				waiting := findWaiting(f.manager)
				p.mu.Lock()
				answered := make(chan error, 1)
				go func() {
					_, err := f.manager.Find(ctx, f.access, PageID(f.pageID), kind, "beta", f.mount, f.seqFor(kind))
					answered <- err
				}()
				// The action waits for the lock with its deadline running;
				// the lock is held until its caller went, or until the
				// deadline passed.
				action := <-waiting
				if why == "caller" {
					cancel()
				}
				<-action.Done()
				p.mu.Unlock()
				if err := <-answered; err == nil {
					t.Fatalf("%s that waited past its deadline was not refused", kind)
				}
				f.noCall(t)
				f.noEvent(t)
				p.findMu.Lock()
				after := p.find
				p.findMu.Unlock()
				if after != before {
					t.Fatalf("%s that waited past its deadline changed the find: %+v, was %+v", kind, after, before)
				}
			})
		}
	}
}

// An engine stop that never answers is bounded by its action's context
// wherever a find action makes one: a stop, and the clearing of a step the
// engine failed. The fake engine says when the stop is waiting, and the
// action's caller then goes: the action answers, the stop stays owed, and
// the next transition makes it. Nothing waits on a clock.
func TestFindStopThatNeverAnswersIsBoundedAndStaysOwed(t *testing.T) {
	for _, kind := range []string{"stop", "next"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			stopping := make(chan struct{})
			var first sync.Once
			f.fake.mu.Lock()
			if kind == "next" {
				// The step fails; the stop that clears it is the one that
				// never answers.
				f.fake.findErr = errors.New("engine failed")
				record := f.fake.findHook
				f.fake.findHook = func(call string) {
					f.fake.mu.Lock()
					f.fake.findErr = nil
					f.fake.mu.Unlock()
					record(call)
				}
			}
			// The first stop never answers before its context ends; every
			// later one answers at once, refused only if its context already
			// ended.
			f.fake.findStopErr = func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				hang := false
				first.Do(func() { hang = true })
				if !hang {
					return nil
				}
				close(stopping)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.Context().Done():
					// The test ended: let its cleanup close the page.
					return errors.New("released by the test's end")
				}
			}
			f.fake.mu.Unlock()
			// No end submitted before is still to be applied: the next
			// transition is the test's own.
			f.manager.findWork.Wait()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			answered := make(chan error, 1)
			go func() {
				_, err := f.manager.Find(ctx, f.access, PageID(f.pageID), kind, "", f.mount, f.seqFor(kind))
				answered <- err
			}()
			<-stopping
			cancel()
			// A stop reports its stop's end; a step reports its own failure.
			// The bound is reached only when the engine stop ignores the
			// action's end.
			select {
			case err := <-answered:
				if err == nil || (kind == "stop" && !errors.Is(err, context.Canceled)) {
					t.Fatalf("a %s whose engine stop never answered = %v", kind, err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("a %s's engine stop outlived the action's end", kind)
			}
			if !f.session(t).engine {
				t.Fatal("a stop that never answered was recorded as made")
			}
			// The action's own calls, its end-of-transition retry among them;
			// then the next transition makes the stop it is owed.
			for draining := true; draining; {
				select {
				case <-f.calls:
				default:
					draining = false
				}
			}
			f.find(t, "stop", "")
			f.wantCall(t, "stop")
			if f.session(t).engine {
				t.Fatal("the next transition left the owed stop owed")
			}
		})
	}
}

// A page whose search ended with its engine stop still owed is not settled:
// hiding the companion or unmounting the pane makes that stop, so no
// highlight outlives the pane.
func TestFindOwedStopIsMadeWhenThePaneGoes(t *testing.T) {
	for _, why := range []string{"detach", "hide"} {
		t.Run(why, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			refuse := true
			f.fake.mu.Lock()
			f.fake.findStopErr = func(context.Context) error {
				f.fake.mu.Lock()
				defer f.fake.mu.Unlock()
				if refuse {
					return errors.New("engine busy")
				}
				return nil
			}
			f.fake.mu.Unlock()
			f.manager.findWork.Wait()
			if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "stop", "", f.mount, f.seq); err == nil {
				t.Fatal("a refused stop reported nothing")
			}
			f.wantCall(t, "stop")
			f.wantCall(t, "stop")
			if session := f.session(t); session.active || !session.engine {
				t.Fatalf("after the refused stop the page holds %+v", session)
			}
			f.fake.mu.Lock()
			refuse = false
			f.fake.mu.Unlock()
			if why == "detach" {
				f.manager.DetachPane(f.mount)
			} else {
				hide := false
				if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
					t.Fatal(err)
				}
			}
			f.manager.findWork.Wait()
			f.wantCall(t, "stop")
			if f.session(t).engine {
				t.Fatalf("the owed stop was not made on %s", why)
			}
			f.fake.mu.Lock()
			query := f.fake.findQuery
			f.fake.mu.Unlock()
			if query != "" {
				t.Fatalf("the engine still searches for %q after %s", query, why)
			}
		})
	}
}
