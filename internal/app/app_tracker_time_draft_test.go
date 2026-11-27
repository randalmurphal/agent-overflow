package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackertools"
	"agent-overflow/internal/transport"
)

// timeEntriesEvent waits for the next tracker:time_entries_updated event,
// skipping every other.
func (f *trackerBoardFixture) timeEntriesEvent(t *testing.T, what string) trackerapp.TimeEntriesEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-f.events.Events():
			if e.Channel != string(eventchan.TrackerTimeEntriesUpdated) {
				continue
			}
			var got trackerapp.TimeEntriesEvent
			if err := json.Unmarshal(e.Data, &got); err != nil {
				t.Fatal(err)
			}
			return got
		case <-deadline:
			t.Fatalf("no tracker:time_entries_updated event %s", what)
		}
	}
}

// timeDraftRows are the thread's time draft timeline rows.
func timeDraftRows(t *testing.T, a *App, threadID string) []store.Item {
	t.Helper()
	items, err := a.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(items, func(it store.Item) bool { return it.ToolName != "time_draft" })
}

// An agent drafts a day through the time tools and nothing is logged; the
// call's one timeline row names the call and its day; the entries are read
// by the thread through ThreadTimeEntries; the person's approval through
// PostTrackerTimeEntries logs the ticket's hour in Jira, says the
// question is still open, and is told as an event; time_read then shows
// the hour logged. A read-only thread's draft is refused.
func TestAnAgentsTimeDraftPostsOnlyOnTheApproval(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	if got := workflowMCPToolNames(t, endpoint); !slices.Equal(got, trackertools.ToolNames(true)) {
		t.Fatalf("a session with a Jira connection sees %v", got)
	}
	day := time.Now().AddDate(0, 0, -2).Format(time.DateOnly)
	label, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	posts := func() int {
		n := 0
		for _, r := range f.e.Requests(0).Requests {
			if r.Route == "POST rest/api/3/issue/{issue}/worklog" {
				n++
			}
		}
		return n
	}

	text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{
		{"day": day, "key": "WEB-1", "hours": 1, "reason": "Built the form"},
		{"day": day, "hours": 0.5, "reason": "Which ticket was the standup?", "offer": []string{"WEB-3"}},
	}})
	if isError || !strings.Contains(text, `"note":"Nothing is logged until the user approves it."`) || !strings.Contains(text, `"key":"WEB-1"}],"questions":[`) {
		t.Fatalf("time_draft = %s (error %v)", text, isError)
	}
	if n := posts(); n != 0 {
		t.Fatalf("the draft logged %d worklogs", n)
	}
	if ev := f.timeEntriesEvent(t, "for the draft"); !slices.Equal(ev.Days, []string{day}) || !slices.Equal(ev.Threads, []string{thread.ID}) || ev.Connection != f.conn.ID {
		t.Fatalf("the draft told %+v", ev)
	}
	rows := timeDraftRows(t, f.a, thread.ID)
	if len(rows) != 1 || rows[0].Summary != "Drafted time for "+label.Format("Mon Jan 2") {
		t.Fatalf("timeline rows %+v", rows)
	}
	var meta struct {
		Kind, Call, Connection string
		Days                   []string
	}
	if err := json.Unmarshal([]byte(rows[0].Meta), &meta); err != nil || meta.Kind != "time_draft" || meta.Call == "" || meta.Connection != f.conn.ID || !slices.Equal(meta.Days, []string{day}) {
		t.Fatalf("the row's meta %s: %v", rows[0].Meta, err)
	}

	entries, err := f.a.ThreadTimeEntries(ctx, thread.ID, meta.Call)
	if err != nil || len(entries) != 2 || entries[0].Key != "WEB-1" || entries[0].Call != meta.Call || entries[1].Key != "" || entries[1].Offers[0].Key != "WEB-3" {
		t.Fatalf("ThreadTimeEntries = %+v, %v", entries, err)
	}
	refs := []trackerapp.TimeEntryRefView{{ID: entries[0].ID, Revision: entries[0].Revision}, {ID: entries[1].ID, Revision: entries[1].Revision}}
	got, err := f.a.PostTrackerTimeEntries(ctx, f.conn.ID, trackerapp.PostTimeRequest{Entries: refs})
	dayLabel := label.Format("Mon Jan 2")
	if err != nil || got.Sentence != fmt.Sprintf("Posted 1 of 1 entries for %s. 1 question for %s is still open.", dayLabel, dayLabel) || posts() != 1 {
		t.Fatalf("PostTrackerTimeEntries = %+v, %v with %d worklog posts", got, err, posts())
	}
	if ev := f.timeEntriesEvent(t, "for the post"); !slices.Equal(ev.Days, []string{day}) {
		t.Fatalf("the post told %+v", ev)
	}
	if len(timeDraftRows(t, f.a, thread.ID)) != 1 {
		t.Fatal("the post wrote a time draft row")
	}

	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "time_read", map[string]any{"from": day, "to": day})
	if isError || !strings.Contains(text, `"logged":[{"key":"WEB-1","title":"First story","hours":1,"description":"Built the form"}]`) ||
		!strings.Contains(text, `"questions":[{"id":`) || strings.Contains(text, `"drafted"`) {
		t.Fatalf("time_read = %s (error %v)", text, isError)
	}

	readOnly, endpoint := trackerWriteThread(t, f, provider.RuntimeReadOnly)
	text, isError = trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{{"day": day, "key": "WEB-2", "hours": 1, "reason": "Work"}}})
	if !isError || text != "[tracker_read_only] This thread is read-only, so it cannot change tickets; ask the user to change its access." {
		t.Fatalf("a read-only draft = %s (error %v)", text, isError)
	}
	if rows := timeDraftRows(t, f.a, readOnly.ID); len(rows) != 0 {
		t.Fatalf("the read-only thread drafted %+v", rows)
	}
}

// A computer whose only connection is Linear gives a session the ticket
// tools and not the time tools.
func TestTheTimeToolsNeedAJiraConnection(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.trackerMCPServer().Close() })
	withTrackerService(t, app)
	if _, err := app.trackers.Add(trackerapp.NewConnection{Kind: store.TrackerKindLinear, Site: "https://linear.app/example/"}); err != nil {
		t.Fatal(err)
	}
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	endpoint := trackerMCPEndpoint(t, app, thread, token)
	if got := workflowMCPToolNames(t, endpoint); !slices.Equal(got, trackertools.ToolNames(false)) {
		t.Fatalf("a session with only Linear sees %v", got)
	}
	text, isError := trackerMCPStreamedCall(t, app, endpoint, "time_read", map[string]any{})
	if !isError || text != `[tracker_request_invalid] There is no tool named "time_read". The tracker tools are: ticket_find, ticket_read, ticket_create, ticket_update, dashboard_widget.` {
		t.Fatalf("time_read on a Linear session = %s (error %v)", text, isError)
	}
}
