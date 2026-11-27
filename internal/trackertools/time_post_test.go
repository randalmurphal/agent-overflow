package trackertools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

// openDay is Tue Nov 24's entries not yet logged on timeJira, by ID:
// drafted on WEB-1 and WEB-2, one that may already be logged on WEB-3,
// and a question.
func openDay() []trackerapp.TimeEntryView {
	return []trackerapp.TimeEntryView{
		{ID: 1, Revision: 1, Day: "2026-11-24", Issue: "10101", Key: "WEB-1", Title: "Checkout form", Minutes: 90, Reason: "Built the form", State: "drafted"},
		{ID: 2, Revision: 3, Day: "2026-11-24", Issue: "10102", Key: "WEB-2", Title: "Review", Minutes: 30, Reason: "Reviewed the pull request", State: "drafted", By: "person"},
		{ID: 3, Revision: 2, Day: "2026-11-24", Issue: "10103", Key: "WEB-3", Minutes: 60, Reason: "Triaged", State: "unknown"},
		{ID: 4, Revision: 1, Day: "2026-11-24", Minutes: 15, Reason: "Standup", State: "drafted"},
	}
}

// mayHave is what a post says of an entry that may already be logged.
const mayHave = "may already be logged, so it is not sent again; look at the day's logged time, and remove the entry if the time is there, or log the time by hand and then remove the entry"

// A post asks the person first, with every entry of its days: each under
// its ticket's key with its day, time and whole reason, an entry it will
// not send saying why, and the question in the notes. Approved, it posts
// exactly the entries shown at the revisions shown, answers each entry
// as the approval showed it, logged or not with why, and puts one row on
// the timeline that counts what the approval offered to log.
func TestAPostAsksThePersonAndPostsWhatTheySaw(t *testing.T) {
	t.Parallel()
	// The service answers by day and ID.
	week := append([]trackerapp.TimeEntryView{{ID: 5, Revision: 1, Day: "2026-11-23", Issue: "10101", Key: "WEB-1", Minutes: 60, Reason: "Planned the form", State: "drafted"}}, openDay()...)
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: week},
		post: func(_ string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
			// WEB-1 of Tue Nov 24 was edited since the card was raised, as
			// the service would answer it.
			return trackerapp.PostTimeView{Outcomes: []trackerapp.PostOutcome{
				{ID: 5, State: "posted"},
				{ID: 1, State: "not_posted", Why: "changed since it was read; read the day again"},
				{ID: 2, State: "not_posted", Why: "was refused: Worklog is closed for this period"},
				{ID: 3, State: "uncertain", Why: mayHave},
				{ID: 4, State: "not_posted", Why: "its ticket is not picked yet, so it is not posted"},
			}}, nil
		}}
	h := &fakeHost{}
	h.approve = func(Approval) error {
		if len(f.posts) != 0 {
			t.Error("the post was sent before the person answered")
		}
		return nil
	}
	answer, err := timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24", "2026-11-23"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.approvals) != 1 {
		t.Fatalf("asked %d approvals", len(h.approvals))
	}
	a := h.approvals[0]
	if a.Title != "Log time for Mon Nov 23 to Tue Nov 24" || a.Tool != "time_draft" || a.ID != "call-1" {
		t.Fatalf("the approval is %q by %q, id %q", a.Title, a.Tool, a.ID)
	}
	var shown []string
	for _, tk := range a.Tickets {
		for _, c := range tk.Changes {
			shown = append(shown, tk.Key+" "+tk.Title+" | "+c.Line)
		}
	}
	want := []string{
		"WEB-1  | Mon Nov 23, 1h: Planned the form",
		"WEB-1  | Tue Nov 24, 1h 30m: Built the form",
		"WEB-2 Review | Tue Nov 24, 30m: Reviewed the pull request",
		"WEB-3  | Tue Nov 24, 1h: Triaged (" + mayHave + ")",
	}
	if !slices.Equal(shown, want) || !slices.Equal(a.Notes, []string{"Tue Nov 24, 15m: Standup (its ticket is not picked yet, so it is not posted)"}) {
		t.Fatalf("the approval shows\n%s\nnotes %q\nwant\n%s", strings.Join(shown, "\n"), a.Notes, strings.Join(want, "\n"))
	}
	if want := `conn-jira {"entries":[{"id":5,"revision":1},{"id":1,"revision":1},{"id":2,"revision":3},{"id":3,"revision":2},{"id":4,"revision":1}]}`; len(f.posts) != 1 || f.posts[0] != want {
		t.Fatalf("posted %v\nwant [%s]", f.posts, want)
	}
	if want := `{"logged":[{"day":"2026-11-23","key":"WEB-1","hours":1}],"not_logged":[` +
		`{"why":"changed since it was read; read the day again","entries":[{"day":"2026-11-24","key":"WEB-1","hours":1.5}]},` +
		`{"why":"was refused: Worklog is closed for this period","entries":[{"day":"2026-11-24","key":"WEB-2","hours":0.5}]},` +
		`{"why":"its ticket is not picked yet, so it is not posted","entries":[{"day":"2026-11-24","hours":0.25}]}],` +
		`"may_be_logged":[{"why":"` + mayHave + `","entries":[{"day":"2026-11-24","key":"WEB-3","hours":1}]}]}`; answer != want {
		t.Fatalf("answered %s\nwant    %s", answer, want)
	}
	if len(h.postRows) != 1 || h.postRows[0].Title() != "Logged 1 of 3 entries for Mon Nov 23 to Tue Nov 24" ||
		!slices.Equal(h.postRows[0].Connections, []string{timeJira.ID}) || h.postRows[0].ID != "call-1" {
		t.Fatalf("the timeline rows are %+v", h.postRows)
	}
	if len(f.drafts) != 0 || len(h.timeRows) != 0 {
		t.Fatalf("a post drafted %v, rows %v", f.drafts, h.timeRows)
	}
}

