package trackertools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	if !slices.Equal(names, []string{"ticket_find", "ticket_read", "ticket_create", "ticket_update", "time_read", "time_draft"}) || !slices.Equal(names, ToolNames(true)) {
		t.Fatalf("with the time tools = %v", names)
	}
	if got := len(s.Tools(false)); got != 4 {
		t.Fatalf("without the time tools %d tools are listed", got)
	}
	sizes := map[string]int{}
	for _, tool := range s.Tools(true)[4:] {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		sizes[tool["name"].(string)] = len(raw)
	}
	t.Logf("time_read %d bytes, time_draft %d bytes", sizes["time_read"], sizes["time_draft"])
	if sizes["time_read"] != 268 || sizes["time_draft"] != 611 {
		t.Errorf("the time tools are %v bytes; pinned time_read 268, time_draft 611", sizes)
	}
	_, err := s.Call(t.Context(), testCaller, "time_draft", json.RawMessage(`{}`))
	refused(t, err, CodeInvalidRequest, `There is no tool named "time_draft". The tracker tools are: ticket_find, ticket_read, ticket_create, ticket_update.`)
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
		{"no entries", []trackerapp.TicketSource{timeJira}, false, []map[string]any{}, CodeInvalidRequest, "time_draft takes 1 to 250 entries a call."},
		{"read-only", []trackerapp.TicketSource{timeJira}, true, []map[string]any{entry("WEB-1")}, "tracker_read_only",
			"This thread is read-only, so it cannot change tickets; ask the user to change its access."},
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
