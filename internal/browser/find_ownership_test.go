package browser

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// An end submitted while the page is busy is applied, with the engine stop
// it owes, before any action that comes after it: the stop never lands on
// a search started later.
func TestFindEndDoesNotStopANewerSession(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	p, _, err := f.manager.lookupOwnedPage(f.access, PageID(f.pageID))
	if err != nil {
		t.Fatal(err)
	}
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	// Hold the page so the end waits behind the page's operation.
	p.mu.Lock()
	f.fake.commitDocument()
	f.noEvent(t)
	p.mu.Unlock()
	f.wantEnded(t, 0)
	f.wantCall(t, "stop")
	restarted := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	if state := f.find(t, "next", ""); !state.Active || state.Session != restarted.Session {
		t.Fatalf("the late stop ended the new find: %+v", state)
	}
	f.wantCall(t, "step")
	select {
	case call := <-f.calls:
		t.Fatalf("unexpected engine call %q after the new find", call)
	case <-time.After(50 * time.Millisecond):
	}
}

// An end that lands while a start's engine call runs (the page is switched
// away, or commits another document) is applied after the start: the start
// answers its search, and the end that follows is newer and stops the
// search.
func TestFindEndDuringAStartLandsAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*testing.T, *findFixture)
	}{
		{"switch page", func(t *testing.T, f *findFixture) {
			other, err := f.manager.NewCompanionPage(t.Context(), f.access, "")
			if err != nil {
				t.Error(err)
				return
			}
			if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(other.ID)); err != nil {
				t.Error(err)
			}
		}},
		{"new document", func(t *testing.T, f *findFixture) { f.fake.commitDocument() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			var once sync.Once
			setFindPoint(f.manager, func(point string) {
				if point == "engine" {
					once.Do(func() { tc.end(t, f) })
				}
			})
			got, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq())
			if err != nil || !got.Active {
				t.Fatalf("start with an end landing during it = %+v, %v", got, err)
			}
			end := f.wantEnded(t, got.Session)
			if end.Session <= got.Session {
				t.Fatalf("the end %d is not newer than the start %d", end.Session, got.Session)
			}
			f.wantCall(t, "start alpha")
			f.wantCall(t, "stop")
			if state := f.find(t, "next", ""); !state.Foreign || state.Active {
				t.Fatalf("step after the end = %+v", state)
			}
		})
	}
}

// A stop ends what is there and asks nothing more: with no search it
// answers so, on a page already closed it answers as for no search, and on
// a suspended page it leaves it suspended rather than restoring it to stop
// a search that already ended.
func TestFindStopNeverRestoresAndIsIdempotent(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	// A stop names a bound even before any start.
	f.seq = 1
	for range 2 {
		if state := f.find(t, "stop", ""); state.Active || !state.Foreign {
			t.Fatalf("stop with no search = %+v", state)
		}
	}
	f.noCall(t)
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, 0)
	if state := f.find(t, "stop", ""); state.Active || !state.Foreign || state.Mount != "" {
		t.Fatalf("stop on a suspended page = %+v", state)
	}
	f.manager.mu.Lock()
	_, suspended := f.manager.suspended[PageID(f.pageID)]
	live, _ := f.manager.livePageLocked(PageID(f.pageID))
	f.manager.mu.Unlock()
	if !suspended || live != nil {
		t.Fatalf("a stop restored the suspended page (suspended %v, live %v)", suspended, live != nil)
	}
	if err := f.manager.ClosePage(t.Context(), f.access, PageID(f.pageID)); err != nil {
		t.Fatal(err)
	}
	if state := f.find(t, "stop", ""); state.Active || state.PageID != f.pageID || !state.Foreign || state.Mount != "" {
		t.Fatalf("stop on a closed page = %+v", state)
	}
}