// A denial, a wait that ended, a policy that no longer lets the thread
// log time once the person approved, and a call that names neither or
// both of entries and post: nothing is posted and no row is written, and
// each says why.
func TestAPostThatIsNotApprovedPostsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    map[string]any
		approve func(Approval) error
		// policy runs before each policy read with its count.
		policy func(h *fakeHost, n int)
		open   []trackerapp.TimeEntryView
		code   string
		want   string
	}{
		{"denied", map[string]any{"post": []string{"2026-11-24"}},
			func(Approval) error {
				return errorsx.Public("tracker_write_denied", "The user denied this change; nothing was sent.", nil)
			},
			nil, openDay(), "tracker_write_denied", "The user denied this change; nothing was sent."},
		{"not answered", map[string]any{"post": []string{"2026-11-24"}},
			func(Approval) error {
				return errorsx.Public("tracker_write_unanswered", "The user did not answer within 15 minutes; nothing was sent.", nil)
			},
			nil, openDay(), "tracker_write_unanswered", "The user did not answer within 15 minutes; nothing was sent."},
		{"made read-only while asked", map[string]any{"post": []string{"2026-11-24"}}, nil,
			func(h *fakeHost, n int) { h.readOnly = n == 2 }, openDay(), "tracker_read_only",
			"This thread is read-only, so it cannot change tickets or log time; ask the user to change its access."},
		{"read-only", map[string]any{"post": []string{"2026-11-24"}}, nil,
			func(h *fakeHost, _ int) { h.readOnly = true }, openDay(), "tracker_read_only",
			"This thread is read-only, so it cannot change tickets or log time; ask the user to change its access."},
		{"neither", map[string]any{}, nil, nil, openDay(), CodeInvalidRequest,
			"Give entries to draft time, or post to ask the user to log the draft of those days."},
		{"both", map[string]any{"post": []string{"2026-11-24"}, "entries": []map[string]any{{"day": "2026-11-24", "key": "WEB-1", "hours": 1, "reason": "x"}}},
			nil, nil, openDay(), CodeInvalidRequest, "Give entries or post, not both: a call that posts drafts nothing."},
		{"not a day", map[string]any{"post": []string{"Tuesday"}}, nil, nil, openDay(), CodeInvalidRequest, `post takes days as YYYY-MM-DD, not "Tuesday".`},
		{"a day twice", map[string]any{"post": []string{"2026-11-24", " 2026-11-24"}}, nil, nil, openDay(), CodeInvalidRequest, "post names 2026-11-24 twice."},
		{"eight days", map[string]any{"post": []string{"2026-11-17", "2026-11-18", "2026-11-19", "2026-11-20", "2026-11-21", "2026-11-22", "2026-11-23", "2026-11-24"}},
			nil, nil, openDay(), CodeInvalidRequest, "post names at most 7 days; post the rest in another call."},
		{"another day only", map[string]any{"post": []string{"2026-11-22"}}, nil, nil, openDay(), CodeInvalidRequest,
			"Sun Nov 22 has no entry not yet logged; time_read shows the draft."},
	} {
		f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: tc.open}}
		h := &fakeHost{approve: tc.approve}
		if tc.policy != nil {
			h.onPolicy = func(n int) { tc.policy(h, n) }
		}
		_, err := timeCall(t, timeServer(f, h), "time_draft", tc.args)
		if code, message, _ := errorsx.PublicDetails(err); code != tc.code || message != tc.want {
			t.Errorf("%s: %v\nwant [%s] %s", tc.name, err, tc.code, tc.want)
		}
		if len(f.posts)+len(h.postRows)+len(f.drafts) != 0 {
			t.Errorf("%s posted %v, rows %v, drafts %v", tc.name, f.posts, h.postRows, f.drafts)
		}
	}
}

