package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
)

// A ticket's estimate is the ticket's own read, not the board's last
// poll: the first ticket's estimate goes from 3 to 5 on the tracker and
// no board is read again. ticket_read says 5 points, and an update that
// sets 3 is sent, not answered as already set; read first or not.
func TestTrackerAgentEstimateIsTheTicketsOwnRead(t *testing.T) {
	t.Parallel()
	for _, linear := range []bool{false, true} {
		tracker, key := "jira", "WEB-1"
		if linear {
			tracker, key = "linear", "example/ENG-1"
		}
		for _, c := range []struct {
			name string
			read bool
		}{{"read first", true}, {"not read first", false}} {
			t.Run(tracker+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				f, thread, endpoint := costFixtureWith(t, linear, 3, func(site *trackerfake.JiraSite, ws *trackerfake.LinearWorkspace) {
					setCostEstimate(site, ws, 3)
				})
				world := f.e.Snapshot()
				world.Jira[0].Token, world.Linear[0].APIKey = trackerTestToken, costLinear(3).APIKey
				setCostEstimate(&world.Jira[0], &world.Linear[0], 5)
				if err := f.e.Seed(world); err != nil {
					t.Fatal(err)
				}
				if c.read {
					text, _ := toolRoutes(t, f, thread, endpoint, "ticket_read", map[string]any{"key": key})
					if !strings.Contains(text, `"estimate":"5 points"`) {
						t.Errorf("ticket_read = %s; want the estimate the tracker holds, 5 points", text)
					}
				}
				text, _ := toolRoutes(t, f, thread, endpoint, "ticket_update", map[string]any{"keys": []string{key}, "set": map[string]any{"estimate": 3}})
				if !strings.Contains(text, `"applied":["estimate"]`) {
					t.Errorf("ticket_update = %s; want the estimate of 3 sent", text)
				}
			})
		}
	}
}

// setCostEstimate sets the first issue's estimate on both trackers of a
// costFixture to v, updated now.
func setCostEstimate(site *trackerfake.JiraSite, ws *trackerfake.LinearWorkspace, v float64) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	setCostEstimateOnly(site, ws, v)
	site.Issues[0].Updated, ws.Issues[0].UpdatedAt = now, now
}

// setCostEstimateOnly is setCostEstimate leaving the update times as they
// are.
func setCostEstimateOnly(site *trackerfake.JiraSite, ws *trackerfake.LinearWorkspace, v float64) {
	raw, _ := json.Marshal(v)
	is := &site.Issues[0]
	if is.Values == nil {
		is.Values = map[string]json.RawMessage{}
	}
	is.Values["customfield_10016"] = raw
	ws.Issues[0].Estimate = &v
}

// An estimate the tracker changes while the person decides is sent
// nothing: three tickets get an estimate of 3, and WEB-1's goes from 2 to
// 5 on the tracker during the wait, with no board read again and its
// update time as it was, so its card did not move. The rows read after
// the approval finds the estimate moved; the other two are sent theirs.
func TestTrackerAgentEstimateChangedDuringTheWaitIsSentNothing(t *testing.T) {
	t.Parallel()
	for _, linear := range []bool{false, true} {
		tracker, keys := "jira", []string{"WEB-1", "WEB-2", "WEB-3"}
		if linear {
			tracker, keys = "linear", []string{"example/ENG-1", "example/ENG-2", "example/ENG-3"}
		}
		t.Run(tracker, func(t *testing.T) {
			t.Parallel()
			f, thread, endpoint := costFixtureWith(t, linear, 3, func(site *trackerfake.JiraSite, ws *trackerfake.LinearWorkspace) {
				setCostEstimate(site, ws, 2)
			})
			done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": keys, "set": map[string]any{"estimate": 3}})
			open, _ := trackerWritePrompt(t, f.a, thread)
			world := f.e.Snapshot()
			world.Jira[0].Token, world.Linear[0].APIKey = trackerTestToken, costLinear(3).APIKey
			setCostEstimateOnly(&world.Jira[0], &world.Linear[0], 5)
			if err := f.e.Seed(world); err != nil {
				t.Fatal(err)
			}
			if err := f.a.RespondToApproval(t.Context(), thread, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
			result := <-done
			text, _ := result[0].(string)
			if !strings.Contains(text, `"key":"`+keys[0]+`","refused":"`+keys[0]+` changed on`) || strings.Count(text, `"applied":["estimate"]`) != 2 {
				t.Errorf("ticket_update = %s; want %s sent nothing for its estimate changed during the wait, the others sent theirs", text, keys[0])
			}
		})
	}
}
