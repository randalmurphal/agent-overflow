package trackertools

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/activity"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

var (
	timeJira   = trackerapp.TicketSource{ID: "conn-jira", Site: "example.atlassian.net", Kind: "jira", Account: "acc-user1"}
	timeJira2  = trackerapp.TicketSource{ID: "conn-jira-2", Site: "other.atlassian.net", Kind: "jira", Account: "acc-user1"}
	timeLinear = trackerapp.TicketSource{ID: "conn-linear", Site: "example", Kind: "linear"}
)

// timeCaller is a session given the time tools.
var timeCaller = Caller{Thread: "thread-1", Session: "session-1", Time: true}

// timeServer is a server over f and h whose today is Wednesday 2026-11-25.
func timeServer(f *fakeTracker, h *fakeHost) *Server {
	s := New(f, h)
	s.now = func() time.Time { return time.Date(2026, 11, 25, 15, 0, 0, 0, time.Local) }
	s.newID = func() string { return "call-1" }
	return s
}

func timeCall(t *testing.T, s *Server, name string, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Call(t.Context(), timeCaller, name, raw)
	if err != nil {
		return "", err
	}
	answer, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(answer), nil
}

// The time tools are listed, and answer, only for a session given them;
// their two definitions are pinned by size, the bytes every such session
// pays for.
func TestTheTimeToolsAreGivenWithAJiraConnection(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	var names []string
	for _, tool := range s.Tools(true) {
		names = append(names, tool["name"].(string))
	}
	if !slices.Equal(names, []string{"ticket_find", "ticket_read", "ticket_create", "ticket_update", "dashboard_widget", "time_read", "time_draft"}) || !slices.Equal(names, ToolNames(true)) {
		t.Fatalf("with the time tools = %v", names)
	}
	if got := len(s.Tools(false)); got != 5 {
		t.Fatalf("without the time tools %d tools are listed", got)
	}
	sizes := map[string]int{}
	for _, tool := range s.Tools(true)[5:] {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		sizes[tool["name"].(string)] = len(raw)
	}
	t.Logf("time_read %d bytes, time_draft %d bytes", sizes["time_read"], sizes["time_draft"])
	if sizes["time_read"] != 268 || sizes["time_draft"] != 687 {
		t.Errorf("the time tools are %v bytes; pinned time_read 268, time_draft 687", sizes)
	}
	_, err := s.Call(t.Context(), testCaller, "time_draft", json.RawMessage(`{}`))
	refused(t, err, CodeInvalidRequest, `There is no tool named "time_draft". The tracker tools are: ticket_find, ticket_read, ticket_create, ticket_update, dashboard_widget.`)
}

// time_read's days and their refusals, each asserted as the text the
// model reads.
func TestTimeReadsSevenDaysAtMost(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, time: map[string]trackerapp.WorklogDays{timeJira.ID: {}}}
	s := timeServer(f, &fakeHost{})
	for _, tc := range []struct {
		args    map[string]any
		asked   string
		refusal string
	}{
		{map[string]any{}, "time conn-jira 2026-11-23 2026-11-29", ""},
		{map[string]any{"from": "2026-11-02"}, "time conn-jira 2026-11-02 2026-11-08", ""},
		{map[string]any{"to": "2026-11-08"}, "time conn-jira 2026-11-02 2026-11-08", ""},
		{map[string]any{"from": "2026-11-02", "to": "2026-11-02"}, "time conn-jira 2026-11-02 2026-11-02", ""},
		{map[string]any{"from": "Monday"}, "", "from and to take YYYY-MM-DD."},
		{map[string]any{"from": "2026-11-09", "to": "2026-11-02"}, "", "to is before from."},
		{map[string]any{"from": "2026-11-02", "to": "2026-11-09"}, "", "time_read covers at most 7 days; ask again for the rest."},
	} {
		f.asked = nil
		_, err := timeCall(t, s, "time_read", tc.args)
		if tc.refusal != "" {
			refused(t, err, CodeInvalidRequest, tc.refusal)
			continue
		}
		if err != nil || !slices.Equal(f.asked, []string{tc.asked}) {
			t.Fatalf("time_read %v asked %v, %v; want %s", tc.args, f.asked, err, tc.asked)
		}
	}
	f.sources = []trackerapp.TicketSource{timeLinear}
	_, err := timeCall(t, s, "time_read", map[string]any{})
	refused(t, err, CodeNoConnection, "This computer has no Jira connection turned on, and time is kept on Jira. The user adds or turns one on in the Issues settings.")
}

