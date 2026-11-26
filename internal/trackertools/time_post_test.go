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

// A post asks the person first, with every entry of its days: each under
// its ticket's key with its day, time and whole reason, an entry it will
// not send saying why, and the question in the notes. Approved, it posts
// exactly the entries shown at the revisions shown, answers what was
// posted and the sentence of what was not, and puts one row on the
// timeline that counts what the approval offered to log.
func TestAPostAsksThePersonAndPostsWhatTheySaw(t *testing.T) {
	t.Parallel()
	// The service answers by day and ID.
	week := append([]trackerapp.TimeEntryView{{ID: 5, Revision: 1, Day: "2026-11-23", Issue: "10101", Key: "WEB-1", Minutes: 60, Reason: "Planned the form", State: "drafted"}}, openDay()...)
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira}, open: map[string][]trackerapp.TimeEntryView{timeJira.ID: week},
		post: func(string, trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
			return trackerapp.PostTimeView{Posted: 2, Of: 4, Sentence: "Posted 2 of 4 entries for Mon Nov 23 to Tue Nov 24. WEB-2, 30m, was refused: Worklog is closed for this period."}, nil
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
		"WEB-3  | Tue Nov 24, 1h: Triaged (not sent: it may already be logged)",
	}
	if !slices.Equal(shown, want) || !slices.Equal(a.Notes, []string{"Tue Nov 24, 15m: Standup (not posted: its ticket is not picked yet)"}) {
		t.Fatalf("the approval shows\n%s\nnotes %q\nwant\n%s", strings.Join(shown, "\n"), a.Notes, strings.Join(want, "\n"))
	}
	if want := `conn-jira {"entries":[{"id":5,"revision":1},{"id":1,"revision":1},{"id":2,"revision":3},{"id":3,"revision":2},{"id":4,"revision":1}]}`; len(f.posts) != 1 || f.posts[0] != want {
		t.Fatalf("posted %v\nwant [%s]", f.posts, want)
	}
	if want := `{"posts":[{"posted":2,"of":3,"sentence":"Posted 2 of 4 entries for Mon Nov 23 to Tue Nov 24. WEB-2, 30m, was refused: Worklog is closed for this period."}]}`; answer != want {
		t.Fatalf("answered %s\nwant    %s", answer, want)
	}
	if len(h.postRows) != 1 || h.postRows[0].Title() != "Logged 2 of 3 entries for Mon Nov 23 to Tue Nov 24" ||
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
		{"nothing drafted", map[string]any{"post": []string{"2026-11-24"}}, nil, nil, openDay()[2:4], CodeInvalidRequest,
			"Tue Nov 24 has no drafted entry on a ticket to log; time_read shows the draft."},
		{"another day only", map[string]any{"post": []string{"2026-11-22"}}, nil, nil, openDay(), CodeInvalidRequest,
			"Sun Nov 22 has no drafted entry on a ticket to log; time_read shows the draft."},
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
// key with its site; the post runs once per site, the answer says each
// site's, and a site whose post failed is said in words beside the one
// that posted. The row counts what was offered on both.
func TestAPostOnTwoSitesAsksOnceAndAnswersEach(t *testing.T) {
	t.Parallel()
	other := []trackerapp.TimeEntryView{{ID: 9, Revision: 1, Day: "2026-11-24", Issue: "20101", Key: "OPS-1", Minutes: 60, Reason: "On call", State: "drafted"}}
	f := &fakeTracker{sources: []trackerapp.TicketSource{timeJira, timeJira2},
		open: map[string][]trackerapp.TimeEntryView{timeJira.ID: openDay()[:2], timeJira2.ID: other},
		post: func(connection string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
			if connection == timeJira2.ID {
				return trackerapp.PostTimeView{}, errorsx.Public("tracker_disabled", "this connection is turned off", nil)
			}
			return trackerapp.PostTimeView{Posted: 2, Of: 2, Sentence: "Posted 2 of 2 entries for Tue Nov 24."}, nil
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
	want := `{"posts":[{"site":"example.atlassian.net","posted":2,"of":2,"sentence":"Posted 2 of 2 entries for Tue Nov 24."},` +
		`{"site":"other.atlassian.net","posted":0,"of":1,"sentence":"other.atlassian.net: this connection is turned off"}]}`
	if answer != want || len(f.posts) != 2 {
		t.Fatalf("answered %s\nwant     %s\nposts %v", answer, want, f.posts)
	}
	if len(h.postRows) != 1 || h.postRows[0].Title() != "Logged 2 of 3 entries for Tue Nov 24" || !slices.Equal(h.postRows[0].Connections, []string{timeJira.ID}) {
		t.Fatalf("the timeline rows are %+v", h.postRows)
	}
	// Every site failing is the call's error, and nothing is on the timeline.
	f.post = func(string, trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
		return trackerapp.PostTimeView{}, errorsx.Public("tracker_disabled", "this connection is turned off", nil)
	}
	h = &fakeHost{}
	_, err = timeCall(t, timeServer(f, h), "time_draft", map[string]any{"post": []string{"2026-11-24"}})
	refused(t, err, "tracker_disabled", "example.atlassian.net: this connection is turned off")
	if len(h.postRows) != 0 {
		t.Fatalf("a post that never ran wrote %+v", h.postRows)
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
	refused(t, err, CodeInvalidRequest, "Tue Nov 24 holds 251 entries not yet logged on example.atlassian.net, more than one post takes (250); post fewer days.")
	if len(h.approvals)+len(f.posts) != 0 {
		t.Fatalf("asked %d, posted %v", len(h.approvals), f.posts)
	}
}
