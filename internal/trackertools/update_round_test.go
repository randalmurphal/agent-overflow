package trackertools

import (
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/trackerapp"
)

// readAs reads key as session, failing the test on a refusal.
func (w *writeFixture) readAs(t *testing.T, session string, args map[string]any) readAnswer {
	t.Helper()
	answer, err := w.call(t, session, "ticket_read", args)
	if err != nil {
		t.Fatal(err)
	}
	return decodeAs[readAnswer](t, answer)
}

// A mark follows only what the tracker confirmed this call wrote: a
// status applied and a description refused leave the description unread
// at the version the status left, so the same call again is refused; a
// description applied is the session's own there, and the ticket's other
// texts are unread.
func TestAMarkFollowsOnlyWhatTheCallWrote(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.readAs(t, "session-1", map[string]any{"key": "WEB-12"})
	moved := func() {
		d := w.f.details["conn-jira/10012"]
		d.Card.Updated = w.f.cards["conn-jira/10012"].Updated
		w.f.details["conn-jira/10012"] = d
	}
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		if name == "edit" {
			return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: "edit", State: "refused", Refusal: &trackerapp.RefusalView{
				Reason: "stale", Message: "jira: Description on WEB-12 changed since the write was planned; plan it again"}}}}, nil
		}
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "applied"}}}, nil
	}
	args := map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"status": "Done", "description": "Replaced."}}
	answer, err := w.update(t, args)
	if err != nil || !strings.Contains(answer, `"applied":["status"],"refused":"description: `) {
		t.Fatalf("the first call = %s, %v", answer, err)
	}
	moved()
	_, err = w.update(t, args)
	refused(t, err, CodeInvalidRequest, "WEB-12 changed since you read it; read it again before you change its description.")

	w.f.write = nil
	w.readAs(t, "session-1", map[string]any{"key": "WEB-12"})
	// The write's refresh leaves the card at 3000.
	c := w.f.cards["conn-jira/10012"]
	c.Updated = 3_000
	w.f.cards["conn-jira/10012"] = c
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"description": "Mine."}}); err != nil {
		t.Fatal(err)
	}
	d := w.f.details["conn-jira/10012"]
	d.Description = "Mine."
	w.f.details["conn-jira/10012"] = d
	moved()
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"description": "Mine again."}}); err != nil {
		t.Fatalf("an edit of the description the session wrote: %v", err)
	}
	moved()
	_, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"Notes": "New notes."}})
	refused(t, err, CodeInvalidRequest, `WEB-12 changed since you read it; read it again before you change its "Notes".`)
}

// A read that answers after its session ended records nothing, so the
// session's next long-text edit has no read to stand on.
func TestASessionThatEndedRecordsNoMarks(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.h.ended = map[string]bool{"session-1": true}
	w.readAs(t, "session-1", map[string]any{"key": "WEB-12"})
	w.h.ended = nil
	_, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"description": "Replaced."}})
	refused(t, err, CodeInvalidRequest, "Read WEB-12 before you change its description.")
}