// A time_read answers each day of the range with its logged worklogs,
// the draft's entries not yet logged and its open questions; a
// connection whose read failed is named with what it says, and the call
// fails only when every one did.
func TestTimeReadAnswersEachDay(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira, timeLinear, timeJira2}, time: map[string]trackerapp.WorklogDays{
		timeJira.ID: {
			Worklogs: []trackerapp.WorklogView{{ID: "20001", Key: "WEB-1", Title: "First story", Day: "2026-11-24", SpentSeconds: 5400, Description: "Built the form\n"}},
			Drafted: []trackerapp.TimeEntryView{
				{ID: 1, Day: "2026-11-24", Key: "WEB-1", Minutes: 90, Reason: "Built the form", By: "agent", State: "posted"},
				{ID: 2, Day: "2026-11-24", Key: "WEB-2", Minutes: 30, Reason: "Reviewed", By: "person", State: "drafted"},
				{ID: 3, Day: "2026-11-25", Minutes: 15, Reason: "Standup", By: "agent", State: "drafted",
					Offers: []trackerapp.TimeOfferView{{Issue: "10103", Key: "WEB-3"}}},
				{ID: 4, Day: "2026-11-25", Minutes: 15, Reason: "Left out", By: "person", State: "left_out"},
			},
		},
	}}
	s := timeServer(f, &fakeHost{})
	answer, err := timeCall(t, s, "time_read", map[string]any{"from": "2026-11-24", "to": "2026-11-25"})
	want := `{"days":[{"day":"2026-11-24","logged":[{"key":"example.atlassian.net/WEB-1","title":"First story","hours":1.5,"description":"Built the form"}],` +
		`"drafted":[{"id":2,"key":"example.atlassian.net/WEB-2","hours":0.5,"reason":"Reviewed","by":"you","state":"drafted"}]},` +
		`{"day":"2026-11-25","logged":[],"questions":[{"id":3,"hours":0.25,"reason":"Standup","offer":["example.atlassian.net/WEB-3"]}]}],` +
		`"unread":[{"site":"other.atlassian.net","error":"this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings"}]}`
	if err != nil || answer != want {
		t.Fatalf("time_read = %s, %v\nwant %s", answer, err, want)
	}
	// One connection refused and the other read from the mirror after its
	// refresh failed: the answer holds the mirror's copy, naming both.
	f.time[timeJira.ID] = trackerapp.WorklogDays{Worklogs: []trackerapp.WorklogView{}, Unread: []string{"2026-11-25"},
		Failure: &trackerapp.FailureView{Kind: "tracker", Message: "Jira did not answer in time; try again"}}
	answer, err = timeCall(t, s, "time_read", map[string]any{"from": "2026-11-24", "to": "2026-11-25"})
	if err != nil || !strings.Contains(answer, `"unread":[{"site":"example.atlassian.net","error":"Jira did not answer in time; try again","days":["2026-11-25"]},{"site":"other.atlassian.net"`) {
		t.Fatalf("time_read with one refusal and one failed refresh = %s, %v", answer, err)
	}
	delete(f.time, timeJira.ID)
	_, err = timeCall(t, s, "time_read", map[string]any{})
	refused(t, err, "tracker_connection_unverified", "example.atlassian.net: this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings")
}