// With two Jira connections, one approval shows both sites' entries, each
// key with its site; the post runs once per site, and the entries of a
// site whose post was refused before it sent are not logged, with the
// refusal, beside the ones logged. The row counts what was offered on
// both. When every site's post is refused, the answer names each entry
// with its refusal, and nothing is on the timeline.
func TestAPostOnTwoSitesAsksOnceAndAnswersEach(t *testing.T) {
	t.Parallel()
	other := []trackerapp.TimeEntryView{{ID: 9, Revision: 1, Day: "2026-11-24", Issue: "20101", Key: "OPS-1", Minutes: 60, Reason: "On call", State: "drafted"}}
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira, timeJira2},
		open: map[string][]trackerapp.TimeEntryView{timeJira.ID: openDay()[:2], timeJira2.ID: other},
		post: func(connection string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
			if connection == timeJira2.ID {
				return trackerapp.PostTimeView{}, errorsx.Public("tracker_disabled", "this connection is turned off", nil)
			}
			return postedAsRead(openDay(), in), nil
		}}
	h := &fakeHost{}
	answer, err := timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, tk := range h.approvals[0].Tickets {
		keys = append(keys, tk.Key)
	}
	if !slices.Equal(keys, []string{"example.atlassian.net/WEB-1", "example.atlassian.net/WEB-2", "other.atlassian.net/OPS-1"}) {
		t.Fatalf("the approval's keys are %v", keys)
	}
	want := `{"logged":[{"day":"2026-11-24","key":"example.atlassian.net/WEB-1","hours":1.5},{"day":"2026-11-24","key":"example.atlassian.net/WEB-2","hours":0.5}],` +
		`"not_logged":[{"why":"was not sent: other.atlassian.net: this connection is turned off","entries":[{"day":"2026-11-24","key":"other.atlassian.net/OPS-1","hours":1}]}]}`
	if answer != want || len(f.posts) != 2 {
		t.Fatalf("answered %s\nwant     %s\nposts %v", answer, want, f.posts)
	}
	if len(h.postRows) != 1 || h.postRows[0].Title() != "Logged 2 of 3 entries for Tue Nov 24" || !slices.Equal(h.postRows[0].Connections, []string{timeJira.ID}) {
		t.Fatalf("the timeline rows are %+v", h.postRows)
	}
	f.post = func(string, trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
		return trackerapp.PostTimeView{}, errorsx.Public("tracker_disabled", "this connection is turned off", nil)
	}
	h = &fakeHost{}
	answer, err = timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"logged":[],"not_logged":[{"why":"was not sent: example.atlassian.net: this connection is turned off","entries":[` +
		`{"day":"2026-11-24","key":"example.atlassian.net/WEB-1","hours":1.5},{"day":"2026-11-24","key":"example.atlassian.net/WEB-2","hours":0.5}]},` +
		`{"why":"was not sent: other.atlassian.net: this connection is turned off","entries":[{"day":"2026-11-24","key":"other.atlassian.net/OPS-1","hours":1}]}]}`
	if answer != want || len(h.postRows) != 0 {
		t.Fatalf("answered %s\nwant     %s\nrows %+v", answer, want, h.postRows)
	}
}

// Once approved, the post is sent whole though the call ends while the
// person answers: the post's context is not the call's.
func TestAnApprovedPostOutlivesTheCall(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: openDay()}}
	ctx, cancel := context.WithCancel(t.Context())
	h := &fakeHost{approve: func(Approval) error { cancel(); return nil }}
	if _, err := timeServer(f, h).Call(ctx, timeCaller, "time_draft", json.RawMessage(`{"post":["2026-11-24"]}`)); err != nil {
		t.Fatal(err)
	}
	if len(f.postCtx) != 1 || f.postCtx[0] != nil {
		t.Fatalf("the post began with %v", f.postCtx)
	}
}

// A post's approval past maxApprovalBytes is refused before it asks.
func TestAPostTooLargeToShowIsRefused(t *testing.T) {
	t.Parallel()
	var many []trackerapp.TimeEntryView
	for i := range 250 {
		many = append(many, trackerapp.TimeEntryView{ID: int64(i + 1), Revision: 1, Day: "2026-11-24", Issue: fmt.Sprint(10000 + i), Key: fmt.Sprintf("WEB-%d", i),
			Minutes: 5, Reason: strings.Repeat("x", 1100), State: "drafted"})
	}
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: many}}
	h := &fakeHost{}
	_, err := timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	refused(t, err, CodeInvalidRequest, "Tue Nov 24's entries hold more than 256 KiB, more than an approval shows the person; post fewer days in each call.")
	f.open[timeJira.ID] = append(many, trackerapp.TimeEntryView{ID: 999, Revision: 1, Day: "2026-11-24", Key: "WEB-999", Minutes: 5, Reason: "x", State: "drafted"})
	_, err = timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	refused(t, err, CodeInvalidRequest, "Tue Nov 24 holds 251 entries not yet logged, more than one post takes (250); post fewer days.")
	if len(h.approvals)+len(f.posts) != 0 {
		t.Fatalf("asked %d, posted %v", len(h.approvals), f.posts)
	}
}

// A Jira connection added and never verified holds no draft: it is not
// read and does not stop the post. One whose read is refused is named in
// the approval and the answer, and the others post; when every connection
// read is refused, that is the call's error.
func TestAPostReadsTheConnectionsThatCanHoldItsDays(t *testing.T) {
	t.Parallel()
	added := trackerapp.TicketSource{ID: "conn-new", Site: "new.atlassian.net", Kind: "jira", Email: "user1@example.com"}
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira, added, timeJira2}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: openDay()[:1]}}
	h := &fakeHost{}
	answer, err := timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"open time conn-jira 2026-11-24", "open time conn-jira-2 2026-11-24"}; !slices.Equal(f.asked, want) {
		t.Fatalf("asked %v; want %v", f.asked, want)
	}
	unread := "other.atlassian.net could not be read: this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings; its time is not in this post."
	if len(h.approvals) != 1 || !slices.Equal(h.approvals[0].Notes, []string{unread}) {
		t.Fatalf("the approvals are %+v; want the unread connection in the notes", h.approvals)
	}
	if want := `{"logged":[{"day":"2026-11-24","key":"example.atlassian.net/WEB-1","hours":1.5}],"unread":[{"site":"other.atlassian.net",` +
		`"error":"this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings"}]}`; answer != want {
		t.Fatalf("answered %s\nwant     %s", answer, want)
	}
	f = &fakeTracker{sources: []trackerapp.TicketSource{timeJira, added}}
	h = &fakeHost{}
	_, err = timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	refused(t, err, "tracker_connection_unverified", "Nothing was posted for Tue Nov 24: example.atlassian.net could not be read: this connection has no verified account, "+
		"so whose time it is is not known; check the connection in the Issues settings.")
	if len(h.approvals)+len(f.posts) != 0 {
		t.Fatalf("asked %d, posted %v", len(h.approvals), f.posts)
	}
}

// With nothing readable to post, the refusal names every connection that
// could not be read, by account where two share a site, and says when the
// others hold nothing; refusals of two kinds are the call's failure.
func TestARefusalOfAPostNamesEveryUnreadConnection(t *testing.T) {
	t.Parallel()
	work := timeJira
	work.Label, work.Email = "Work", "user1@example.com"
	second := trackerapp.TicketSource{ID: "conn-jira-b", Site: timeJira.Site, Kind: "jira", Account: "acc-user2", Email: "user2@example.com"}
	f := &fakeTracker{sources: []trackerapp.TicketSource{work, second}}
	_, err := timeCall(t, timeServer(f, &fakeHost{}), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	unverified := "could not be read: this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings"
	refused(t, err, "tracker_connection_unverified", "Nothing was posted for Tue Nov 24: example.atlassian.net (Work) "+unverified+
		"; example.atlassian.net (user2@example.com) "+unverified+".")

	// One unread beside one read whose day is empty.
	f = &fakeTracker{sources: []trackerapp.TicketSource{work, second}, open: map[string][]trackerapp.TimeEntryView{work.ID: {}}}
	_, err = timeCall(t, timeServer(f, &fakeHost{}), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	refused(t, err, "tracker_connection_unverified", "Nothing was posted for Tue Nov 24: example.atlassian.net (user2@example.com) "+unverified+
		". The other connections hold no entry not yet logged for those days.")

	// Two kinds of refusal.
	f = &fakeTracker{sources: []trackerapp.TicketSource{work, second}, openErr: map[string]error{
		second.ID: errorsx.Public("tracker_disabled", "this connection is turned off", nil)}}
	_, err = timeCall(t, timeServer(f, &fakeHost{}), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if code, message, _ := errorsx.PublicDetails(err); code != CodeFailed || !strings.Contains(message, "(user2@example.com) could not be read: this connection is turned off") {
		t.Fatalf("refused [%s] %s; want [%s] naming both", code, message, CodeFailed)
	}
}

// A day whose entries hold none a post sends (one that may already be
// logged, a question) is answered with each entry's reason, in the words
// the approval would show it, with nothing asked, posted or put on the
// timeline.
func TestADayWithNothingToSendSaysWhyEachIsNot(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: openDay()[2:4]}}
	h := &fakeHost{}
	answer, err := timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"logged":[],"not_logged":[{"why":"its ticket is not picked yet, so it is not posted","entries":[{"day":"2026-11-24","hours":0.25}]}],` +
		`"may_be_logged":[{"why":"` + mayHave + `","entries":[{"day":"2026-11-24","key":"WEB-3","hours":1}]}]}`
	if answer != want {
		t.Fatalf("answered %s\nwant     %s", answer, want)
	}
	if len(h.approvals)+len(f.posts)+len(h.postRows) != 0 {
		t.Fatalf("asked %d, posted %v, rows %+v", len(h.approvals), f.posts, h.postRows)
	}
}