// A pane mount detached while its start runs, after the start checked the
// mount, ends the search once the start finishes: another mount of the
// thread stays, so the page stays shown, and the search is ended, announced
// and stopped.
func TestFindDetachedDuringAnActionEndsItAfter(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	owner := f.mount
	f.attach(t)
	var once sync.Once
	setFindPoint(f.manager, func(point string) {
		if point == "checked" {
			once.Do(func() { f.manager.DetachPane(owner) })
		}
	})
	got, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", owner, f.nextSeq())
	if err != nil || got.Foreign || MountID(got.Mount) != owner {
		t.Fatalf("start by a mount detached during it = %+v, %v", got, err)
	}
	f.wantEndedFor(t, got.Session, string(owner))
	f.wantCall(t, "start alpha")
	f.wantCall(t, "stop")
	f.noCall(t)
}

// The transport can run a start whose connection already closed (its
// mount's cleanup runs after). The cleanup detaches the mount, which ends
// the search and stops the engine's, and nothing of it is left.
func TestFindStartedByAMountThatThenDetachesLeavesNothing(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	f.manager.DetachPane(f.mount)
	if end := f.wantEnded(t, started.Session); end.Seq != f.seq {
		t.Fatalf("end names sequence %d, want %d", end.Seq, f.seq)
	}
	f.wantCall(t, "stop")
	if session := f.session(t); session.active || session.engine || session.owner != "" || session.seq != 0 {
		t.Fatalf("after the detach the page holds %+v", session)
	}
	f.fake.mu.Lock()
	query := f.fake.findQuery
	f.fake.mu.Unlock()
	if query != "" {
		t.Fatalf("the engine still searches for %q", query)
	}
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq()); !errors.Is(err, errFindNoMount) {
		t.Fatalf("start from the detached mount = %v", err)
	}
	f.noCall(t)
}

// A restored page is a new incarnation under the same id, and its find's
// generations continue past every one the page answered before, so an end
// of the restored page's find is newer than what a bar last showed.
func TestFindGenerationsOutliveARestore(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	for _, query := range []string{"a", "al", "alp", "alpha"} {
		f.find(t, "start", query)
		f.wantCall(t, "start "+query)
	}
	before := f.find(t, "next", "")
	f.wantCall(t, "step")
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	ended := f.wantEnded(t, before.Session)
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(f.pageID)); err != nil {
		t.Fatal(err)
	}
	f.fake = f.page(t, f.pageID)
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if started.Incarnation == before.Incarnation {
		t.Fatalf("restored page answered incarnation %d, the same as before", started.Incarnation)
	}
	if started.Session <= ended.Session {
		t.Fatalf("restored page's start session %d is not newer than the end %d", started.Session, ended.Session)
	}
	f.fake.commitDocument()
	if end := f.wantEnded(t, started.Session); end.Incarnation != started.Incarnation {
		t.Fatalf("end incarnation %d, want %d", end.Incarnation, started.Incarnation)
	}
}

// A start from another mount replaces the search: the old owner is told its
// search ended, the engine's search is replaced rather than stopped, and the
// old owner's later stop or step leaves the new owner's search alone.
func TestFindTakenOverByAnotherMount(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha-beta")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	old, oldSeq := f.mount, f.seq
	f.attach(t)
	taken := f.find(t, "start", "beta")
	if end := f.wantEndedFor(t, started.Session, string(old)); end.Session >= taken.Session || end.Seq != oldSeq {
		t.Fatalf("takeover end %+v against the new start %d", end, taken.Session)
	}
	f.wantCall(t, "start beta")
	if MountID(taken.Mount) != f.mount || taken.Seq != f.seq || !taken.Active {
		t.Fatalf("start answered %+v", taken)
	}
	for _, kind := range []string{"stop", "next"} {
		// Not the old owner's search: the answer is marked so and names the
		// search's real owner.
		state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), kind, "", old, oldSeq+10)
		if err != nil || state.Active || !state.Foreign || MountID(state.Mount) != f.mount || state.Seq != f.seq {
			t.Fatalf("%s by the old owner = %+v, %v", kind, state, err)
		}
	}
	f.noCall(t)
	f.noEvent(t)
	if state := f.find(t, "next", ""); !state.Active || MountID(state.Mount) != f.mount {
		t.Fatalf("new owner's step after the old owner's stop = %+v", state)
	}
	f.wantCall(t, "step")
	f.find(t, "stop", "")
	f.wantCall(t, "stop")
}

