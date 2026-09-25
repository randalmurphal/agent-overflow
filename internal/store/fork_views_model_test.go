package store

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// TestForkWritesKeepEveryOtherThreadsView drives random writes over a
// family of pointer forks: reverts from a turn or an item, late writes to
// rows and turn rows, deletes of rows, new turns, new forks of any thread
// and deletes of any thread. A write changes what its own thread reads and
// nothing any other thread reads, however the holders it makes or reuses
// stack up.
func TestForkWritesKeepEveryOtherThreadsView(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 4} {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			s := newTestStore(t)
			seedTurnedSource(t, s, "S", 12)
			live := []string{"S"}
			rng := rand.New(rand.NewPCG(seed, 99))
			views := readerViews(t, s, live...)
			forks, next := 0, 0
			var history []string
			var thread, op string
			defer func() {
				if !t.Failed() {
					return
				}
				for _, entry := range history {
					t.Log(entry)
				}
				t.Logf("failed in %s %s", thread, op)
				for _, id := range live {
					t.Logf("%s lineage %v", id, forkLineage(t, s, id))
				}
			}()
			lastTurn := func(thread string) int {
				t.Helper()
				last := -1
				for _, it := range forkRows(t, s, thread) {
					last = max(last, it.TurnIndex)
				}
				turns, err := s.ListRecentTurns(thread, 1)
				if err != nil {
					t.Fatal(err)
				}
				if len(turns) > 0 {
					last = max(last, turns[0].TurnIndex)
				}
				return last
			}
			pickRow := func(thread string, assistant bool) string {
				t.Helper()
				var ids []string
				for _, it := range forkRows(t, s, thread) {
					if !assistant || (it.Role == "assistant" && it.TurnIndex > 0) {
						ids = append(ids, it.ID)
					}
				}
				if len(ids) == 0 {
					return ""
				}
				return ids[rng.IntN(len(ids))]
			}
			newTurn := func(thread string, turn int) {
				t.Helper()
				next++
				appendSourceTurn(t, s, thread, turn, fmt.Sprintf("%s-n%d", thread, next))
			}
			for step := range 250 {
				thread, op = live[rng.IntN(len(live))], ""
				var err error
				switch rng.IntN(9) {
				case 0:
					last := lastTurn(thread)
					if last < 1 {
						continue
					}
					turn := 1 + rng.IntN(last)
					op = fmt.Sprintf("revert from turn %d", turn)
					if _, _, err = s.DeleteConversationFromTurn(thread, turn); err == nil {
						newTurn(thread, turn)
					}
				case 1:
					id := pickRow(thread, true)
					if id == "" {
						continue
					}
					op = "revert from item " + id
					if _, _, err = s.DeleteConversationFromItem(thread, id); err == nil {
						newTurn(thread, lastTurn(thread)+1)
					}
				case 2:
					id := pickRow(thread, false)
					if id == "" {
						continue
					}
					op = "late write to " + id
					err = s.UpdateItemMeta(thread, id, fmt.Sprintf(`{"late":%d}`, step))
				case 3:
					ids, qerr := queryIDs(s.db, `SELECT turn_id FROM turns WHERE thread_id = ? ORDER BY turn_index`, thread)
					if qerr != nil {
						t.Fatal(qerr)
					}
					if len(ids) == 0 {
						continue
					}
					id := ids[rng.IntN(len(ids))]
					op = "late settle of " + id
					err = s.UpdateTurnCompleted(id, int64(1000+step), "late", "", "", "")
				case 4:
					id := pickRow(thread, false)
					if id == "" {
						continue
					}
					op = "delete " + id
					err = s.DeleteThreadItem(thread, id)
				case 5:
					op = "new turn"
					newTurn(thread, lastTurn(thread)+1)
				case 6, 7:
					if len(live) >= 8 {
						continue
					}
					forks++
					fork := fmt.Sprintf("F%d", forks)
					cut := ForkCut{}
					if last := lastTurn(thread); last > 0 && rng.IntN(2) == 0 {
						cut = throughTurn(rng.IntN(last))
					}
					op = "fork " + fork
					mustPointerFork(t, s, thread, fork, cut)
					live = append(live, fork)
					views[fork] = readerViews(t, s, fork)[fork]
				default:
					if len(live) < 3 {
						continue
					}
					op = "delete thread"
					if err = s.DeleteThread(thread); err == nil {
						live = slices.DeleteFunc(live, func(id string) bool { return id == thread })
						delete(views, thread)
					}
				}
				history = append(history, fmt.Sprintf("step %d, %s %s", step, thread, op))
				if err != nil {
					t.Fatalf("step %d, %s %s: %v", step, thread, op, err)
				}
				mine := views[thread]
				delete(views, thread)
				requireViews(t, s, views, fmt.Sprintf("step %d, %s %s", step, thread, op))
				if mine != nil {
					views[thread] = readerViews(t, s, thread)[thread]
				}
			}
			t.Logf("%d threads live, %d holders, deepest lineage %d", len(live), len(holderIDs(t, s)), maxLineageDepth(t, s))
		})
	}
}
