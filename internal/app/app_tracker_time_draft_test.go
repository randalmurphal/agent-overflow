package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackertools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
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

	entries, err := f.a.ThreadTimeEntries(ctx, thread.ID)
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
	if !isError || text != "[tracker_read_only] This thread is read-only, so it cannot change tickets or log time; ask the user to change its access." {
		t.Fatalf("a read-only draft = %s (error %v)", text, isError)
	}
	if got, err := f.a.ThreadTimeEntries(ctx, readOnly.ID); err != nil || len(got) != 0 {
		t.Fatalf("the read-only thread drafted %+v, %v", got, err)
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

// timePostRows are the thread's time post timeline rows.
func timePostRows(t *testing.T, a *App, threadID string) []store.Item {
	t.Helper()
	items, err := a.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(items, func(it store.Item) bool { return !strings.Contains(it.Meta, `"kind":"time_post"`) })
}

// An agent's post of a day waits for the person through the tracker
// write's own prompt, titled with the day and showing each entry; what it
// sends is exactly what that prompt showed: an entry the person edited,
// one they removed, and one another thread drafted while the prompt was
// open are not logged, and the answer names the first two as the prompt
// showed them, with why. The
// call's one row counts what the prompt offered. A second post, with
// nothing changed, logs both entries the prompt shows.
func TestAnAgentsPostLogsOnlyWhatThePersonSaw(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	_, other := trackerWriteThread(t, f, provider.RuntimeAuto)
	day := time.Now().AddDate(0, 0, -2).Format(time.DateOnly)
	label, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	dayLabel := label.Format("Mon Jan 2")
	posted := func() []string {
		var out []string
		for _, r := range f.e.Requests(0).Requests {
			if r.Route == "POST rest/api/3/issue/{issue}/worklog" {
				out = append(out, r.Path)
			}
		}
		return out
	}
	if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{
		{"day": day, "key": "WEB-1", "hours": 1, "reason": "Built the form"},
		{"day": day, "key": "WEB-2", "hours": 0.5, "reason": "Reviewed the pull request"},
	}}); isError {
		t.Fatalf("time_draft = %s", text)
	}

	done := trackerWriteCall(t, f.a, endpoint, "time_draft", map[string]any{"post": []string{day}})
	open, approval := trackerWritePrompt(t, f.a, thread.ID)
	if open.ToolName != "time_draft" || open.Title != "Log time for "+dayLabel || !strings.HasPrefix(open.RequestID, trackerApprovalPrefix) {
		t.Fatalf("the prompt is %q titled %q, id %q", open.ToolName, open.Title, open.RequestID)
	}
	var shown []string
	for _, tk := range approval.Tickets {
		for _, c := range tk.Changes {
			shown = append(shown, tk.Key+" "+tk.Title+": "+c.Line)
		}
	}
	if want := []string{"WEB-1 First story: " + dayLabel + ", 1h: Built the form", "WEB-2 Second task: " + dayLabel + ", 30m: Reviewed the pull request"}; !slices.Equal(shown, want) {
		t.Fatalf("the prompt shows %q, want %q", shown, want)
	}
	if got := posted(); len(got) != 0 {
		t.Fatalf("logged %v before the person answered", got)
	}
	// While the prompt is open: the person edits WEB-1 and removes WEB-2,
	// and another thread drafts WEB-3.
	entries, err := f.a.ThreadTimeEntries(ctx, thread.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ThreadTimeEntries = %+v, %v", entries, err)
	}
	two := 2.0
	if _, err := f.a.EditTrackerTimeEntry(ctx, f.conn.ID, trackerapp.TimeEntryRefView{ID: entries[0].ID, Revision: entries[0].Revision}, trackerapp.TimeEntryEdit{Hours: &two}); err != nil {
		t.Fatal(err)
	}
	if gone, err := f.a.RemoveTrackerTimeEntries(ctx, f.conn.ID, []trackerapp.TimeEntryRefView{{ID: entries[1].ID, Revision: entries[1].Revision}}); err != nil || len(gone) != 1 {
		t.Fatalf("remove = %+v, %v", gone, err)
	}
	if text, isError := trackerMCPStreamedCall(t, f.a, other, "time_draft", map[string]any{"entries": []map[string]any{
		{"day": day, "key": "WEB-3", "hours": 0.75, "reason": "Triaged the backlog"},
	}}); isError {
		t.Fatalf("the other thread's time_draft = %s", text)
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	result := <-done
	want := fmt.Sprintf(`{"logged":[],"not_logged":[{"why":"changed since it was read; read the day again","entries":[{"day":%[1]q,"key":"WEB-1","hours":1}]},`+
		`{"why":"was removed since it was read","entries":[{"day":%[1]q,"key":"WEB-2","hours":0.5}]}]}`, day)
	if result[1].(bool) || result[0] != want {
		t.Fatalf("the post answered %v\nwant %s", result, want)
	}
	if got := posted(); len(got) != 0 {
		t.Fatalf("logged %v, none of which the person saw as it is", got)
	}
	rows := timePostRows(t, f.a, thread.ID)
	if len(rows) != 1 || rows[0].Summary != "Logged 0 of 2 entries for "+dayLabel || rows[0].ID != open.RequestID {
		t.Fatalf("time post rows %+v", rows)
	}
	var meta struct {
		Kind, Call        string
		Connections, Days []string
		Posted, Of        int
	}
	if err := json.Unmarshal([]byte(rows[0].Meta), &meta); err != nil || meta.Kind != "time_post" || "tracker:"+meta.Call != open.RequestID ||
		!slices.Equal(meta.Connections, []string{f.conn.ID}) || !slices.Equal(meta.Days, []string{day}) || meta.Posted != 0 || meta.Of != 2 {
		t.Fatalf("the row's meta %s: %v", rows[0].Meta, err)
	}

	// Asked again, the prompt shows the day as it is now, and the approval
	// logs both entries.
	done = trackerWriteCall(t, f.a, endpoint, "time_draft", map[string]any{"post": []string{day}})
	open, approval = trackerWritePrompt(t, f.a, thread.ID)
	if len(approval.Tickets) != 2 || approval.Tickets[0].Changes[0].Line != dayLabel+", 2h: Built the form" || approval.Tickets[1].Key != "WEB-3" {
		t.Fatalf("the second prompt %+v", approval)
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	result = <-done
	if want := fmt.Sprintf(`{"logged":[{"day":%[1]q,"key":"WEB-1","hours":2},{"day":%[1]q,"key":"WEB-3","hours":0.75}]}`, day); result[1].(bool) || result[0] != want {
		t.Fatalf("the second post answered %v\nwant %s", result, want)
	}
	if got := posted(); len(got) != 2 {
		t.Fatalf("logged %v; want WEB-1 and WEB-3", got)
	}
	rows = timePostRows(t, f.a, thread.ID)
	if len(rows) != 2 || rows[1].Summary != "Logged 2 of 2 entries for "+dayLabel {
		t.Fatalf("time post rows %+v", rows)
	}
	// The first row is as the first post left it.
	if rows[0].Summary != "Logged 0 of 2 entries for "+dayLabel {
		t.Fatalf("the first row became %q", rows[0].Summary)
	}
}

// An approval of a post from a screen that did not draw its entries
// (olderApprovals) is refused: nothing is logged, and the prompt stays
// open for a screen that drew them, whose approval then logs the entry.
func TestAPostApprovedOnAScreenThatDrewNoEntryIsRefused(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	day := time.Now().AddDate(0, 0, -2).Format(time.DateOnly)
	if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{
		{"day": day, "key": "WEB-1", "hours": 1, "reason": "Built the form"},
	}}); isError {
		t.Fatalf("time_draft = %s", text)
	}
	posted := func() int {
		n := 0
		for _, r := range f.e.Requests(0).Requests {
			if r.Route == "POST rest/api/3/issue/{issue}/worklog" {
				n++
			}
		}
		return n
	}
	done := trackerWriteCall(t, f.a, endpoint, "time_draft", map[string]any{"post": []string{day}})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	for name, answer := range olderApprovals(open) {
		err := f.a.RespondToApproval(t.Context(), thread.ID, answer)
		if err == nil || errors.Is(err, transport.ErrAlreadyHandled) {
			t.Fatalf("%s's approval = %v; want a refusal that says the screen cannot show the change", name, err)
		}
		if n := posted(); n != 0 {
			t.Fatalf("%s's approval logged %d worklogs", name, n)
		}
		if !f.a.trackerApprovalOpen(thread.ID, open.RequestID) {
			t.Fatalf("%s's refused approval closed the prompt", name)
		}
	}
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
		t.Fatalf("the approval from a screen that drew it: %v", err)
	}
	if result := <-done; result[1].(bool) || posted() != 1 {
		t.Fatalf("the post answered %v with %d worklogs", result, posted())
	}
}