// time_draft resolves each key, an entry's or an offer's, to its Jira
// ticket and drafts them on that connection; a call that changed the
// draft writes one timeline row naming the call and its days; the answer
// is by day, with the hours logged and the note that nothing is logged.
func TestTimeDraftResolvesItsKeys(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, lookup: map[string]map[string]string{timeJira.ID: {"WEB-1": "10101", "WEB-2": "10102", "WEB-3": "10103"}},
		details: map[string]trackerapp.DetailView{"conn-jira/10101": {Card: trackerapp.CardView{Title: "First story"}}, "conn-jira/10103": {Card: trackerapp.CardView{Title: "Backlog task"}}}}
	f.draft = func(connection string, in trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error) {
		return trackerapp.TimeDraftView{Author: "acc-user1",
			Made: []trackerapp.TimeEntryView{
				{ID: 7, Day: "2026-11-24", Key: "WEB-1", Minutes: 90},
				{ID: 8, Day: "2026-11-24", Minutes: 30, Reason: "Standup", Offers: []trackerapp.TimeOfferView{{Key: "WEB-3"}}},
			},
			Kept:    []trackerapp.TimeEntryView{{ID: 2, Day: "2026-11-24", Key: "WEB-2", Minutes: 30, By: "person", State: "drafted"}},
			Skipped: []trackerapp.TimeEntryView{{Day: "2026-11-24", Key: "WEB-2", Minutes: 60}},
			Logged:  []trackerapp.LoggedDay{{Day: "2026-11-24", Minutes: 60}}}, nil
	}
	h := &fakeHost{}
	s := timeServer(f, h)
	answer, err := timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{
		{"day": "2026-11-24", "key": "web-1", "hours": 1.5, "reason": "Built the form"},
		{"day": "2026-11-24", "hours": 0.5, "reason": "Standup", "offer": []string{"WEB-3"}},
		{"day": "2026-11-24", "key": "WEB-2", "hours": 1, "reason": "Reviewed"},
	}})
	want := `{"days":[{"day":"2026-11-24","made":[{"id":7,"key":"WEB-1"}],"questions":[8],"other_hours":0.5,"skipped":["WEB-2"],"logged_hours":1}],` +
		`"note":"Nothing is logged until the user approves it. A skipped key already has an entry of that day not yet logged; time_read shows it."}`
	if err != nil || answer != want {
		t.Fatalf("time_draft = %s, %v\nwant %s", answer, err, want)
	}
	wantDraft := `conn-jira [{"Day":"2026-11-24","Ticket":{"issue":"10101","key":"WEB-1","title":"First story"},"Hours":1.5,"Reason":"Built the form","Offers":null},` +
		`{"Day":"2026-11-24","Ticket":null,"Hours":0.5,"Reason":"Standup","Offers":[{"issue":"10103","key":"WEB-3","title":"Backlog task"}]},` +
		`{"Day":"2026-11-24","Ticket":{"issue":"10102","key":"WEB-2"},"Hours":1,"Reason":"Reviewed","Offers":null}]`
	if len(f.drafts) != 1 || f.drafts[0] != wantDraft {
		t.Fatalf("drafts = %v\nwant %s", f.drafts, wantDraft)
	}
	if !reflect.DeepEqual(h.timeRows, []TimeRow{{ID: "call-1", Connection: timeJira.ID, Days: []string{"2026-11-24"}}}) {
		t.Fatalf("timeline rows %+v", h.timeRows)
	}
	// A draft that changed nothing writes no row.
	f.draft = func(string, trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error) {
		return trackerapp.TimeDraftView{Skipped: []trackerapp.TimeEntryView{{Day: "2026-11-24", Key: "WEB-1", Minutes: 60}}, Logged: []trackerapp.LoggedDay{{Day: "2026-11-24", Unread: true}}}, nil
	}
	answer, err = timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{{"day": "2026-11-24", "key": "WEB-1", "hours": 1, "reason": "Again"}}})
	if err != nil || len(h.timeRows) != 1 || !strings.Contains(answer, `"skipped":["WEB-1"]`) || strings.Contains(answer, "logged_hours") {
		t.Fatalf("a draft that changed nothing = %s, %v; rows %+v", answer, err, h.timeRows)
	}
	// With another connection on, the answer names the site once and its
	// keys bare.
	f.sources = []trackerapp.TicketSource{timeJira, timeLinear}
	answer, err = timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{{"day": "2026-11-24", "key": "WEB-1", "hours": 1, "reason": "Again"}}})
	if err != nil || !strings.HasPrefix(answer, `{"site":"example.atlassian.net","days":[{"day":"2026-11-24","skipped":["WEB-1"]`) {
		t.Fatalf("a draft with two connections on = %s, %v", answer, err)
	}
}

