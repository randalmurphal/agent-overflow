package app

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A nudge's run is recorded once its message is accepted as new work, by
// the worker's placement then: a send that fails records none, and a frame
// repeating one already accepted starts nothing.

// nudgeDoor sends worker a message through one door that starts work.
type nudgeDoor struct {
	name string
	send func(t *testing.T, f *requestFixture, worker store.Thread, sendID string) error
}

var nudgeDoors = []nudgeDoor{
	{"the composer", func(t *testing.T, f *requestFixture, worker store.Thread, sendID string) error {
		_, err := f.app.SendMessageWithOptions(t.Context(), worker.ID, "also check the docs", SendMessageOptions{SendID: sendID})
		return err
	}},
	{"revert and resend", func(t *testing.T, f *requestFixture, worker store.Thread, sendID string) error {
		return revertAndResendForTest(f.app, t.Context(), worker.ID, "user:1", RevertAndResendOptions{Content: "also check the docs", SendID: sendID})
	}},
	{"another thread's send", func(t *testing.T, f *requestFixture, worker store.Thread, _ string) error {
		other := f.plainThread(t)
		_, err := f.adapter().Send(t.Context(), threadtools.Caller{ThreadID: other.ID, Title: other.Title}, threadtools.SendCall{
			ThreadID: worker.ID, Message: "also check the docs",
		})
		return err
	}},
}

// nudgeRuns is the nudge rows of the fixture's caller, open or in Past.
func (f *requestFixture) nudgeRuns(t *testing.T) []ThreadAgentRow {
	t.Helper()
	f.app.threadRequestsWG.Wait()
	var out []ThreadAgentRow
	for _, row := range f.openRows(t, f.caller.ID) {
		if row.Kind == threadAgentKindNudge {
			out = append(out, row)
		}
	}
	page, err := f.app.ListThreadPastAgentRows(t.Context(), f.caller.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Rows {
		if row.Kind == threadAgentKindNudge {
			out = append(out, row)
		}
	}
	return out
}

// A placement change between a message's admission and its acceptance
// decides by the placement at acceptance: the worker taken out of the
// sidebar has its spawner's nudge run, the worker shown in it has none,
// and a message that fails has none either way. The change lands inside
// the send, through the send seam or once the message is placed with a
// provider that starts or fails to.
func TestAPlacementChangeDuringASendDecidesItsNudgeRun(t *testing.T) {
	t.Parallel()
	for _, door := range nudgeDoors {
		for _, tc := range []struct {
			name     string
			from, to string
			fails    bool
			nudge    bool
		}{
			{name: "shown in the sidebar, then failed", from: store.PlacementOwned, to: store.PlacementStandalone, fails: true},
			{name: "shown in the sidebar, then accepted", from: store.PlacementOwned, to: store.PlacementStandalone},
			{name: "taken out of the sidebar, then failed", from: store.PlacementStandalone, to: store.PlacementOwned, fails: true},
			{name: "taken out of the sidebar, then accepted", from: store.PlacementStandalone, to: store.PlacementOwned, nudge: true},
		} {
			for _, provider := range []bool{false, true} {
				name := door.name + "/" + tc.name + "/send seam"
				if provider {
					name = door.name + "/" + tc.name + "/provider"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					f := newRequestFixture(t)
					f.watchAgentRows()
					f.holdCallerStart()
					worker, _ := seedResendThread(t, f.app, "race-worker")
					f.restAsWorker(t, "race-spawn", worker, tc.from)
					place := func() {
						if _, err := f.app.SetThreadPlacement([]string{worker.ID}, tc.to); err != nil {
							t.Error(err)
						}
					}
					switch {
					case !provider:
						f.app.sendMessageFn = func(threadID, _ string, _ []string) error {
							if threadID != worker.ID {
								return nil
							}
							place()
							if tc.fails {
								return errors.New("the provider refused the message")
							}
							return nil
						}
					case tc.fails:
						if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": filepath.Join(t.TempDir(), "missing-claude")}); err != nil {
							t.Fatal(err)
						}
					default:
						installMockClaudeTurns(t, f.app, [][]string{heldTurnLines("raced", "On it.")})
					}
					if provider {
						f.app.sendStageFn = func(threadID, stage string) {
							if threadID == worker.ID && stage == "placed" {
								place()
							}
						}
					}
					err := door.send(t, f, worker, "race-send")
					if tc.fails != (err != nil) {
						t.Fatalf("the send: %v; want it to fail %v", err, tc.fails)
					}
					runs := f.nudgeRuns(t)
					if tc.nudge && (len(runs) != 1 || runs[0].ThreadID != worker.ID) {
						t.Fatalf("nudge rows %+v; want the accepted message's one run", runs)
					}
					if !tc.nudge && len(runs) != 0 {
						t.Fatalf("nudge rows %+v; want none", runs)
					}
				})
			}
		}
	}
}

// A frame repeating a message already accepted, at any door with a send
// id, after the worker came to rest again, records no second nudge run.
func TestARepeatedAcceptedSendRecordsNoNudgeRun(t *testing.T) {
	t.Parallel()
	for _, door := range nudgeDoors[:2] {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			f.holdCallerStart()
			gate := filepath.Join(t.TempDir(), "open")
			release(t, gate)
			installMockClaudeTurns(t, f.app, [][]string{runTurnLines("accepted", "Checked the docs.", gate)})
			worker := f.plainThread(t)
			f.restAsWorker(t, "repeat-spawn", worker, store.PlacementOwned)
			if _, err := f.app.SendMessageWithOptions(t.Context(), worker.ID, "also check the docs", SendMessageOptions{SendID: "repeat-send"}); err != nil {
				t.Fatal(err)
			}
			var first ThreadAgentRow
			waitUntil(t, 10*time.Second, func() bool {
				runs := f.nudgeRuns(t)
				if len(runs) == 1 {
					first = runs[0]
				}
				return len(runs) == 1 && !runs[0].open()
			})
			if err := door.send(t, f, worker, "repeat-send"); err != nil {
				t.Fatalf("the repeated frame: %v", err)
			}
			if runs := f.nudgeRuns(t); len(runs) != 1 || runs[0].Token != first.Token {
				t.Fatalf("after the repeated frame the nudge rows = %+v; want the first message's alone", runs)
			}
		})
	}
}