// A commit is stamped where the engine reports it; the end it causes can run
// later. A late end leaves a find opened on the new document alone and ends
// one opened before the commit.
func TestFindLateCommitEndsOnlyOlderDocuments(t *testing.T) {
	t.Run("newer find", func(t *testing.T) {
		f := newFindFixture(t, "https://example.test/alpha")
		f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		deliver := f.fake.commitDocumentLate()
		started := f.find(t, "start", "beta")
		f.wantCall(t, "start beta")
		deliver()
		f.noEvent(t)
		f.noCall(t)
		if state := f.find(t, "next", ""); !state.Active || state.Document != started.Document {
			t.Fatalf("step after a late commit = %+v", state)
		}
		f.wantCall(t, "step")
	})
	t.Run("older find", func(t *testing.T) {
		f := newFindFixture(t, "https://example.test/alpha")
		started := f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		deliver := f.fake.commitDocumentLate()
		deliver()
		if end := f.wantEnded(t, started.Session); end.Document != started.Document {
			t.Fatalf("end document %d, want %d", end.Document, started.Document)
		}
		f.wantCall(t, "stop")
	})
}

// A search belongs to its mount and sequence: a later start of the same
// mount replaces it without an end (its provider made that start), and the
// earlier search's late steps and stops, a retried stop among them, change
// nothing of the later one's.
func TestFindEarlierSearchOfTheSameMountTouchesNothingOfALaterOne(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha-beta")
	first := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	earlier := f.seq
	later := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	f.noEvent(t)
	if later.Session <= first.Session || later.Seq != f.seq || later.Foreign {
		t.Fatalf("later start = %+v", later)
	}
	for _, kind := range []string{"next", "previous", "stop", "stop"} {
		state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), kind, "", f.mount, earlier)
		if err != nil || !state.Foreign || state.Seq != f.seq {
			t.Fatalf("%s by the earlier bar = %+v, %v", kind, state, err)
		}
	}
	f.noCall(t)
	f.noEvent(t)
	if state := f.find(t, "next", ""); !state.Active || state.Query != "beta" {
		t.Fatalf("the later bar's search after the earlier bar's actions = %+v", state)
	}
	f.wantCall(t, "step")
}

// A start from a mount that is already gone is refused before it takes
// anything over, and its steps and stops answer foreign: the live owner's
// search stays.
func TestFindFromAGoneMountTakesNothingOver(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	owner := f.mount
	f.attach(t)
	gone := f.mount
	f.mount = owner
	f.manager.DetachPane(gone)
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "beta", gone, 99); !errors.Is(err, errFindNoMount) {
		t.Fatalf("start from a gone mount: %v", err)
	}
	for _, kind := range []string{"next", "stop"} {
		if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), kind, "", gone, 99); err != nil || !state.Foreign || MountID(state.Mount) != owner {
			t.Fatalf("%s from a gone mount = %+v, %v", kind, state, err)
		}
	}
	f.noEvent(t)
	f.noCall(t)
	if state := f.find(t, "next", ""); !state.Active {
		t.Fatalf("owner's search after a gone mount's start = %+v", state)
	}
	f.wantCall(t, "step")
}