// Two connections of one site are told apart on the card and in the
// answer by the name the person gave each in the Issues settings, or its
// account email when they gave none, and by both when they gave the two
// one name, so their entries of one ticket are two headings; each key
// stays as answers write it, so it opens that ticket again. A connection
// that could not be read is named the same way.
func TestTwoAccountsOfOneSiteAreToldApart(t *testing.T) {
	t.Parallel()
	named := timeJira
	named.Label, named.Email = "Work", "user1@example.com"
	second := trackerapp.TicketSource{ID: "conn-jira-b", Site: timeJira.Site, Kind: "jira", Account: "acc-user2", Email: "user2@example.com"}
	third := trackerapp.TicketSource{ID: "conn-jira-c", Site: timeJira.Site, Kind: "jira", Account: "acc-user3", Email: "user3@example.com"}
	mine := []trackerapp.TimeEntryView{{ID: 7, Revision: 1, Day: "2026-11-24", Issue: "10101", Key: "WEB-1", Minutes: 30, Reason: "Paired", State: "drafted"}}
	f := &fakeTracker{sources: []trackerapp.TicketSource{named, second, third, timeJira2}, open: map[string][]trackerapp.TimeEntryView{
		named.ID: openDay()[:1], second.ID: mine, timeJira2.ID: {}}}
	f.post = func(connection string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
		return postedAsRead(f.open[connection], in), nil
	}
	headings := func(a Approval) []string {
		var out []string
		for _, tk := range a.Tickets {
			out = append(out, tk.Key+" | "+tk.Account)
		}
		return out
	}
	h := &fakeHost{}
	answer, err := timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.atlassian.net/WEB-1 | Work", "example.atlassian.net/WEB-1 | user2@example.com"}; !slices.Equal(headings(h.approvals[0]), want) {
		t.Fatalf("the card's headings are %q; want %q", headings(h.approvals[0]), want)
	}
	unread := `{"site":"example.atlassian.net","account":"user3@example.com","error":"this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings"}`
	if want := `{"logged":[{"day":"2026-11-24","key":"example.atlassian.net/WEB-1","account":"Work","hours":1.5},` +
		`{"day":"2026-11-24","key":"example.atlassian.net/WEB-1","account":"user2@example.com","hours":0.5}],"unread":[` + unread + `]}`; answer != want {
		t.Fatalf("answered %s\nwant     %s", answer, want)
	}
	if want := "example.atlassian.net (user3@example.com) could not be read: "; len(h.approvals[0].Notes) != 1 || !strings.HasPrefix(h.approvals[0].Notes[0], want) {
		t.Fatalf("the card's notes are %q; want one starting %q", h.approvals[0].Notes, want)
	}
	// One name for two: each goes by its name and email.
	second.Label = "Work"
	f.sources = []trackerapp.TicketSource{named, second}
	h = &fakeHost{}
	answer, err = timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.atlassian.net/WEB-1 | Work · user1@example.com", "example.atlassian.net/WEB-1 | Work · user2@example.com"}; !slices.Equal(headings(h.approvals[0]), want) {
		t.Fatalf("the card's headings are %q; want %q", headings(h.approvals[0]), want)
	}
	if want := `{"logged":[{"day":"2026-11-24","key":"example.atlassian.net/WEB-1","account":"Work · user1@example.com","hours":1.5},` +
		`{"day":"2026-11-24","key":"example.atlassian.net/WEB-1","account":"Work · user2@example.com","hours":0.5}]}`; answer != want {
		t.Fatalf("answered %s\nwant     %s", answer, want)
	}
}

