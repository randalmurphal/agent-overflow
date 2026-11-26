package app

import (
	"encoding/json"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
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

// twoEstimateBoards is a Jira costFixture of n issues whose WEB-1 is also
// on a second board, WEB team, which estimates by Team Points
// (customfield_10026), with both boards read: WEB-1 holds 3 Story Points,
// which board 1 estimates by, and 5 Team Points.
func twoEstimateBoards(t *testing.T, n int) (*trackerBoardFixture, string, string) {
	t.Helper()
	f, thread, endpoint := costFixtureWith(t, false, n, func(site *trackerfake.JiraSite, ws *trackerfake.LinearWorkspace) {
		site.Fields = append(site.Fields, trackerfake.JiraField{ID: "customfield_10026", Name: "Team Points",
			Schema: trackerfake.JiraSchema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10026}})
		site.Boards = append(site.Boards, trackerfake.JiraBoard{ID: 2, Name: "WEB team", Type: "scrum", Project: "WEB", Columns: site.Boards[0].Columns, Estimation: "customfield_10026"})
		site.Sprints = append(site.Sprints, trackerfake.JiraSprint{ID: 3, Board: 2, Name: "Team 1", State: "active", Start: site.Sprints[0].Start, End: site.Sprints[0].End})
		is := &site.Issues[0]
		is.Sprints = []int64{1, 3}
		is.Values = map[string]json.RawMessage{"customfield_10016": json.RawMessage(`3`), "customfield_10026": json.RawMessage(`5`)}
	})
	readBoard(t, f, f.conn.ID, "2", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "3"}, {Kind: trackerapp.SectionBacklog}})
	return f, thread, endpoint
}

// fakeValues are the values the fake holds of issue's fields, as JSON.
func fakeValues(f *trackerBoardFixture, issue string, fields ...string) []string {
	world := f.e.Snapshot()
	i := slices.IndexFunc(world.Jira[0].Issues, func(is trackerfake.JiraIssue) bool { return is.ID == issue })
	out := make([]string, len(fields))
	for k, id := range fields {
		out[k] = string(world.Jira[0].Issues[i].Values[id])
	}
	return out
}

// An estimate named by a board's own field is written on that board: WEB-1
// is on two boards, the first estimating by Story Points (3) and the
// second by Team Points (5). Team Points set to 8, by name or by ID, writes
// Team Points and leaves Story Points; the generic estimate set to 4 writes
// the first board's.
func TestTrackerAgentEstimateByAFieldIsWrittenOnItsBoard(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, set string
		v         float64
		want      []string
	}{
		{"by name", "Team Points", 8, []string{"3", "8"}},
		{"by id", "customfield_10026", 8, []string{"3", "8"}},
		{"the generic name", "estimate", 4, []string{"4", "5"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f, thread, endpoint := twoEstimateBoards(t, 3)
			text, _ := toolRoutes(t, f, thread, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "set": map[string]any{c.set: c.v}})
			if !strings.Contains(text, `"applied":[`+strconv.Quote(c.set)+`]`) {
				t.Errorf("ticket_update = %s; want %s applied", text, c.set)
			}
			if got := fakeValues(f, "10101", "customfield_10016", "customfield_10026"); !slices.Equal(got, c.want) {
				t.Errorf("Story Points and Team Points = %v; want %v", got, c.want)
			}
		})
	}
}

// An estimate of a ticket on two boards reads only the board it is set
// on before its send: WEB-1's estimate set to 4 is one rows read, the
// estimate, and the refresh of both boards; set to the 3 it holds, one
// rows read and nothing sent.
func TestTrackerAgentEstimateOnTwoBoardsRequests(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		v    float64
		want string
	}{
		{"set", 4, "rows estimate rows×2"},
		{"already", 3, "rows"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f, thread, endpoint := twoEstimateBoards(t, 3)
			_, routes := toolRoutes(t, f, thread, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "set": map[string]any{"estimate": c.v}})
			if got := costNames(routes); got != c.want {
				t.Errorf("%d requests: %s\nwant %s", len(routes), got, c.want)
			}
		})
	}
}