// A post may name days more than a month apart: each day's entries are
// read, and the drafted one is logged. A worklog whose answer is lost is
// one that may have been logged, in the answer and on the post's row.
func TestAPostOfDaysFarApartReadsEachDay(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	day := time.Now().AddDate(0, 0, -2).Format(time.DateOnly)
	far := time.Now().AddDate(0, 0, -60).Format(time.DateOnly)
	if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{
		{"day": day, "key": "WEB-1", "hours": 1, "reason": "Built the form"},
	}}); isError {
		t.Fatalf("time_draft = %s", text)
	}
	done := trackerWriteCall(t, f.a, endpoint, "time_draft", map[string]any{"post": []string{far, day}})
	waitFor(t, "the post's approval or its answer", func() bool {
		select {
		case result := <-done:
			t.Fatalf("the post of %s and %s answered %v before asking", far, day, result)
		default:
		}
		return slices.ContainsFunc(f.a.triage.PendingInteractiveRequests(thread.ID).Approvals, func(r provider.ApprovalRequest) bool { return r.Kind == triage.ApprovalKindTrackerWrite })
	})
	open, _ := trackerWritePrompt(t, f.a, thread.ID)
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{"logged":[{"day":%q,"key":"WEB-1","hours":1}]}`, day); func() bool { result := <-done; return result[1].(bool) || result[0] != want }() {
		t.Fatalf("the post of %s and %s did not answer %s", far, day, want)
	}

	if text, isError := trackerMCPStreamedCall(t, f.a, endpoint, "time_draft", map[string]any{"entries": []map[string]any{
		{"day": far, "key": "WEB-2", "hours": 0.5, "reason": "Reviewed the pull request"},
	}}); isError {
		t.Fatalf("time_draft = %s", text)
	}
	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: "POST rest/api/3/issue/{issue}/worklog", Kind: trackerfake.FaultFailAfterApply, Times: 1}); err != nil {
		t.Fatal(err)
	}
	done = trackerWriteCall(t, f.a, endpoint, "time_draft", map[string]any{"post": []string{far, day}})
	open, _ = trackerWritePrompt(t, f.a, thread.ID)
	if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result[1].(bool) || !strings.Contains(result[0].(string), `"why":"may already be logged`) || !strings.Contains(result[0].(string), `"key":"WEB-2","hours":0.5`) {
		t.Fatalf("the post whose answer was lost answered %v", result)
	}
	rows := timePostRows(t, f.a, thread.ID)
	farLabel, _ := time.Parse(time.DateOnly, far)
	dayLabel, _ := time.Parse(time.DateOnly, day)
	if want := "Logged 0 of 1 entries for " + farLabel.Format("Mon Jan 2") + " to " + dayLabel.Format("Mon Jan 2") + "; 1 may have been logged"; len(rows) != 2 || rows[1].Summary != want {
		t.Fatalf("time post rows %+v; want the second titled %q", rows, want)
	}
	var meta struct{ Posted, Of, Uncertain int }
	if err := json.Unmarshal([]byte(rows[1].Meta), &meta); err != nil || meta.Posted != 0 || meta.Of != 1 || meta.Uncertain != 1 {
		t.Fatalf("the row's meta %s: %v", rows[1].Meta, err)
	}
}
