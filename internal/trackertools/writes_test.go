package trackertools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
)

// writeFixture is one server over a fake tracker and host, so the read
// marks of one session last across its calls.
type writeFixture struct {
	f   *fakeTracker
	h   *fakeHost
	srv *Server
}

// writableDetail is WEB-12 as a write reads it: in progress, with its
// moves, a priority field, a long text field, and its row on board 1.
func writableDetail(key, issue string) trackerapp.DetailView {
	d := detail(key)
	d.Card.ID, d.Card.Updated = issue, 1_000
	d.Description = "The login page fails."
	d.Moves = []trackerapp.MoveView{
		{ID: "11", Name: "Start", To: trackerapp.StatusView{ID: "1", Name: "To Do"}},
		{ID: "21", Name: "Review", To: trackerapp.StatusView{ID: "4", Name: "In Review"}},
		{ID: "31", Name: "Finish", To: trackerapp.StatusView{ID: "5", Name: "Done"}},
	}
	d.Fields = []trackerapp.FieldView{
		{ID: "customfield_10016", Name: "Story points", Kind: "number", Editable: true},
		{ID: "customfield_10020", Name: "Notes", Kind: "rich_text", Editable: true, Value: trackerapp.FieldValueView{Kind: "rich_text", Texts: []string{"Some notes."}}},
		{ID: "components", Name: "Component", Kind: "option", List: true, Editable: true},
		{ID: "creator", Name: "Creator", Kind: "user"},
		{ID: "customfield_10030", Name: "Due at", Kind: tracker.KindDateTime.String(), Editable: true},
	}
	points := 3.0
	d.Rows = []trackerapp.TicketRowView{{Board: "1", Estimate: &points, Unit: "points"}}
	return d
}

func newWriteFixture(t *testing.T) *writeFixture {
	t.Helper()
	src := jiraSource
	src.Kind, src.Account = "jira", "acc-user1"
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{src},
		lookup:  map[string]map[string]string{src.ID: {"WEB-12": "10012", "WEB-18": "10018", "WEB-27": "10027"}},
		details: map[string]trackerapp.DetailView{},
		boards: map[string]trackerapp.BoardSummary{src.ID + "/1": {Name: "Web Team", Project: "10000", PlansSprints: true, Unit: "points",
			EstimateField: "customfield_10016", Sprints: []trackerapp.SprintView{
				{ID: "13", Name: "Sprint 13", State: "closed"}, {ID: "14", Name: "Sprint 14", State: "active"}, {ID: "15", Name: "Sprint 15", State: "future"},
			}}},
		cards: map[string]trackerapp.CardView{},
	}
	for key, issue := range f.lookup[src.ID] {
		f.details[src.ID+"/"+issue] = writableDetail(key, issue)
		f.cards[src.ID+"/"+issue] = trackerapp.CardView{ID: issue, Key: key, Updated: 2_000}
	}
	h := &fakeHost{createBoards: map[string][]CreateBoard{"WEB": {{Connection: src.ID, Board: "1", Thread: true}}}}
	srv := New(f, h)
	srv.newID = func() string { return "call-1" }
	return &writeFixture{f: f, h: h, srv: srv}
}

// call runs one tool as session, answering its JSON or its error.
func (w *writeFixture) call(t *testing.T, session, name string, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.srv.Call(t.Context(), Caller{Thread: "thread-1", Session: session}, name, raw)
	if err != nil {
		return "", err
	}
	answer, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(answer), nil
}

func (w *writeFixture) update(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	return w.call(t, "session-1", "ticket_update", args)
}

