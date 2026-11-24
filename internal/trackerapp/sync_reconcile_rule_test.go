package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// A reconcile pass whose rows read the tracker refuses for the listing's
// rule (tracker.ErrRuleRefused: the tracker refused the query that carried
// it) ends, and the board lists again at once, at most once a reconcile
// interval; every other failure keeps the exact pass. These run the
// reconcile against a scripted provider: the handling is the same
// whichever provider refuses.

// failAt is a passBoard.fail that fails the rows reads numbered reads
// with err.
func failAt(read int, err error, more ...int) func(int) error {
	return func(n int) error {
		if n == read || slices.Contains(more, n) {
			return err
		}
		return nil
	}
}

// ruleRefused is a provider's typed refusal of a rule, wrapping its 400.
func ruleRefused() error {
	return fmt.Errorf("%w: %w", tracker.ErrRuleRefused, &trackerapi.StatusError{Status: http.StatusBadRequest})
}

// rowsHeld reports whether m's mirror holds a row of each of issues.
func rowsHeld(t *testing.T, m *mirrorFixture, issues []tracker.IssueID) bool {
	t.Helper()
	for _, id := range issues {
		rows, _, err := m.st.ListMirrorIssueRows(m.root(), string(id), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			return false
		}
	}
	return true
}

// dueTick runs a tick of m's board as its pump runs it, the units the
// pump's own due rule (boardDue) picks.
func dueTick(t *testing.T, m *mirrorFixture, holder *boardPump) Outcome {
	t.Helper()
	out, _, err := m.svc.syncBoard(t.Context(), holder, Background, m.svc.boardDue)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The refusal in the first batch stores nothing and in the third batch,
// after two committed, keeps those rows; either way the pass ends and the
// tick lists the board again at once and reads the rest, the committed
// rows asked once.
func TestARefusedRuleListsTheBoardAgainAtOnce(t *testing.T) {
	t.Parallel()
	for _, refused := range []int{1, 3} {
		t.Run(fmt.Sprint("batch ", refused), func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			n := 4 * readBatch
			b := newPassBoard(m, n, func(int) bool { return true })
			b.fail = failAt(refused, ruleRefused())
			holder := &boardPump{scope: m.scope()}
			out := passTick(t, m, holder, BoardDue{Structure: true, Reconcile: true})
			if out.Failure != nil || out.More() || m.p.count("BoardIssues") != 2 {
				t.Fatalf("the refused tick %+v after %d listings; want the board listed again and the pass done", out, m.p.count("BoardIssues"))
			}
			if !rowsHeld(t, m, b.listed) || holder.pass.openIn(m.sync(testBoard).Lifetime) {
				t.Fatalf("after the second listing every row held %v, the pass %+v", rowsHeld(t, m, b.listed), holder.pass)
			}
			// The committed rows were asked once: the second listing found them.
			b.mu.Lock()
			defer b.mu.Unlock()
			for _, id := range b.listed[:(refused-1)*readBatch] {
				if b.asked[id] != 1 {
					t.Fatalf("committed issue %s asked %d times", id, b.asked[id])
				}
			}
		})
	}
}