// No two connections of one site come out of postAccount with one name,
// however their names and emails collide, and a site with one connection
// names none.
func TestPostAccountNamesEachConnectionOfASiteApart(t *testing.T) {
	t.Parallel()
	conn := func(id, site, label, email string) trackerapp.TicketSource {
		return trackerapp.TicketSource{ID: id, Site: site, Kind: "jira", Label: label, Email: email}
	}
	for _, tc := range [][]trackerapp.TicketSource{
		{conn("a", "s", "Work", "a@x.com"), conn("b", "s", "", "b@x.com")},
		{conn("a", "s", "Work", "a@x.com"), conn("b", "s", "Work", "b@x.com")},
		{conn("a", "s", "", "a@x.com"), conn("b", "s", "", "b@x.com")},
		// A label that is another's email.
		{conn("a", "s", "b@x.com", "a@x.com"), conn("b", "s", "", "b@x.com")},
		// A label that is what a collision names another.
		{conn("a", "s", "Work", "a@x.com"), conn("b", "s", "Work", "b@x.com"), conn("c", "s", "Work · a@x.com", "c@x.com")},
		{conn("a", "s", "b@x.com", "a@x.com"), conn("b", "s", "", "b@x.com"), conn("c", "s", "a@x.com", "c@x.com")},
		{conn("a", "s", "Work", "a@x.com"), conn("b", "s", "Home", "b@x.com"), conn("c", "s", "Home · b@x.com", "c@x.com")},
	} {
		jira := append(tc, conn("o", "other", "Work", "a@x.com"))
		seen := map[string]string{}
		for _, src := range tc {
			name := postAccount(jira, src)
			if name == "" {
				t.Errorf("%+v: %s has no name", tc, src.ID)
			}
			if other, ok := seen[name]; ok {
				t.Errorf("%+v: %s and %s are both %q", tc, other, src.ID, name)
			}
			seen[name] = src.ID
		}
		if name := postAccount(jira, jira[len(jira)-1]); name != "" {
			t.Errorf("the one connection of its site is named %q", name)
		}
	}
}