// One ticket's status and comment are sent in order, status first, as the
// agent's writes, and the call's row names the ticket; the answer names
// each step by the name the call gave.
func TestUpdateAppliesStatusThenComment(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	answer, err := w.update(t, map[string]any{"keys": []string{"web-12"}, "set": map[string]any{"Status": "in review"}, "comment": "Ready for review."})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"tickets":[{"key":"WEB-12","applied":["Status","comment"]}]}`; answer != want {
		t.Fatalf("answer = %s\nwant %s", answer, want)
	}
	want := []string{
		`move conn-jira {"issue":"10012","move":"21"}`,
		`comment conn-jira {"issue":"10012","markdown":"Ready for review."}`,
	}
	if !slices.Equal(w.f.writes, want) {
		t.Fatalf("writes %q\nwant %q", w.f.writes, want)
	}
	for _, ctx := range w.f.agents {
		if ctx.Err() != nil || ctx.Done() != nil {
			t.Errorf("a write runs on a context the call's end cancels")
		}
	}
	if len(w.h.approvals) != 0 {
		t.Fatalf("one ticket asked for approval: %+v", w.h.approvals)
	}
	if len(w.h.rows) != 1 || w.h.rows[0].ID != "call-1" || w.h.rows[0].Tickets[0].Key != "WEB-12" || w.h.rows[0].Tickets[0].Issue != "10012" ||
		!slices.Equal(w.h.rows[0].Tickets[0].Applied, []string{"Status", "comment"}) {
		t.Fatalf("rows %+v", w.h.rows)
	}
	// The same call again: the ticket is in review, so the status is
	// already there, with no request and no row.
	d := w.f.details["conn-jira/10012"]
	d.Card.Status = trackerapp.StatusView{ID: "4", Name: "In Review"}
	w.f.details["conn-jira/10012"] = d
	w.f.writes = nil
	if answer, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"status": "In Review"}}); err != nil ||
		answer != `{"tickets":[{"key":"WEB-12","already":["status"]}]}` || len(w.f.writes) != 0 || len(w.h.rows) != 1 {
		t.Fatalf("a status the ticket has = %s, %v, writes %q, rows %+v", answer, err, w.f.writes, w.h.rows)
	}
}

// A call naming several tickets waits for the person: a denial sends
// nothing, an approval shows each ticket's changes and sends them all,
// the moves into one sprint as one write.
func TestUpdateOfSeveralTicketsWaitsForApproval(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	denied := errorsx.Public("tracker_write_denied", "The user denied this change; nothing was sent.", nil)
	w.h.approve = func(Approval) error { return denied }
	args := map[string]any{"keys": []string{"WEB-12", "WEB-18", "WEB-27"}, "set": map[string]any{"sprint": "Sprint 15"}}
	_, err := w.update(t, args)
	refused(t, err, "tracker_write_denied", "The user denied this change; nothing was sent.")
	if len(w.f.writes) != 0 || len(w.h.rows) != 0 {
		t.Fatalf("a denied call wrote %q", w.f.writes)
	}
	a := w.h.approvals[0]
	if a.ID != "call-1" || len(a.Tickets) != 3 || a.Tickets[0].Key != "WEB-12" || a.Tickets[0].Title != "Login fails on Safari" ||
		!slices.Equal(a.Tickets[0].Changes, []ApprovalChange{{Line: "Sprint → Sprint 15"}}) {
		t.Fatalf("approval %+v", a)
	}
	w.h.approve = nil
	answer, err := w.update(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"tickets":[{"key":"WEB-12","applied":["sprint"]},{"key":"WEB-18","applied":["sprint"]},{"key":"WEB-27","applied":["sprint"]}]}`; answer != want {
		t.Fatalf("answer = %s\nwant %s", answer, want)
	}
	if want := []string{`sprint_move conn-jira {"board":"1","sprint":"15","issues":["10012","10018","10027"]}`}; !slices.Equal(w.f.writes, want) {
		t.Fatalf("writes %q", w.f.writes)
	}
}