// A plain text field is judged per value, by what the read gave: a long
// second value needs a read, and so does a list whose read stopped part
// way though each value is short. An edit that replaces them carries
// them, for the tracker to compare.
func TestAPlainTextIsJudgedPerValue(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	long := strings.Repeat("Long text. ", 300)
	d := w.f.details["conn-jira/10012"]
	d.Fields = append(d.Fields, trackerapp.FieldView{ID: "customfield_10060", Name: "Tags", Kind: "text", List: true, Editable: true,
		Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{"short", long}}})
	w.f.details["conn-jira/10012"] = d
	args := map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"Tags": []string{"one"}}}
	_, err := w.update(t, args)
	refused(t, err, CodeInvalidRequest, `Read WEB-12 before you change its "Tags".`)
	first := w.readAs(t, "session-1", map[string]any{"key": "WEB-12"})
	_, err = w.update(t, args)
	refused(t, err, CodeInvalidRequest, `You have read only part of WEB-12's "Tags"; read the rest of it through more before you change it.`)
	for label, more := range first.More {
		if strings.HasPrefix(label, "the rest of Tags") {
			w.readAs(t, "session-1", map[string]any{"more": more})
		}
	}
	if _, err := w.update(t, args); err != nil {
		t.Fatalf("an edit after reading the rest: %v", err)
	}
	if want := `{"field":"customfield_10060","typed":["one"],"replaces":["short","` + long + `"]}`; len(w.f.writes) != 1 || !strings.Contains(w.f.writes[0], want) {
		t.Fatalf("writes %q\nwant one with %s", w.f.writes, want)
	}

	items := make([]string, 20)
	for i := range items {
		items[i] = strings.Repeat("x", 199) + string(rune('a'+i))
	}
	d.Fields[len(d.Fields)-1].Value.Texts = items
	d.Card.Updated = 4_000
	w.f.details["conn-jira/10012"] = d
	w.readAs(t, "session-1", map[string]any{"key": "WEB-12"})
	_, err = w.update(t, args)
	refused(t, err, CodeInvalidRequest, `You have read only part of WEB-12's "Tags"; read the rest of it through more before you change it.`)
}

// A clear of a description is the empty Markdown from the text it
// replaces, which the tracker compares as it does any Markdown edit.
func TestAClearOfADescriptionCarriesWhatItReplaces(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.readAs(t, "session-1", map[string]any{"key": "WEB-12"})
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"description": nil}}); err != nil {
		t.Fatal(err)
	}
	if want := `{"field":"description","markdown":{"markdown":"","from":"The login page fails."}}`; len(w.f.writes) != 1 || !strings.Contains(w.f.writes[0], want) {
		t.Fatalf("writes %q\nwant one with %s", w.f.writes, want)
	}
}

// What an approval carries: each ticket's lines, a line that shows a
// value in part naming the value it shows, and every value and the
// comment whole, up to the bound; a call past it is refused before it
// reads or asks anything.
func TestAnApprovalCarriesEveryValueWhole(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	for _, key := range []string{"WEB-12", "WEB-18"} {
		w.readAs(t, "session-1", map[string]any{"key": key})
	}
	label := strings.Repeat("Long label ", 20)
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12", "WEB-18"}, "set": map[string]any{"labels": label, "description": "New text.", "status": "Done"}, "comment": "Done."}); err != nil {
		t.Fatal(err)
	}
	a := w.h.approvals[0]
	shown, _ := textClip(label, 0, shownCut)
	if want := []ApprovalChange{{Line: "description → new text (9 characters)", Value: "description"}, {Line: "labels → " + shown, Value: "labels"}, {Line: "Status → Done"}}; !slices.Equal(a.Tickets[0].Changes, want) {
		t.Fatalf("changes %+v\nwant %+v", a.Tickets[0].Changes, want)
	}
	if want := []ApprovalValue{{Name: "description", Value: "New text."}, {Name: "labels", Value: label}, {Name: "status", Value: "Done"}}; !slices.Equal(a.Values, want) || a.Comment != "Done." {
		t.Fatalf("values %+v, comment %q", a.Values, a.Comment)
	}

	// At the bound: carried whole. Past it: refused, with nothing read.
	quoted := len(`""`)
	at := strings.Repeat("a", maxApprovalBytes-len("Done.")-len("priority")-quoted)
	w.h.approvals, w.f.asked = nil, nil
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12", "WEB-18"}, "set": map[string]any{"priority": at}, "comment": "Done."}); err != nil || len(w.h.approvals) != 1 {
		t.Fatalf("a call at the bound = %v, approvals %d", err, len(w.h.approvals))
	}
	if v := w.h.approvals[0].Values; len(v) != 1 || v[0].Value != at {
		t.Fatal("a value at the bound was not carried whole")
	}
	w.h.approvals, w.f.asked = nil, nil
	_, err := w.update(t, map[string]any{"keys": []string{"WEB-12", "WEB-18"}, "set": map[string]any{"priority": at + "a"}, "comment": "Done."})
	refused(t, err, CodeInvalidRequest, "set and comment together hold more than 256 KiB, more than an approval shows the person; change fewer or shorter values in each call.")
	if len(w.h.approvals)+len(w.f.asked) != 0 {
		t.Fatalf("a call past the bound asked %+v, read %q", w.h.approvals, w.f.asked)
	}
}