// An estimate on a board that no longer lists the ticket is refused, and
// nothing is sent: WEB-1 moves to another project on the tracker, as
// OPS-1, which takes it off board 1, and no board is read again before
// the call, which names it by the key the mirror holds.
func TestTrackerAgentEstimateOffItsBoardIsRefused(t *testing.T) {
	t.Parallel()
	f, thread, endpoint := costFixture(t, false, 3)
	world := f.e.Snapshot()
	world.Jira[0].Token, world.Linear[0].APIKey = trackerTestToken, costLinear(3).APIKey
	world.Jira[0].Projects = append(world.Jira[0].Projects, trackerfake.JiraProject{ID: "10001", Key: "OPS", Name: "Ops"})
	world.Jira[0].Issues[0].Project, world.Jira[0].Issues[0].Key = "OPS", "OPS-1"
	if err := f.e.Seed(world); err != nil {
		t.Fatal(err)
	}
	text, routes := toolRoutes(t, f, thread, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "set": map[string]any{"estimate": 4}})
	if !strings.Contains(text, `is not on board \"WEB board\" any more`) {
		t.Errorf("ticket_update = %s; want the estimate refused, the ticket not on its board", text)
	}
	// The board's rows read confirms an issue its list left out with one
	// membership match (jiraprovider BoardRows).
	if got := costNames(routes); got != "match rows" {
		t.Errorf("%d requests: %s; want the board's rows read alone", len(routes), got)
	}
	if got := fakeValues(f, "10101", "customfield_10016"); got[0] != "" {
		t.Errorf("Story Points = %s; want nothing written", got[0])
	}
}

// describeEstimate is a Linear estimate as fakeValues writes a Jira one:
// its JSON, empty for none.
func describeEstimate(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
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
// the approval finds the estimate moved; the other two are sent theirs,
// which the tracker's own record of the writes and its estimates show.
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
			log := f.e.Requests(0).Requests
			before := log[len(log)-1].Seq
			if err := f.a.RespondToApproval(t.Context(), thread, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
			result := <-done
			text, _ := result[0].(string)
			if !strings.Contains(text, `"key":"`+keys[0]+`","refused":"`+keys[0]+` changed on`) || strings.Count(text, `"applied":["estimate"]`) != 2 {
				t.Errorf("ticket_update = %s; want %s sent nothing for its estimate changed during the wait, the others sent theirs", text, keys[0])
			}
			// What the tracker received and holds: the two others' estimates
			// alone were written, and the first keeps the 5 set during the
			// wait.
			after := f.e.Snapshot()
			wantSent := []string{"10102", "10103"}
			var sent, held []string
			if linear {
				ws := after.Linear[0]
				wantSent = []string{ws.Issues[1].ID, ws.Issues[2].ID}
				for _, is := range ws.Issues {
					held = append(held, describeEstimate(is.Estimate))
				}
			} else {
				held = fakeValues(f, "10101", "customfield_10016")
				held = append(held, fakeValues(f, "10102", "customfield_10016")...)
				held = append(held, fakeValues(f, "10103", "customfield_10016")...)
			}
			for _, r := range f.e.Requests(before).Requests {
				switch {
				case r.Route == "PUT rest/agile/1.0/issue/{issue}/estimation":
					sent = append(sent, path.Base(path.Dir(r.Path)))
				case r.Operation == "UpdateIssue":
					var v struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(r.Variables, &v); err != nil {
						t.Fatal(err)
					}
					sent = append(sent, v.ID)
				}
			}
			if !slices.Equal(sent, wantSent) {
				t.Errorf("estimates sent to %q; want %q", sent, wantSent)
			}
			if want := []string{"5", "3", "3"}; !slices.Equal(held, want) {
				t.Errorf("the tracker holds the estimates %q; want %q", held, want)
			}
		})
	}
}