// A status the ticket cannot move to, or a sprint that is not open, is
// that ticket's refusal, and nothing is sent for any ticket.
func TestUpdateRefusedByTheTicketSendsNothing(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	answer, err := w.update(t, map[string]any{"keys": []string{"WEB-12", "WEB-18"}, "set": map[string]any{"status": "Shipped"}, "comment": "Done."})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tickets":[{"key":"WEB-12","refused":"status: WEB-12 cannot move to \"Shipped\"; it can move to \"To Do\", \"In Review\", or \"Done\".","not_sent":["comment"]},` +
		`{"key":"WEB-18","refused":"status: WEB-18 cannot move to \"Shipped\"; it can move to \"To Do\", \"In Review\", or \"Done\".","not_sent":["comment"]}]}`
	if answer != want {
		t.Fatalf("answer = %s\nwant %s", answer, want)
	}
	answer, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"sprint": "Sprint 99"}})
	if err != nil || answer != `{"tickets":[{"key":"WEB-12","refused":"sprint: board \"Web Team\" has no open sprint called \"Sprint 99\"; its open sprints are \"Sprint 14\" and \"Sprint 15\"."}]}` {
		t.Fatalf("a sprint not open = %s, %v", answer, err)
	}
	if len(w.f.writes)+len(w.h.approvals) != 0 {
		t.Fatalf("refused calls wrote %q, asked %+v", w.f.writes, w.h.approvals)
	}
}

// Field names match without regard to case; a name no field has is
// refused naming the ticket's fields, and so is a field no one can edit.
// The estimate takes the form a read prints, and answers to the name of
// the field its board estimates by. A sprint of null is the backlog.
func TestUpdateNamesAndValues(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	_, err := w.update(t, map[string]any{"set": map[string]any{"Severity": "High"}, "keys": []string{"WEB-12"}})
	refused(t, err, CodeInvalidRequest, `WEB-12 has no field "Severity"; it has status, sprint, estimate, title, assignee, priority, labels, parent, description, "Story points", "Notes", "Component", and "Due at".`)
	_, err = w.update(t, map[string]any{"set": map[string]any{"creator": "User 2"}, "keys": []string{"WEB-12"}})
	refused(t, err, CodeInvalidRequest, `WEB-12's "Creator" cannot be changed here.`)
	_, err = w.update(t, map[string]any{"set": map[string]any{"created": "2026-01-01"}, "keys": []string{"WEB-12"}})
	refused(t, err, CodeInvalidRequest, `"created" is the tracker's own; no write sets it.`)

	d := w.f.details["conn-jira/10012"]
	d.Card.Sprints = []string{"14"}
	w.f.details["conn-jira/10012"] = d
	answer, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{
		"PRIORITY": "High", "component": []string{"Login", "Web"}, "assignee": "me", "estimate": "5 points", "sprint": nil,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"tickets":[{"key":"WEB-12","applied":["PRIORITY","assignee","component","estimate","sprint"]}]}`; answer != want {
		t.Fatalf("answer = %s\nwant %s", answer, want)
	}
	want := []string{
		`edit conn-jira {"issue":"10012","fields":[{"field":"priority","typed":["High"]},{"field":"assignee","value":{"kind":"user","choices":[{"id":"acc-user1"}]}},{"field":"components","typed":["Login","Web"]}]}`,
		`estimate conn-jira {"board":"1","issue":"10012","value":5}`,
		`sprint_move conn-jira {"board":"1","issues":["10012"]}`,
	}
	if !slices.Equal(w.f.writes, want) {
		t.Fatalf("writes %q\nwant %q", w.f.writes, want)
	}
	w.f.writes = nil
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"story POINTS": 8}}); err != nil ||
		!slices.Equal(w.f.writes, []string{`estimate conn-jira {"board":"1","issue":"10012","value":8}`}) {
		t.Fatalf("the board's estimate field by name: %v, writes %q", err, w.f.writes)
	}
	_, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"estimate": "two"}})
	refused(t, err, CodeInvalidRequest, `Board "Web Team" estimates in points; give a number, such as 5 or "5 points".`)
	// A parent and a date-time as a read prints them, and a time estimate.
	w.f.writes = nil
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"parent": "WEB-4 Login page", "due at": "2026-12-01T09:30Z"}}); err != nil ||
		!slices.Equal(w.f.writes, []string{`edit conn-jira {"issue":"10012","fields":[{"field":"customfield_10030","typed":["2026-12-01T09:30:00Z"]},{"field":"parent","typed":["WEB-4"]}]}`}) {
		t.Fatalf("a printed parent and date-time: %v, writes %q", err, w.f.writes)
	}
	b := w.f.boards["conn-jira/1"]
	b.Unit = "time"
	w.f.boards["conn-jira/1"] = b
	d.Rows[0].Unit = "time"
	w.f.details["conn-jira/10012"] = d
	w.f.writes = nil
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"estimate": "1h 30m"}}); err != nil ||
		!slices.Equal(w.f.writes, []string{`estimate conn-jira {"board":"1","issue":"10012","value":5400}`}) {
		t.Fatalf("a time estimate as a read prints it: %v, writes %q", err, w.f.writes)
	}
	_, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"estimate": "5 points"}})
	refused(t, err, CodeInvalidRequest, `Board "Web Team" estimates in time; give it as 45m, 2h or 1h 30m.`)
	_, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"title": nil}})
	refused(t, err, CodeInvalidRequest, "The title cannot be cleared; give one.")
	_, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}})
	refused(t, err, CodeInvalidRequest, "Give set or comment.")
}

// A long text is changed only from a read this session made of the
// ticket as it is now that gave it all of the text: never read, changed
// since, or read in part (a description longer than the first read gives)
// is refused, and reading the rest through more lets the edit through.
// Another session's read is not this one's.
func TestUpdateOfALongTextNeedsItWhole(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	edit := map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"description": "The login page fails on Safari 17."}}
	_, err := w.update(t, edit)
	refused(t, err, CodeInvalidRequest, "Read WEB-12 before you change its description.")

	read := func(session string, args map[string]any) readAnswer {
		t.Helper()
		answer, err := w.call(t, session, "ticket_read", args)
		if err != nil {
			t.Fatal(err)
		}
		return decodeAs[readAnswer](t, answer)
	}
	read("session-1", map[string]any{"key": "WEB-12"})
	if _, err := w.update(t, edit); err != nil {
		t.Fatalf("an edit after a whole read: %v", err)
	}
	if !strings.Contains(w.f.writes[0], `"markdown":{"markdown":"The login page fails on Safari 17.","from":"The login page fails."}`) {
		t.Fatalf("the edit %q", w.f.writes)
	}
	_, err = w.call(t, "session-2", "ticket_update", edit)
	refused(t, err, CodeInvalidRequest, "Read WEB-12 before you change its description.")

	// Another person changes the ticket after this session's read.
	d := w.f.details["conn-jira/10018"]
	read("session-1", map[string]any{"key": "WEB-18"})
	d.Card.Updated = 5_000
	w.f.details["conn-jira/10018"] = d
	_, err = w.update(t, map[string]any{"keys": []string{"WEB-18"}, "set": map[string]any{"Notes": "New notes."}})
	refused(t, err, CodeInvalidRequest, `WEB-18 changed since you read it; read it again before you change its "Notes".`)

	// A description longer than the first read gives.
	d = w.f.details["conn-jira/10027"]
	d.Description = strings.Repeat("A long description. ", 3_000)
	w.f.details["conn-jira/10027"] = d
	first := read("session-1", map[string]any{"key": "WEB-27"})
	more, ok := first.More[fmt.Sprintf("the rest of the description (%s)", sizeLabel(len(d.Description)-len(strings.TrimSuffix(first.Description, cut))))]
	if !ok {
		t.Fatalf("the first read gave the description whole: more %v", first.More)
	}
	long := map[string]any{"keys": []string{"WEB-27"}, "set": map[string]any{"description": "Replaced."}}
	_, err = w.update(t, long)
	refused(t, err, CodeInvalidRequest, "You have read only part of WEB-27's description; read the rest of it through more before you change it.")
	for more != "" {
		next := read("session-1", map[string]any{"more": more})
		more = ""
		for label, value := range next.More {
			if strings.HasPrefix(label, "the rest of the description") {
				more = value
			}
		}
	}
	if _, err := w.update(t, long); err != nil {
		t.Fatalf("an edit after reading the rest: %v", err)
	}
}

// The first refused or unknown step stops its ticket: what came after it
// is not sent, and an unknown says the same call again makes only what
// was not.
func TestUpdateStopsAtTheFirstStepNotMade(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		switch name {
		case "edit":
			return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: "edit", State: "refused", Refusal: &trackerapp.RefusalView{
				Reason: "no_match", Message: `jira: no Priority value matches "Urgent"; candidates: Highest (1), High (2)`,
				Candidates: []trackerapp.ChoiceView{{ID: "1", Name: "Highest"}, {ID: "2", Name: "High"}, {ID: "3", Name: "Medium"}},
			}}}}, nil
		case "move":
			return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: "move", State: "unknown"}}}, nil
		}
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "applied"}}}, nil
	}
	answer, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"priority": "Urgent"}, "comment": "Raised."})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"tickets":[{"key":"WEB-12","refused":"priority: no value matches; it takes \"Highest\", \"High\", or \"Medium\".","not_sent":["comment"]}]}`; answer != want {
		t.Fatalf("answer = %s\nwant %s", answer, want)
	}
	answer, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"status": "Done"}, "comment": "Done."})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"tickets":[{"key":"WEB-12","unknown":"status: Jira's answer never came, so the move may or may not have been made; read the ticket to see before you call again.","not_sent":["comment"]}]}`; answer != want {
		t.Fatalf("answer = %s\nwant %s", answer, want)
	}
	if len(w.h.rows) != 0 {
		t.Fatalf("a call that applied nothing wrote a row: %+v", w.h.rows)
	}
}

// A comment alone reads the ticket's ID, key and title from its lookup,
// with no detail read. A comment an earlier call of the thread added is
// already, with no row; one an earlier call is still sending, or whose
// earlier attempt could not be looked for, is refused, and the call says
// why (trackerapp's repeat record).
func TestUpdateDoesNotAddACommentTwice(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	args := map[string]any{"keys": []string{"WEB-12"}, "comment": "Ready."}
	if answer, err := w.update(t, args); err != nil || answer != `{"tickets":[{"key":"WEB-12","applied":["comment"]}]}` {
		t.Fatalf("a comment = %s, %v", answer, err)
	}
	if want := []string{"lookup conn-jira WEB-12"}; !slices.Equal(w.f.asked, want) {
		t.Fatalf("a comment alone asked %q\nwant %q", w.f.asked, want)
	}
	if len(w.h.rows) != 1 || w.h.rows[0].Tickets[0].Title != "Login fails on Safari" {
		t.Fatalf("rows %+v", w.h.rows)
	}
	w.h.rows = nil
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{}, Repeat: &trackerapp.RepeatView{Issue: "10012", Comment: "c-1"}}, nil
	}
	answer, err := w.update(t, args)
	if err != nil || answer != `{"tickets":[{"key":"WEB-12","already":["comment"],"note":"Your earlier call added this comment; it was not added again."}]}` || len(w.h.rows) != 0 {
		t.Fatalf("a repeat of an added comment = %s, %v, rows %+v", answer, err, w.h.rows)
	}
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{}, fmt.Errorf("the write: %w", trackerapp.ErrAgentWriteInFlight)
	}
	answer, err = w.update(t, args)
	if want := `{"tickets":[{"key":"WEB-12","refused":"comment: the same comment is still being sent by an earlier call, so it was not sent again; call again once that call has answered."}]}`; err != nil || answer != want {
		t.Fatalf("a comment in flight = %s, %v\nwant %s", answer, err, want)
	}
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{}, fmt.Errorf("%w: down", trackerapp.ErrAgentRepeatUnchecked)
	}
	answer, err = w.update(t, args)
	if want := `{"tickets":[{"key":"WEB-12","refused":"comment: an earlier call's comment may have been added, and the ticket's comments could not be read to check, so it was not sent again; call again to check again."}]}`; err != nil || answer != want {
		t.Fatalf("a comment whose earlier attempt was not checked = %s, %v\nwant %s", answer, err, want)
	}
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{}, fmt.Errorf("%w: %w", trackerapp.ErrAgentRepeatUnchecked, trackerapp.ErrAgentAccountUnknown)
	}
	answer, err = w.update(t, args)
	if want := `{"tickets":[{"key":"WEB-12","refused":"comment: an earlier call's comment may have been added, and it cannot be checked: the connection's account was not known when it was sent. It was not sent again, and a call again within an hour of that attempt is refused the same way; read the ticket to see whether the comment is there."}]}`; err != nil || answer != want {
		t.Fatalf("a comment of an unknown account = %s, %v\nwant %s", answer, err, want)
	}
	if len(w.h.rows) != 0 {
		t.Fatalf("calls that added nothing wrote rows %+v", w.h.rows)
	}
}

// A read-only thread changes nothing and reads nothing.
func TestUpdateFromAReadOnlyThread(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.h.readOnly = true
	_, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "comment": "Hi."})
	refused(t, err, "tracker_read_only", "This thread is read-only, so it cannot change tickets; ask the user to change its access.")
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New"})
	refused(t, err, "tracker_read_only", "This thread is read-only, so it cannot change tickets; ask the user to change its access.")
	if len(w.f.asked)+len(w.f.writes) != 0 {
		t.Fatalf("a read-only call asked %q, wrote %q", w.f.asked, w.f.writes)
	}
}

// A key the mirrors hold resolves with no request; a bare key whose
// project two sites' mirrors hold is refused with none; a key whose
// project one site's mirror holds is looked up on that site alone.
func TestKeysResolveThroughTheMirrorsFirst(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	linear := linearSource
	linear.Kind = "linear"
	w.f.sources = append(w.f.sources, linear)
	w.f.keyed = map[string][]trackerapp.KeyedTicket{
		"WEB-12": {{Connection: "conn-jira", Issue: "10012"}},
		"WEB-18": {{Connection: "conn-jira", Issue: "10018"}, {Connection: "conn-linear"}},
		"WEB-27": {{Connection: "conn-jira"}},
	}
	if _, err := w.call(t, "session-1", "ticket_read", map[string]any{"key": "WEB-12"}); err != nil {
		t.Fatal(err)
	}
	_, err := w.call(t, "session-1", "ticket_read", map[string]any{"key": "WEB-18"})
	refused(t, err, CodeAmbiguous, "WEB-18 is on example.atlassian.net and example; name one: example.atlassian.net/WEB-18 or example/WEB-18.")
	if _, err := w.call(t, "session-1", "ticket_read", map[string]any{"key": "WEB-27"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"detail conn-jira 10012", "lookup conn-jira WEB-27", "detail conn-jira 10027"}; !slices.Equal(w.f.asked, want) {
		t.Fatalf("asked %q\nwant %q", w.f.asked, want)
	}
}

// A create goes through a board of its project here; the answer is its
// key, and a field refused before the create leaves nothing made. A
// status after the create that is refused says the ticket exists.
func TestCreate(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10001", Name: "Story"}, {ID: "10002", Name: "Task"}, {ID: "10003", Name: "Bug"}}
	w.f.fields = []trackerapp.FieldView{{ID: "components", Name: "Component", Kind: "option", List: true, Editable: true}}
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		if name == "create" {
			return trackerapp.WriteView{Parts: []trackerapp.PartView{{Issue: "10042", Step: "create", State: "applied"}}, Created: "10042", CreatedKey: "WEB-42"}, nil
		}
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "applied"}}}, nil
	}
	w.f.details["conn-jira/10042"] = writableDetail("WEB-42", "10042")
	answer, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "web", "title": "Login fails", "type": "bug",
		"description": "Steps.", "set": map[string]any{"Component": "Login", "status": "In Review"}})
	if err != nil {
		t.Fatal(err)
	}
	if answer != `{"key":"WEB-42","applied":["status"]}` {
		t.Fatalf("answer = %s", answer)
	}
	want := []string{
		`create conn-jira {"board":"1","project":"10000","type":"10003","title":"Login fails","fields":[{"field":"components","typed":["Login"]},{"field":"description","markdown":{"markdown":"Steps.","from":""}}]}`,
		`move conn-jira {"issue":"10042","move":"21"}`,
	}
	if !slices.Equal(w.f.writes, want) {
		t.Fatalf("writes %q\nwant %q", w.f.writes, want)
	}
	if len(w.h.rows) != 1 || !w.h.rows[0].Created || !slices.Equal(w.h.rows[0].Tickets[0].Applied, []string{"created", "status"}) {
		t.Fatalf("rows %+v", w.h.rows)
	}

	w.f.writes = nil
	answer, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Second", "set": map[string]any{"status": "Shipped"}})
	if err != nil || answer != `{"key":"WEB-42","refused":"status: WEB-42 cannot move to \"Shipped\"; it can move to \"To Do\", \"In Review\", or \"Done\"."}` {
		t.Fatalf("a refused status after the create = %s, %v", answer, err)
	}
	if !strings.Contains(w.f.writes[0], `"type":"10002"`) {
		t.Fatalf("a create with no type is a Task: %q", w.f.writes)
	}

	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: "create", State: "refused", Refusal: &trackerapp.RefusalView{
			Reason: "missing", Message: "jira: Component and Fix versions are required",
			Fields: []trackerapp.FieldMessageView{{Field: "components", Message: "Component is required"}, {Field: "fixVersions", Message: "Fix versions is required"}},
		}}}}, nil
	}
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Third"})
	refused(t, err, CodeInvalidRequest, `WEB needs "Component" and "Fix versions" to create a ticket; give them in set.`)
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: "create", State: "unknown"}}, CreateUnknown: true}, nil
	}
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Fourth"})
	refused(t, err, CodeUnknown, "Jira's answer never came, so the ticket may or may not have been created; the same call again looks for it first.")

	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "OPS", "title": "Elsewhere"})
	refused(t, err, CodeNotFound, "No board of project OPS is on this computer; ask the user to add one in Settings, Issues.")
	w.h.createBoards["APP"] = []CreateBoard{{Connection: "conn-jira", Board: "9"}}
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "app", "title": "Unread"})
	refused(t, err, CodeNotFound, "Project APP's board on example.atlassian.net has not been read on this computer yet; ask the user to open it in Issues, then call again.")
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Typo", "type": "Bugg"})
	refused(t, err, CodeInvalidRequest, `WEB has no issue type "Bugg"; it has "Story", "Task", and "Bug".`)
}

// A create an earlier call made is not made again: its repeat answers the
// key the mirror holds and sets only the steps after the create the
// ticket does not hold, and its row is a change, not a create. One an
// earlier call is still sending, or whose earlier attempt could not be
// looked for, creates nothing and says why.
func TestCreateIsNotMadeTwice(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10002", Name: "Task"}}
	w.f.cards["conn-jira/10042"] = trackerapp.CardView{ID: "10042", Key: "WEB-42"}
	w.f.details["conn-jira/10042"] = writableDetail("WEB-42", "10042")
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		if name == "create" {
			return trackerapp.WriteView{Parts: []trackerapp.PartView{}, Repeat: &trackerapp.RepeatView{Issue: "10042", Key: "WEB-42"}}, nil
		}
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "applied"}}}, nil
	}
	answer, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Login fails", "set": map[string]any{"status": "Done"}})
	if want := `{"key":"WEB-42","applied":["status"],"note":"Your earlier call created it; it was not created again."}`; err != nil || answer != want {
		t.Fatalf("a repeat of a create = %s, %v\nwant %s", answer, err, want)
	}
	if len(w.h.rows) != 1 || w.h.rows[0].Created || !slices.Equal(w.h.rows[0].Tickets[0].Applied, []string{"status"}) {
		t.Fatalf("rows %+v", w.h.rows)
	}
	w.h.rows = nil
	answer, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Login fails", "set": map[string]any{"status": "In Progress"}})
	if want := `{"key":"WEB-42","already":["status"],"note":"Your earlier call created it; it was not created again."}`; err != nil || answer != want || len(w.h.rows) != 0 {
		t.Fatalf("a repeat that changes nothing = %s, %v, rows %+v\nwant %s", answer, err, w.h.rows, want)
	}
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{}, fmt.Errorf("the write: %w", trackerapp.ErrAgentWriteInFlight)
	}
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Login fails"})
	refused(t, err, CodeInvalidRequest, "The same ticket is still being created by an earlier call, so it was not created again; call again once that call has answered.")
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{}, fmt.Errorf("%w: down", trackerapp.ErrAgentRepeatUnchecked)
	}
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Login fails"})
	refused(t, err, CodeUnknown, "An earlier call may have created this ticket, and the tracker could not be searched to check, so it was not created again; call again to check again.")
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{}, fmt.Errorf("%w: %w", trackerapp.ErrAgentRepeatUnchecked, trackerapp.ErrAgentAccountUnknown)
	}
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Login fails"})
	refused(t, err, CodeUnknown, "An earlier call may have created this ticket, and it cannot be checked: the connection's account was not known when it was sent. It was not created again, and a call again within an hour of that attempt is refused the same way; find the ticket by its title to see whether it is there.")
}

// A confirmed create answers the key its own answer gave and writes its
// row, though the ticket cannot be read to set the steps after it: those
// are not sent, and the note says so.
func TestCreateConfirmedWhoseReadFails(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10002", Name: "Task"}}
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Issue: "10042", Step: "create", State: "applied"}}, Created: "10042", CreatedKey: "WEB-42"}, nil
	}
	answer, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "Login fails", "set": map[string]any{"status": "Done", "sprint": "Sprint 14"}})
	if want := `{"key":"WEB-42","not_sent":["sprint","status"],"note":"WEB-42 could not be read to set sprint and status; ticket_update sets them."}`; err != nil || answer != want {
		t.Fatalf("a create whose read failed = %s, %v\nwant %s", answer, err, want)
	}
	if len(w.h.rows) != 1 || !w.h.rows[0].Created || w.h.rows[0].Tickets[0].Key != "WEB-42" {
		t.Fatalf("rows %+v", w.h.rows)
	}
}

// Linear has no issue types; a project two sites have boards of names
// both.
func TestCreateTargets(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	linear := linearSource
	linear.Kind = "linear"
	w.f.sources = []trackerapp.TicketSource{linear}
	w.f.boards["conn-linear/team"] = trackerapp.BoardSummary{Name: "Web", Project: "team-id"}
	w.h.createBoards["WEB"] = append(w.h.createBoards["WEB"], CreateBoard{Connection: "conn-linear", Board: "team", Thread: true})
	_, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New", "type": "Bug"})
	refused(t, err, CodeInvalidRequest, "Linear has no issue types; leave out type.")
	jira := jiraSource
	jira.Kind = "jira"
	w.f.sources = []trackerapp.TicketSource{jira, linear}
	_, err = w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New"})
	refused(t, err, CodeAmbiguous, "WEB is a project on example.atlassian.net and example; write it as example.atlassian.net/WEB or example/WEB.")
}