// Each time_draft refusal, asserted as the text the model reads; none
// reaches the draft.
func TestTimeDraftRefusals(t *testing.T) {
	t.Parallel()
	entry := func(key string) map[string]any {
		e := map[string]any{"day": "2026-11-24", "hours": 1, "reason": "Work"}
		if key != "" {
			e["key"] = key
		}
		return e
	}
	for _, tc := range []struct {
		name     string
		sources  []trackerapp.TicketSource
		readOnly bool
		entries  []map[string]any
		code     string
		want     string
	}{
		{"no entries", []trackerapp.TicketSource{timeJira}, false, []map[string]any{}, CodeInvalidRequest,
			"Give entries to draft time, or post to ask the user to log the draft of those days."},
		{"read-only", []trackerapp.TicketSource{timeJira}, true, []map[string]any{entry("WEB-1")}, "tracker_read_only",
			"This thread is read-only, so it cannot change tickets or log time; ask the user to change its access."},
		{"no Jira", []trackerapp.TicketSource{timeLinear}, false, []map[string]any{entry("ENG-4")}, CodeNoConnection,
			"This computer has no Jira connection turned on, and time is kept on Jira. The user adds or turns one on in the Issues settings."},
		{"a Linear key", []trackerapp.TicketSource{timeJira, timeLinear}, false, []map[string]any{entry("WEB-1"), entry("ENG-4")}, CodeInvalidRequest,
			"entries[1].key ENG-4 is on Linear, which has no time tracking."},
		{"two sites", []trackerapp.TicketSource{timeJira, timeJira2}, false, []map[string]any{entry("example.atlassian.net/WEB-1"), entry("other.atlassian.net/OPS-1")}, CodeInvalidRequest,
			"entries[0].key example.atlassian.net/WEB-1 is on example.atlassian.net and entries[1].key other.atlassian.net/OPS-1 on other.atlassian.net; draft each site's time in its own call."},
		{"no key and two Jira connections", []trackerapp.TicketSource{timeJira, timeJira2}, false, []map[string]any{entry("")}, CodeInvalidRequest,
			"No entry has a key, and this computer has 2 Jira connections; give one entry a key on the site whose time this is."},
		{"an empty offer", []trackerapp.TicketSource{timeJira}, false, []map[string]any{{"day": "2026-11-24", "hours": 1, "reason": "Which?", "offer": []string{" "}}}, CodeInvalidRequest,
			"entries[0].offer holds an empty key."},
		{"an unknown key", []trackerapp.TicketSource{timeJira}, false, []map[string]any{entry("WEB-99")}, CodeNotFound, "No ticket WEB-99 on example.atlassian.net."},
		{"six offers", []trackerapp.TicketSource{timeJira}, false, []map[string]any{{"day": "2026-11-24", "hours": 1, "reason": "Call",
			"offer": []string{"WEB-1", "WEB-2", "WEB-3", "WEB-4", "WEB-5", "WEB-6"}}}, CodeInvalidRequest, "entries[0].offer holds at most 5 keys."},
	} {
		f := &fakeTracker{sources: tc.sources, lookup: map[string]map[string]string{
			timeJira.ID: {"WEB-1": "10101"}, timeLinear.ID: {"ENG-4": "lin-4"}, timeJira2.ID: {"OPS-1": "20101"}}}
		s := timeServer(f, &fakeHost{readOnly: tc.readOnly})
		_, err := timeCall(t, s, "time_draft", map[string]any{"entries": tc.entries})
		if code, message, _ := errorsx.PublicDetails(err); code != tc.code || message != tc.want {
			t.Errorf("%s: %v\nwant [%s] %s", tc.name, err, tc.code, tc.want)
		}
		if len(f.drafts) != 0 {
			t.Errorf("%s reached the draft: %v", tc.name, f.drafts)
		}
		if tc.name == "six offers" && len(f.asked) != 0 {
			t.Errorf("six offers were looked up before the refusal: %v", f.asked)
		}
	}
	// The thread's linked ticket on a connection no longer on.
	gone := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, link: &trackerapp.LinkedTicket{Connection: "conn-gone", Site: "old.atlassian.net", Issue: "30101", Key: "OLD-1"}}
	_, err := timeCall(t, timeServer(gone, &fakeHost{}), "time_draft", map[string]any{"entries": []map[string]any{entry("OLD-1")}})
	refused(t, err, CodeInvalidRequest, "entries[0].key OLD-1 is on a connection that is not turned on.")
	// A thread made read-only during the key lookup drafts nothing.
	h := &fakeHost{}
	late := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, lookup: map[string]map[string]string{timeJira.ID: {"WEB-1": "10101"}},
		onLookup: func() { h.readOnly = true }}
	_, err = timeCall(t, timeServer(late, h), "time_draft", map[string]any{"entries": []map[string]any{entry("WEB-1")}})
	if code, _, _ := errorsx.PublicDetails(err); code != "tracker_read_only" || len(late.drafts) != 0 || len(late.asked) != 1 {
		t.Fatalf("a draft whose thread was made read-only during its lookup: %v; drafts %v, asked %v", err, late.drafts, late.asked)
	}
	// A refusal of the draft's own rules is trackerapp's sentence, as is.
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, draft: func(string, trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error) {
		return trackerapp.TimeDraftView{}, errorsx.Public(CodeInvalidRequest, "entries[0].hours must be more than 0.", nil)
	}}
	_, err = timeCall(t, timeServer(f, &fakeHost{}), "time_draft", map[string]any{"entries": []map[string]any{{"day": "2026-11-24", "hours": 0, "reason": "Which?"}}})
	refused(t, err, CodeInvalidRequest, "entries[0].hours must be more than 0.")
}

// A draft's answer is a receipt: the IDs it made by day, with no text the
// agent sent echoed back, and no entry it did not make listed, so it stays
// within the answer budget at the most entries a call takes; the hours
// the day's other open entries hold are summed.
func TestTimeDraftAnswersWithAReceipt(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1024)
	offers := []trackerapp.TimeOfferView{}
	for i := range 5 {
		key := fmt.Sprintf("example.atlassian.net/LONGPROJECT-%d", 100000+i)
		offers = append(offers, trackerapp.TimeOfferView{Issue: key, Key: key})
	}
	v := trackerapp.TimeDraftView{}
	for i := range 250 {
		v.Made = append(v.Made, trackerapp.TimeEntryView{ID: int64(1_000_000 + i), Day: "2026-11-24", Minutes: 1, Reason: long, Offers: offers, State: "drafted"})
	}
	v.Kept = []trackerapp.TimeEntryView{
		{ID: 8, Day: "2026-11-24", Key: "WEB-1", Minutes: 60, Reason: long, State: "drafted", By: "person"},
		{ID: 9, Day: "2026-11-24", Key: "WEB-2", Minutes: 30, Reason: long, State: "unknown"},
	}
	got := draftAnswerOf(v, "")
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > answerBudget || strings.Contains(string(raw), long) {
		t.Fatalf("the answer is %d bytes, over %d or echoing the reasons", len(raw), answerBudget)
	}
	if strings.Contains(string(raw), `"id":8`) || strings.Contains(string(raw), `"id":9`) {
		t.Fatalf("the answer lists entries the call did not make: %s", raw)
	}
	t.Logf("the answer of 250 questions is %d bytes", len(raw))
	if len(got.Days) != 1 || got.Days[0].OtherHours != 1.5 {
		t.Fatalf("the day's other hours are %+v; want the 1.5 of the entries not yet logged", got.Days)
	}

	// 250 entries on tickets over 25 days of a site with a long name, which
	// a session with two connections names once.
	site := strings.Repeat("s", 55) + ".atlassian.net"
	v = trackerapp.TimeDraftView{}
	for i := range 250 {
		day := fmt.Sprintf("2026-10-%02d", 1+i/10)
		v.Made = append(v.Made, trackerapp.TimeEntryView{ID: int64(1_000_000 + i), Day: day, Key: fmt.Sprintf("LONGPROJECT-%d", 100000+i), Minutes: 1, Reason: long, State: "drafted"})
	}
	answer := draftAnswerOf(v, site)
	raw, err = json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the answer of 250 entries on %d-byte site is %d bytes", len(site), len(raw))
	if len(raw) > answerBudget || strings.Count(string(raw), site) != 1 {
		t.Fatalf("the answer of 250 entries on tickets is %d bytes, over %d, or names the site more than once", len(raw), answerBudget)
	}
}