// A post that failed after it sent answers what the service knows of each
// entry, an entry that may have been logged in a list of its own, says
// that this computer could not record the post, and puts its row on the
// timeline, counting the entries that may have been logged; the error is
// logged. A post whose read-back missed a day says which day shows the
// time after its next read.
func TestAPostThatFailedAfterSendingWritesItsRow(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: openDay()[:2]},
		post: func(string, trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
			return trackerapp.PostTimeView{Outcomes: []trackerapp.PostOutcome{{ID: 1, State: "posted"}, {ID: 2, State: "uncertain", Why: mayHave}}},
				fmt.Errorf("trackerapp: settle the posted entries: disk I/O error: %w", trackerapp.ErrPostUnrecorded)
		}}
	h := &fakeHost{}
	srv := timeServer(f, h)
	var logged []string
	srv.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	answer, err := timeCall(t, srv, "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"logged":[{"day":"2026-11-24","key":"WEB-1","hours":1.5}],"may_be_logged":[{"why":"` + mayHave + `","entries":[{"day":"2026-11-24","key":"WEB-2","hours":0.5}]}],` +
		`"warnings":[{"site":"example.atlassian.net","warning":"The time listed as logged is logged, but this computer could not record it ` +
		`(example.atlassian.net: trackerapp: settle the posted entries: disk I/O error: trackerapp: the post's outcomes are not stored); the Time screen shows these entries as may already be logged after the app restarts."}]}`; answer != want {
		t.Fatalf("answered %s\nwant     %s", answer, want)
	}
	if len(h.postRows) != 1 || h.postRows[0].Title() != "Logged 1 of 2 entries for Tue Nov 24; 1 may have been logged" || h.postRows[0].Uncertain != 1 {
		t.Fatalf("the timeline rows are %+v", h.postRows)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "disk I/O error") {
		t.Fatalf("logged %q", logged)
	}
	// An error after the outcomes were stored (the read of the entries
	// after them) changes nothing the answer says: it is logged alone.
	f.post = func(string, trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
		return trackerapp.PostTimeView{Outcomes: []trackerapp.PostOutcome{{ID: 1, State: "posted"}, {ID: 2, State: "posted"}}},
			fmt.Errorf("trackerapp: read the entries after the post: database is locked")
	}
	logged = nil
	answer, err = timeCall(t, srv, "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if want := `{"logged":[{"day":"2026-11-24","key":"WEB-1","hours":1.5},{"day":"2026-11-24","key":"WEB-2","hours":0.5}]}`; err != nil || answer != want {
		t.Fatalf("a post whose last read failed answered %s, %v\nwant %s", answer, err, want)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "database is locked") {
		t.Fatalf("logged %q", logged)
	}
	f.post = func(_ string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
		out := postedAsRead(openDay(), in)
		out.Stale = []trackerapp.StaleView{{Failure: trackerapp.FailureView{Message: "Jira did not answer in time."},
			Resources: []trackerapp.ResourceView{{Kind: "worklogs", Day: "2026-11-24"}}}}
		return out, nil
	}
	answer, err = timeCall(t, timeServer(f, &fakeHost{}), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"logged":[{"day":"2026-11-24","key":"WEB-1","hours":1.5},{"day":"2026-11-24","key":"WEB-2","hours":0.5}],` +
		`"warnings":[{"site":"example.atlassian.net","warning":"The posted time shows on Tue Nov 24 after the next read: Jira did not answer in time."}]}`; answer != want {
		t.Fatalf("answered %s\nwant     %s", answer, want)
	}
}

// Every entry of a post is in its answer, however many reasons it holds:
// the reasons share the budget, cut.
func TestAPostsAnswerNamesEveryEntryWithinItsBudget(t *testing.T) {
	t.Parallel()
	var many []trackerapp.TimeEntryView
	for i := range 250 {
		many = append(many, trackerapp.TimeEntryView{ID: int64(i + 1), Revision: 1, Day: "2026-11-24", Issue: fmt.Sprint(10000 + i), Key: fmt.Sprintf("WEB-%d", i),
			Minutes: 5, Reason: "x", State: "drafted"})
	}
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: many},
		post: func(_ string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
			var out trackerapp.PostTimeView
			for _, ref := range in.Entries {
				out.Outcomes = append(out.Outcomes, trackerapp.PostOutcome{ID: ref.ID, State: "not_posted", Why: fmt.Sprintf("was refused: %d %s", ref.ID, strings.Repeat("closed ", 150))})
			}
			return out, nil
		}}
	answer, err := timeCall(t, timeServer(f, &fakeHost{}), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	var got timePostAnswer
	if err := json.Unmarshal([]byte(answer), &got); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range got.NotLogged {
		n += len(g.Entries)
	}
	if len(answer) > answerBudget || n != 250 || len(got.NotLogged) != 250 {
		t.Fatalf("the answer is %d bytes naming %d entries in %d groups; want at most %d naming all 250", len(answer), n, len(got.NotLogged), answerBudget)
	}
	// Keys long enough that one answer cannot name every entry are
	// refused before the person is asked.
	for i := range many {
		many[i].Key = fmt.Sprintf("WEB-%d%s", i, strings.Repeat("9", 40))
	}
	h := &fakeHost{}
	_, err = timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	refused(t, err, CodeInvalidRequest, "Tue Nov 24 holds more entries not yet logged than one answer names; post fewer days in each call.")
	if len(h.approvals) != 0 {
		t.Fatalf("asked %d", len(h.approvals))
	}
}

// A post admitted with its answer at the edge of the budget still names
// every entry and its connection's warning: the admission reserves room
// for both.
func TestAPostsAnswerAtTheBudgetsEdgeKeepsItsWarning(t *testing.T) {
	t.Parallel()
	entries := func(pad int) []trackerapp.TimeEntryView {
		var out []trackerapp.TimeEntryView
		for i := range 250 {
			key := fmt.Sprintf("WEB-%03d%s", i, strings.Repeat("9", pad/250))
			if i < pad%250 {
				key += "9"
			}
			out = append(out, trackerapp.TimeEntryView{ID: int64(i + 1), Revision: 1, Day: "2026-11-24", Issue: fmt.Sprint(10000 + i), Key: key,
				Minutes: 5, Reason: "x", State: "drafted"})
		}
		return out
	}
	// The longest keys the admission takes.
	pad := 0
	for postAnswerMost([]timePostSite{{src: timeJira, key: qualifier(nil, timeJira.Site), entries: entries(pad + 1)}}, nil) <= answerBudget {
		pad++
	}
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: entries(pad)},
		post: func(_ string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
			var out trackerapp.PostTimeView
			for i, ref := range in.Entries {
				o := trackerapp.PostOutcome{ID: ref.ID, State: "not_posted", Why: fmt.Sprintf("was refused: %d %s", ref.ID, strings.Repeat("closed ", 150))}
				if i%2 == 0 {
					o.State = "uncertain"
				}
				out.Outcomes = append(out.Outcomes, o)
			}
			return out, fmt.Errorf("trackerapp: settle the posted entries: %s%w", strings.Repeat("disk I/O error ", 100), trackerapp.ErrPostUnrecorded)
		}}
	answer, err := timeCall(t, timeServer(f, &fakeHost{}), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	if err != nil {
		t.Fatal(err)
	}
	var got timePostAnswer
	if err := json.Unmarshal([]byte(answer), &got); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range append(got.NotLogged, got.MayBeLogged...) {
		n += len(g.Entries)
	}
	if len(answer) > answerBudget || n != 250 || len(got.Warnings) != 1 {
		t.Fatalf("the answer is %d bytes naming %d entries and %d warnings; want at most %d naming all 250 and the warning", len(answer), n, len(got.Warnings), answerBudget)
	}
}