// A refusal that persists lists the board again once, then fails the
// tick with the pass ended; the pump's next ticks, and a refresh's
// reconcile, fail with that refusal and list nothing until a reconcile
// interval passed since that listing, and then list once and once again.
func TestAPersistentRefusalListsAgainOnceAnInterval(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	b := newPassBoard(m, 2*readBatch, func(int) bool { return true })
	b.fail = func(int) error { return ruleRefused() }
	holder := &boardPump{scope: m.scope()}
	if out := dueTick(t, m, holder); out.Failure == nil || m.p.count("BoardIssues") != 2 {
		t.Fatalf("the refused tick %+v after %d listings; want two", out, m.p.count("BoardIssues"))
	}
	if holder.pass.openIn(m.sync(testBoard).Lifetime) {
		t.Fatalf("the pass %+v is open after the refusal listed again", holder.pass)
	}
	reads := b.read
	if out := dueTick(t, m, holder); out.Failure == nil || m.p.count("BoardIssues") != 2 || b.read != reads {
		t.Fatalf("the tick after %+v after %d listings and %d reads; want the refusal, 2 and %d", out, m.p.count("BoardIssues"), b.read, reads)
	}
	if out := passTick(t, m, holder, BoardDue{Reconcile: true}); out.Failure == nil || m.p.count("BoardIssues") != 2 {
		t.Fatalf("a refresh's reconcile %+v after %d listings; want the refusal and none more", out, m.p.count("BoardIssues"))
	}
	m.pass(m.svc.cadence.Reconcile - time.Second)
	if out := dueTick(t, m, holder); out.Failure == nil || m.p.count("BoardIssues") != 2 {
		t.Fatalf("the tick before the interval passed %+v after %d listings; want the refusal and 2", out, m.p.count("BoardIssues"))
	}
	m.pass(time.Second)
	if out := dueTick(t, m, holder); out.Failure == nil || m.p.count("BoardIssues") != 4 {
		t.Fatalf("the tick once the interval passed %+v after %d listings; want two more", out, m.p.count("BoardIssues"))
	}
}

// A refresh shortly after a pass finished, its interval far from passed,
// begins a pass whose rows read refuses the rule: the tick lists the
// board again at once and reads the new issue.
func TestARefusedRuleShortlyAfterAReconcileListsAgainAtOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureOn(t, newFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Reconcile: time.Hour}))
	b := newPassBoard(m, 2*readBatch, func(int) bool { return true })
	holder := &boardPump{scope: m.scope()}
	if out := dueTick(t, m, holder); out.Failure != nil || out.More() || m.sync(testBoard).ReconciledAt == 0 {
		t.Fatalf("the first pass %+v", out)
	}
	if out := dueTick(t, m, holder); out.Failure != nil || m.p.count("BoardIssues") != 1 {
		t.Fatalf("a tick after the finished pass %+v after %d listings; want no reconcile due", out, m.p.count("BoardIssues"))
	}
	b.listed = append(b.listed, "30000")
	b.fail = failAt(b.read+1, ruleRefused())
	if out := passTick(t, m, holder, BoardDue{Reconcile: true}); out.Failure != nil || m.p.count("BoardIssues") != 3 || !rowsHeld(t, m, []tracker.IssueID{"30000"}) {
		t.Fatalf("the refreshed reconcile %+v after %d listings; want the board listed again and the new issue held", out, m.p.count("BoardIssues"))
	}
}

// A refresh shortly after a pass finished, its interval far from passed,
// begins a pass whose rows read keeps refusing the rule: the refusal the
// pump holds keeps the reconcile due, so the pump's next tick fails with
// it, listing nothing, though the last pass is recent.
func TestAHeldRefusalStaysDueAfterARecentPass(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureOn(t, newFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Reconcile: time.Hour}))
	b := newPassBoard(m, 2*readBatch, func(int) bool { return true })
	holder := &boardPump{scope: m.scope()}
	if out := dueTick(t, m, holder); out.Failure != nil || out.More() || m.sync(testBoard).ReconciledAt == 0 {
		t.Fatalf("the first pass %+v", out)
	}
	b.listed = append(b.listed, "30000")
	b.fail = func(int) error { return ruleRefused() }
	if out := passTick(t, m, holder, BoardDue{Reconcile: true}); out.Failure == nil || m.p.count("BoardIssues") != 3 {
		t.Fatalf("the refreshed reconcile %+v after %d listings; want the refusal after 3", out, m.p.count("BoardIssues"))
	}
	out := dueTick(t, m, holder)
	if out.Failure == nil || m.p.count("BoardIssues") != 3 || !slices.ContainsFunc(out.Ran, func(u UnitResult) bool { return u.Unit == UnitReconcile }) {
		t.Fatalf("the next tick %+v after %d listings; want its reconcile failing with the refusal and 3", out, m.p.count("BoardIssues"))
	}
}