// time_read answers what this computer measured of each day beside its
// logged time: the active minutes and the phone's share when there is
// any, the minutes on each Jira ticket the focused threads are linked to
// with a ticket only a turn touched named without minutes and a ticket
// of a site that is not on named with its site, at most commitsShown
// commits and how many more, the recording gap while there is one, and
// each repository whose commits could not be read; a day with nothing
// measured carries none of it, and a failed read of the activity fails
// the call.
func TestTimeReadAnswersTheMeasuredDay(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, time: map[string]trackerapp.WorklogDays{timeJira.ID: {}},
		links: map[string]trackerapp.JiraTicket{
			"t1": {Site: timeJira.Site, Key: "WEB-3"}, "t2": {Site: timeJira.Site, Key: "WEB-2"}, "t4": {Site: timeJira.Site, Key: "WEB-4"},
			"t5": {Site: timeJira2.Site, Key: "WEB-5"},
		}}
	day := activity.Day{Day: "2026-11-24", Active: 124*time.Minute + 40*time.Second, Phone: 30 * time.Minute, Threads: map[string]time.Duration{
		"t1": 89*time.Minute + 40*time.Second, "t2": 10 * time.Minute, "t3": 20 * time.Minute, "t5": 15 * time.Minute, "": 5 * time.Minute,
	}, Ran: []string{"t1", "t4"}}
	var commits []Commit
	for i := range commitsShown + 2 {
		commits = append(commits, Commit{At: time.Date(2026, 11, 24, 9, i, 0, 0, time.Local), Subject: fmt.Sprintf("Change %d", i)})
	}
	gap := time.Date(2026, 11, 25, 14, 5, 0, 0, time.Local)
	h := &fakeHost{activity: ActivityDays{Days: []ActivityDay{{Day: day, Commits: commits}}, Gap: gap.UnixMilli(),
		CommitsUnread: []string{"Shop: git log in /src/shop failed: fatal: bad object HEAD"}}}
	answer, err := timeCall(t, timeServer(f, h), "time_read", map[string]any{"from": "2026-11-24", "to": "2026-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := range commitsShown {
		lines = append(lines, fmt.Sprintf(`{"at":"09:%02d","subject":"Change %d"}`, i, i))
	}
	want := `{"days":[{"day":"2026-11-24","logged":[],"active":125,"phone":30,"tickets":[{"key":"WEB-3","minutes":90},` +
		`{"key":"other.atlassian.net/WEB-5","minutes":15},{"key":"WEB-2","minutes":10},{"key":"WEB-4"}],"commits":[` + strings.Join(lines, ",") + `],"commits_more":2},{"day":"2026-11-25","logged":[]}],"activity_gap":"since 2026-11-25 14:05",` +
		`"commits_unread":["Shop: git log in /src/shop failed: fatal: bad object HEAD"]}`
	if answer != want {
		t.Fatalf("time_read = %s\nwant %s", answer, want)
	}
	f.linksErr = errors.New("database is locked")
	if _, err := timeCall(t, timeServer(f, h), "time_read", map[string]any{"from": "2026-11-24", "to": "2026-11-25"}); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("a failed read of the threads' tickets answered %v", err)
	}
	f.linksErr = nil
	h.activityErr = errors.New("disk I/O error")
	if _, err := timeCall(t, timeServer(f, h), "time_read", map[string]any{"from": "2026-11-24", "to": "2026-11-25"}); err == nil || !strings.Contains(err.Error(), "disk I/O error") {
		t.Fatalf("a failed activity read answered %v", err)
	}
}