// A thread made read-only while its approval waited sends nothing once
// the person approves.
func TestWritesAreAdmittedAfterTheApproval(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.h.approve = func(Approval) error { w.h.readOnly = true; return nil }
	_, err := w.update(t, map[string]any{"keys": []string{"WEB-12", "WEB-18"}, "comment": "Done."})
	refused(t, err, "tracker_read_only", "This thread is read-only, so it cannot change tickets; ask the user to change its access.")
	if len(w.f.writes) != 0 {
		t.Fatalf("an approval admitted writes on a read-only thread: %q", w.f.writes)
	}
}

// A create is admitted when it is sent: a thread made read-only while the
// call read the project creates nothing.
func TestACreateIsAdmittedWhenItIsSent(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10002", Name: "Task"}}
	w.h.createBoards["WEB"] = []ListedBoard{{Connection: "conn-jira", Board: "1", Thread: true}}
	w.h.readingProject = func() { w.h.readOnly = true }
	_, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New"})
	refused(t, err, "tracker_read_only", "This thread is read-only, so it cannot change tickets; ask the user to change its access.")
	if len(w.f.writes) != 0 {
		t.Fatalf("a create was sent on a read-only thread: %q", w.f.writes)
	}
}

// The boards a call judges are read once each, however many entries of
// set ask about them.
func TestABoardIsReadOncePerCall(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	if _, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"estimate": 5, "sprint": "Sprint 15", "Component": "Login", "priority": "High"}}); err != nil {
		t.Fatal(err)
	}
	if n := w.f.boardReads["conn-jira/1"]; n != 1 {
		t.Fatalf("board 1 was read %d times, want 1", n)
	}
}

// A create picks one board over every listing of its project: a board
// read here that the thread's project lists, before an unread one and
// before one another project lists, whichever connection of the site
// lists it.
func TestACreatePicksTheThreadsReadBoard(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	second := w.f.sources[0]
	second.ID = "conn-jira-2"
	w.f.sources = append(w.f.sources, second)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10002", Name: "Task"}}
	w.f.boards["conn-jira-2/2"] = trackerapp.BoardSummary{Name: "Team 2", Project: "10000"}
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Issue: "10042", Step: "create", State: "applied"}}, Created: "10042", CreatedKey: "WEB-42"}, nil
	}
	w.h.createBoards["WEB"] = []ListedBoard{{Connection: "conn-jira", Board: "9", Thread: true}, {Connection: "conn-jira", Board: "1"}, {Connection: "conn-jira-2", Board: "2", Thread: true}}
	if _, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New"}); err != nil {
		t.Fatal(err)
	}
	if len(w.f.writes) != 1 || !strings.HasPrefix(w.f.writes[0], `create conn-jira-2 {"board":"2"`) {
		t.Fatalf("writes %q, want the thread's read board 2", w.f.writes)
	}
	w.f.writes = nil
	w.h.createBoards["WEB"] = []ListedBoard{{Connection: "conn-jira", Board: "9", Thread: true}, {Connection: "conn-jira", Board: "1"}}
	if _, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New"}); err != nil {
		t.Fatal(err)
	}
	if len(w.f.writes) != 1 || !strings.HasPrefix(w.f.writes[0], `create conn-jira {"board":"1"`) {
		t.Fatalf("writes %q, want the read board 1", w.f.writes)
	}
}