// Every failure of a rows read that is not a refusal of the rule keeps the
// exact pass: the failed batch and the rest, and the tick after goes on
// with them without listing the board again.
func TestAFailedRowsReadKeepsTheExactPass(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"canceled":     context.Canceled,
		"network":      &trackerapi.TransientError{Err: errors.New("reset")},
		"rate limited": &trackerapi.RateLimitedError{Until: time.Now().Add(time.Minute)},
		"forbidden":    &trackerapi.StatusError{Status: http.StatusForbidden},
		"server":       &trackerapi.StatusError{Status: http.StatusBadGateway},
		"bad request":  &trackerapi.StatusError{Status: http.StatusBadRequest},
		"oversized":    &trackerapi.BodyTooLargeError{Limit: 1},
		"malformed":    errors.New("decode the rows: unexpected end of JSON input"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			b := newPassBoard(m, 3*readBatch, func(int) bool { return true })
			b.fail = failAt(2, err)
			holder := &boardPump{scope: m.scope()}
			if out := passTick(t, m, holder, BoardDue{Structure: true, Reconcile: true}); out.Failure == nil {
				t.Fatalf("the failing tick %+v", out)
			}
			if want := b.listed[readBatch:]; !slices.Equal(holder.pass.left, want) || !reflect.DeepEqual(holder.pass.rule, passRule) {
				t.Fatalf("the pass after the failure holds %d candidates and rule %+v; want the failed batch and the rest, %d, and the listing's",
					len(holder.pass.left), holder.pass.rule, len(want))
			}
			if out := dueTick(t, m, holder); out.Failure != nil || m.p.count("BoardIssues") != 1 || !rowsHeld(t, m, b.listed) {
				t.Fatalf("the retry %+v after %d listings", out, m.p.count("BoardIssues"))
			}
		})
	}
}

// A pass whose board lifetime is replaced while the pass is open is not
// gone on with, and a listing again for a refusal that waits for the
// interval does not wait in the new lifetime: after a refusal that
// persisted, or another failure that kept candidates, the next tick lists
// the new lifetime, and a refusal there lists again at once.
func TestAPassOfAReplacedBoardLifetimeListsTheNewOne(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]func(int) error{
		"refused": failAt(2, ruleRefused(), 3, 4),
		"network": failAt(2, &trackerapi.TransientError{Err: errors.New("reset")}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			b := newPassBoard(m, 3*readBatch, func(int) bool { return true })
			b.fail = fail
			holder := &boardPump{scope: m.scope()}
			if out := passTick(t, m, holder, BoardDue{Structure: true, Reconcile: true}); out.Failure == nil {
				t.Fatalf("the failing tick %+v", out)
			}
			old := m.sync(testBoard).Lifetime
			m.listBoards(m.conn.ID, testBoards[1:]...)
			if err := m.svc.FinishMirrorCleanup(); err != nil {
				t.Fatal(err)
			}
			m.listBoards(m.conn.ID, testBoards...)
			listed := m.p.count("BoardIssues")
			if out := passTick(t, m, holder, BoardDue{Structure: true, Reconcile: true}); out.Failure != nil || m.p.count("BoardIssues") != listed+map[string]int{"refused": 2, "network": 1}[name] {
				t.Fatalf("the tick of the new lifetime %+v after %d listings (%d before)", out, m.p.count("BoardIssues"), listed)
			}
			if now := m.sync(testBoard).Lifetime; now == old || !rowsHeld(t, m, b.listed) {
				t.Fatalf("the board's lifetime %d (was %d); every row held %v", now, old, rowsHeld(t, m, b.listed))
			}
		})
	}
}