// Measured minutes round to the nearest: half a minute is one, and
// anything shorter is none, so a ticket focused that briefly is named
// without minutes and the day's phone time is left out.
func TestTimeReadRoundsMeasuredMinutesToTheNearest(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, time: map[string]trackerapp.WorklogDays{timeJira.ID: {}},
		links: map[string]trackerapp.JiraTicket{"t1": {Site: timeJira.Site, Key: "WEB-1"}, "t2": {Site: timeJira.Site, Key: "WEB-2"}}}
	day := activity.Day{Day: "2026-11-24", Active: 59 * time.Second, Phone: 29 * time.Second,
		Threads: map[string]time.Duration{"t1": 30 * time.Second, "t2": 29 * time.Second}}
	h := &fakeHost{activity: ActivityDays{Days: []ActivityDay{{Day: day}}}}
	answer, err := timeCall(t, timeServer(f, h), "time_read", map[string]any{"from": "2026-11-24", "to": "2026-11-24"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"days":[{"day":"2026-11-24","logged":[],"active":1,"tickets":[{"key":"WEB-1","minutes":1},{"key":"WEB-2"}]}]}`
	if answer != want {
		t.Fatalf("time_read = %s\nwant %s", answer, want)
	}
}

// The bytes a week's measured activity adds to time_read's answer, for a
// day of 7 hours on 3 tickets, a fourth ticket touched, half an hour on
// the phone and 4 commits.
func TestTimeReadsWeekOfActivityBytes(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, time: map[string]trackerapp.WorklogDays{timeJira.ID: {}},
		links: map[string]trackerapp.JiraTicket{
			"t1": {Site: timeJira.Site, Key: "WEB-101"}, "t2": {Site: timeJira.Site, Key: "WEB-102"}, "t3": {Site: timeJira.Site, Key: "WEB-103"},
			"t4": {Site: timeJira.Site, Key: "WEB-104"},
		}}
	var days []ActivityDay
	for d := range 5 {
		name := time.Date(2026, 11, 23+d, 0, 0, 0, 0, time.Local).Format(time.DateOnly)
		var commits []Commit
		for i := range 4 {
			commits = append(commits, Commit{At: time.Date(2026, 11, 23+d, 10+i, 17, 0, 0, time.Local), Subject: "WEB-101: Handle the empty cart on checkout"})
		}
		days = append(days, ActivityDay{Day: activity.Day{Day: name, Active: 7 * time.Hour, Phone: 30 * time.Minute,
			Threads: map[string]time.Duration{"t1": 3 * time.Hour, "t2": 2 * time.Hour, "t3": 90 * time.Minute, "": 30 * time.Minute},
			Ran:     []string{"t1", "t2", "t3", "t4"}}, Commits: commits})
	}
	read := func(h *fakeHost) string {
		answer, err := timeCall(t, timeServer(f, h), "time_read", map[string]any{"from": "2026-11-23", "to": "2026-11-29"})
		if err != nil {
			t.Fatal(err)
		}
		return answer
	}
	without, with := read(&fakeHost{}), read(&fakeHost{activity: ActivityDays{Days: days}})
	t.Logf("a week's answer is %d bytes without the activity and %d with it: %d more", len(without), len(with), len(with)-len(without))
}

// timeTicket is a ticket of timeJira's site a project's Time settings name.
func timeTicket(key string) *trackerapp.TimeTicket {
	return &trackerapp.TimeTicket{Tracker: "jira", Site: "https://" + timeJira.Site, Key: key}
}

// time_read names the tickets the project's Time settings put meeting and
// time-off hours on, as its answers write keys, and nothing when none is
// set; settings this build cannot read fail the call with their sentence.
func TestTimeReadNamesTheProjectsMeetingsAndTimeOffTickets(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, time: map[string]trackerapp.WorklogDays{timeJira.ID: {}, timeJira2.ID: {}}}
	h := &fakeHost{}
	s := timeServer(f, h)
	day := map[string]any{"from": "2026-11-24", "to": "2026-11-24"}
	answer, err := timeCall(t, s, "time_read", day)
	if err != nil || answer != `{"days":[{"day":"2026-11-24","logged":[]}]}` {
		t.Fatalf("time_read with no Time settings = %s, %v", answer, err)
	}
	h.time = trackerapp.TimeSettings{Meetings: timeTicket("OPS-1"), TimeOff: timeTicket("OPS-2")}
	answer, err = timeCall(t, s, "time_read", day)
	if want := `{"days":[{"day":"2026-11-24","logged":[]}],"meetings_ticket":"OPS-1","time_off_ticket":"OPS-2"}`; err != nil || answer != want {
		t.Fatalf("time_read = %s, %v\nwant %s", answer, err, want)
	}
	f.sources = []trackerapp.TicketSource{timeJira, timeJira2}
	h.time = trackerapp.TimeSettings{TimeOff: timeTicket("OPS-2")}
	answer, err = timeCall(t, s, "time_read", day)
	if err != nil || !strings.HasSuffix(answer, `"time_off_ticket":"example.atlassian.net/OPS-2"}`) || strings.Contains(answer, "meetings_ticket") {
		t.Fatalf("time_read with two connections = %s, %v", answer, err)
	}
	h.timeErr = errorsx.Public("tracker_setup_newer", "A newer version of Agent Overflow saved this project's Time settings, so this version cannot tell where its time goes; ask the user to update the app.", nil)
	_, err = timeCall(t, s, "time_read", day)
	refused(t, err, "tracker_setup_newer", "A newer version of Agent Overflow saved this project's Time settings, so this version cannot tell where its time goes; ask the user to update the app.")
}

// A draft entry on a meetings or time-off ticket that Jira no longer
// finds is refused with what to do, the setting named; another key that
// is not found keeps the lookup's own sentence.
func TestTimeDraftSaysASettingsTicketThatIsGone(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, lookup: map[string]map[string]string{timeJira.ID: {"WEB-1": "10101"}}}
	h := &fakeHost{time: trackerapp.TimeSettings{Meetings: timeTicket("OPS-1"), TimeOff: timeTicket("OPS-2")}}
	s := timeServer(f, h)
	entry := func(key string) map[string]any {
		return map[string]any{"day": "2026-11-24", "key": key, "hours": 1, "reason": "Standup"}
	}
	_, err := timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{entry("WEB-1"), entry("ops-1")}})
	refused(t, err, CodeNotFound, "OPS-1, this project's meetings ticket in Settings > Issues > Time, cannot be found on example.atlassian.net, or this connection cannot see it. "+
		"Ask the user which ticket: leave out key, and with more than one Jira connection on, offer a ticket of example.atlassian.net unless another entry has a key there. Tell them to choose another meetings ticket in those settings.")
	_, err = timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{{"day": "2026-11-24", "hours": 8, "reason": "Holiday", "offer": []string{"OPS-2"}}}})
	refused(t, err, CodeNotFound, "OPS-2, this project's time-off ticket in Settings > Issues > Time, cannot be found on example.atlassian.net, or this connection cannot see it. "+
		"Ask the user which ticket: leave out key, and with more than one Jira connection on, offer a ticket of example.atlassian.net unless another entry has a key there. Tell them to choose another time-off ticket in those settings.")
	_, err = timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{entry("WEB-99")}})
	refused(t, err, CodeNotFound, "No ticket WEB-99 on example.atlassian.net.")
	if len(f.drafts) != 0 {
		t.Fatalf("a refused draft reached the draft: %v", f.drafts)
	}

	// With one Jira connection on, the advice's question with no key and
	// no offer drafts.
	f.draft = func(string, trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error) {
		return trackerapp.TimeDraftView{}, nil
	}
	if _, err := timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{{"day": "2026-11-24", "hours": 8, "reason": "Holiday"}}}); err != nil || len(f.drafts) != 1 {
		t.Fatalf("the question the advice asks for on one connection: %v, drafts %v", err, f.drafts)
	}

	// With two Jira connections on, a day's only entry on the gone
	// time-off ticket follows the advice: a question offering a ticket of
	// the site, which drafts.
	f.sources = []trackerapp.TicketSource{timeJira, timeJira2}
	holiday := map[string]any{"day": "2026-11-24", "hours": 8, "reason": "Holiday"}
	_, err = timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{{"day": "2026-11-24", "key": "example.atlassian.net/OPS-2", "hours": 8, "reason": "Holiday"}}})
	if code, message, _ := errorsx.PublicDetails(err); code != CodeNotFound || !strings.Contains(message, "with more than one Jira connection on, offer a ticket of example.atlassian.net unless another entry has a key there") {
		t.Fatalf("the gone time-off ticket with two connections on: %v", err)
	}
	holiday["offer"] = []string{"example.atlassian.net/WEB-1"}
	if _, err := timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{holiday}}); err != nil || len(f.drafts) != 2 {
		t.Fatalf("the question the advice asks for: %v, drafts %v", err, f.drafts)
	}
}

// With Never log to an epic on, the default, an entry whose own ticket
// the lookup or the mirror types as an epic is refused, asking nothing
// more of the tracker; an offer of an epic, an entry whose type neither
// has, and any entry with the rule off are drafted.
func TestTimeDraftKeepsTimeOffEpics(t *testing.T) {
	t.Parallel()
	epic := &trackerapp.IssueTypeView{ID: "10000", Name: "Epic", Level: 1}
	story := &trackerapp.IssueTypeView{ID: "10001", Name: "Story", Level: 0}
	newTracker := func() *fakeTracker {
		f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira},
			lookup:  map[string]map[string]string{timeJira.ID: {"WEB-1": "10101", "WEB-9": "10109"}},
			details: map[string]trackerapp.DetailView{"conn-jira/10101": {Card: trackerapp.CardView{Title: "First story", Type: story}}, "conn-jira/10109": {Card: trackerapp.CardView{Title: "The epic", Type: epic}}},
			// The mirror holds WEB-8, an epic, and WEB-7, whose card has no type.
			keyed: map[string][]trackerapp.KeyedTicket{"WEB-8": {{Connection: timeJira.ID, Issue: "10108"}}, "WEB-7": {{Connection: timeJira.ID, Issue: "10107"}}},
			cards: map[string]trackerapp.CardView{"conn-jira/10108": {Title: "Mirrored epic", Type: epic}, "conn-jira/10107": {Title: "Untyped"}}}
		f.draft = func(string, trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error) {
			return trackerapp.TimeDraftView{}, nil
		}
		return f
	}
	entry := func(key string, offer ...string) map[string]any {
		e := map[string]any{"day": "2026-11-24", "hours": 1, "reason": "Work"}
		if key != "" {
			e["key"] = key
		}
		if len(offer) > 0 {
			e["offer"] = offer
		}
		return e
	}
	for _, tc := range []struct {
		name    string
		off     bool
		entries []map[string]any
		want    string
	}{
		{"an epic the lookup found", false, []map[string]any{entry("WEB-1"), entry("WEB-9")},
			"entries[1].key WEB-9 is an epic, and this project's Time settings keep time off epics; put the hours on a ticket under it, or leave out key and offer tickets under it."},
		{"an epic the mirror holds", false, []map[string]any{entry("WEB-8")},
			"entries[0].key WEB-8 is an epic, and this project's Time settings keep time off epics; put the hours on a ticket under it, or leave out key and offer tickets under it."},
		{"a story", false, []map[string]any{entry("WEB-1")}, ""},
		{"an offer of an epic", false, []map[string]any{entry("", "WEB-9", "WEB-1")}, ""},
		{"a ticket of no known type", false, []map[string]any{entry("WEB-7")}, ""},
		{"an epic with the rule off", true, []map[string]any{entry("WEB-9"), entry("WEB-8")}, ""},
	} {
		f := newTracker()
		h := &fakeHost{}
		if tc.off {
			off := false
			h.time.NeverEpics = &off
		}
		_, err := timeCall(t, timeServer(f, h), "time_draft", map[string]any{"entries": tc.entries})
		switch {
		case tc.want != "":
			if code, message, _ := errorsx.PublicDetails(err); code != CodeInvalidRequest || message != tc.want || len(f.drafts) != 0 {
				t.Errorf("%s: %v, drafts %v\nwant %s", tc.name, err, f.drafts, tc.want)
			}
		case err != nil || len(f.drafts) != 1:
			t.Errorf("%s: %v, drafts %v; want it drafted", tc.name, err, f.drafts)
		}
		if lookups := slices.DeleteFunc(slices.Clone(f.asked), func(a string) bool { return !strings.HasPrefix(a, "lookup ") }); len(lookups) > 1 {
			t.Errorf("%s asked %v; want at most the one lookup of its keys", tc.name, lookups)
		}
	}
}

// A key is written bare only when the one connection on is the key's own
// site: a key of another site, or any key while several are on, carries
// its site, so it opens that site's ticket again.
func TestAKeyIsBareOnlyOnTheOneSiteOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		sources []trackerapp.TicketSource
		want    string
	}{
		{"one source, the key's site", []trackerapp.TicketSource{timeJira}, "OPS-1"},
		{"one source, another site", []trackerapp.TicketSource{timeJira2}, "example.atlassian.net/OPS-1"},
		{"several sources", []trackerapp.TicketSource{timeJira, timeJira2}, "example.atlassian.net/OPS-1"},
	} {
		if got := qualifier(tc.sources, timeJira.Site)("OPS-1"); got != tc.want {
			t.Errorf("%s: %q; want %q", tc.name, got, tc.want)
		}
	}
}

// With the meetings ticket's site turned off and another site on that has
// a ticket of the same key, time_read names the meetings ticket with its
// site, and a draft of that key is refused, drafting nothing on the other
// site.
func TestASettingsTicketOfASiteTurnedOffIsNotDraftedOnAnother(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira2}, time: map[string]trackerapp.WorklogDays{timeJira2.ID: {}},
		lookup: map[string]map[string]string{timeJira2.ID: {"OPS-1": "20101"}}}
	h := &fakeHost{time: trackerapp.TimeSettings{Meetings: timeTicket("OPS-1")}}
	s := timeServer(f, h)
	answer, err := timeCall(t, s, "time_read", map[string]any{"from": "2026-11-24", "to": "2026-11-24"})
	if err != nil || !strings.HasSuffix(answer, `"meetings_ticket":"example.atlassian.net/OPS-1"}`) {
		t.Fatalf("time_read = %s, %v", answer, err)
	}
	var read struct {
		Meetings string `json:"meetings_ticket"`
	}
	if err := json.Unmarshal([]byte(answer), &read); err != nil {
		t.Fatal(err)
	}
	_, err = timeCall(t, s, "time_draft", map[string]any{"entries": []map[string]any{{"day": "2026-11-24", "key": read.Meetings, "hours": 1, "reason": "Standup"}}})
	refused(t, err, CodeNotFound, "No connection on this computer reaches example.atlassian.net; the sites on are other.atlassian.net.")
	if len(f.drafts) != 0 {
		t.Fatalf("the meetings ticket was drafted on the other site: %v", f.drafts)
	}
}