// A stop that waited for the page lock while a reload ended its find
// changes nothing: the end is applied before the stop, whose answer is
// marked Foreign and names no owner, so it acknowledges nothing the end did
// not.
func TestFindStopOvertakenByAnEndAnswersForeign(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	p, _, err := f.manager.lookupOwnedPage(f.access, PageID(f.pageID))
	if err != nil {
		t.Fatal(err)
	}
	waiting := findWaiting(f.manager)
	p.mu.Lock()
	type answer struct {
		state FindState
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		state, err := f.manager.Find(context.Background(), f.access, PageID(f.pageID), "stop", "", f.mount, f.seq)
		answered <- answer{state, err}
	}()
	// The stop resolved the page and waits for its lock; the reload's
	// commit is submitted meanwhile.
	<-waiting
	f.fake.commitDocument()
	p.mu.Unlock()
	got := <-answered
	end := f.wantEnded(t, started.Session)
	if got.err != nil || !got.state.Foreign || got.state.Mount != "" || got.state.Active {
		t.Fatalf("stop overtaken by an end = %+v, %v", got.state, got.err)
	}
	if got.state.Session != end.Session {
		t.Fatalf("overtaken stop answered generation %d, the end was %d", got.state.Session, end.Session)
	}
	f.wantCall(t, "stop")
	f.noCall(t)
}

// A start's sequence must be above every start its mount made before: an
// older or repeated one is refused and changes nothing. Another mount
// numbers its own starts.
func TestFindRefusesAStartNotAboveTheMountsLast(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.seq = 4
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	for _, seq := range []uint64{5, 4, 1} {
		if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "beta", f.mount, seq); !errors.Is(err, errFindStale) {
			t.Fatalf("start %d after %d = %v, want stale", seq, started.Seq, err)
		}
	}
	f.noCall(t)
	if state := f.find(t, "next", ""); !state.Active || state.Seq != 5 || state.Query != "alpha" {
		t.Fatalf("the search after the refused starts = %+v", state)
	}
	f.wantCall(t, "step")
	first := f.mount
	f.attach(t)
	if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", f.mount, 1); err != nil || !state.Active {
		t.Fatalf("another mount's first start = %+v, %v", state, err)
	}
	f.wantEndedFor(t, started.Session, string(first))
	f.wantCall(t, "start alpha")
}

// A stop ends the mount's search exactly when its sequence is at or below
// the stop's bound; a step names its search's exact sequence.
func TestFindStopEndsTheMountsSearchAtOrBelowItsBound(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.seq = 2
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	for _, kind := range []string{"next", "previous"} {
		for _, seq := range []uint64{2, 4} {
			if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), kind, "", f.mount, seq); err != nil || !state.Foreign || state.Seq != 3 {
				t.Fatalf("%s naming %d = %+v, %v", kind, seq, state, err)
			}
		}
	}
	if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "stop", "", f.mount, 2); err != nil || !state.Foreign || state.Seq != 3 {
		t.Fatalf("stop through 2 of search 3 = %+v, %v", state, err)
	}
	f.noCall(t)
	if state := f.session(t); !state.active || state.seq != started.Seq {
		t.Fatalf("after the earlier stop the page holds %+v", state)
	}
	if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "stop", "", f.mount, 9); err != nil || state.Foreign || state.Seq != 3 {
		t.Fatalf("stop through 9 of search 3 = %+v, %v", state, err)
	}
	f.wantCall(t, "stop")
	if state := f.session(t); state.active {
		t.Fatalf("after the stop the page holds %+v", state)
	}
}

// A stop ends the mount's search on whichever page it runs: one naming a
// page already gone still ends the search on the page the mount searched.
func TestFindStopEndsTheMountsSearchOnAnotherPage(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	state, err := f.manager.Find(t.Context(), f.access, "page-already-closed", "stop", "", f.mount, f.seq+1)
	if err != nil || !state.Foreign || state.PageID != "page-already-closed" {
		t.Fatalf("stop naming a gone page = %+v, %v", state, err)
	}
	f.wantCall(t, "stop")
	if session := f.session(t); session.active || session.engine {
		t.Fatalf("after the stop the searched page holds %+v", session)
	}
	// Another mount's search is not the stop's.
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if _, err := f.manager.Find(t.Context(), f.access, "page-already-closed", "stop", "", "another-mount", 99); err != nil {
		t.Fatal(err)
	}
	f.noCall(t)
	if session := f.session(t); !session.active {
		t.Fatalf("another mount's stop ended the search: %+v", session)
	}
}
