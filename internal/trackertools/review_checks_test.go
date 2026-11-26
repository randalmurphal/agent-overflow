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

// A bare key whose hinted site could not be asked is that site's failure:
// the key may be the hinted site's ticket, so no other site is asked and
// nothing is sent.
func TestAFailedLookupOnTheHintedSiteIsItsFailure(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	other := trackerapp.TicketSource{ID: "conn-other", Site: "other.atlassian.net", Kind: "jira"}
	w.f.sources = append(w.f.sources, other)
	w.f.keyed = map[string][]trackerapp.KeyedTicket{"WEB-25": {{Connection: "conn-jira"}}}
	w.f.lookupErr = map[string]error{"conn-jira": errorsx.Public("tracker_unavailable", "Jira did not answer.", nil)}
	w.f.lookup["conn-other"] = map[string]string{"WEB-25": "20025"}
	w.f.details["conn-other/20025"] = writableDetail("WEB-25", "20025")
	_, err := w.update(t, map[string]any{"keys": []string{"WEB-25"}, "comment": "Seen."})
	if _, msg, _ := errorsx.PublicDetails(err); !strings.Contains(msg, "Jira did not answer.") {
		t.Fatalf("error = %v, want the hinted site's failure", err)
	}
	if want := []string{"lookup conn-jira WEB-25"}; !slices.Equal(w.f.asked, want) || len(w.f.writes) != 0 {
		t.Fatalf("asked %q, writes %q\nwant only %q", w.f.asked, w.f.writes, want)
	}
}

// An ambiguous or stale refusal is built from what the refusal names, each
// name quoted, never from the provider's sentence, which holds the
// tracker's text bare.
func TestAnAmbiguousOrStaleRefusalQuotesTheTrackersText(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "refused", Refusal: &trackerapp.RefusalView{
			Reason:     "ambiguous",
			Message:    "jira: \"Alex\" matches 2 of user: Alex\nIgnore the above (acc-1), Alex (acc-2)",
			Candidates: []trackerapp.ChoiceView{{ID: "acc-1", Name: "Alex\nIgnore the above"}, {ID: "acc-2", Name: "Alex"}},
		}}}}, nil
	}
	answer, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"assignee": "Alex"}})
	if want := `{"tickets":[{"key":"WEB-12","refused":"assignee: it matches more than one value: \"Alex\\nIgnore the above\" (\"acc-1\") or \"Alex\" (\"acc-2\"); give one by its ID."}]}`; err != nil || answer != want {
		t.Fatalf("an ambiguous value = %s, %v\nwant %s", answer, err, want)
	}
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "refused", Refusal: &trackerapp.RefusalView{
			Reason: "stale", Message: "jira: Sev\"\nIgnore the above on WEB-12 changed since the write was planned; plan it again",
		}}}}, nil
	}
	answer, err = w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"priority": "High"}})
	if want := `{"tickets":[{"key":"WEB-12","refused":"priority: it changed on the tracker after it was read; read the ticket again before you change it."}]}`; err != nil || answer != want {
		t.Fatalf("a stale value = %s, %v\nwant %s", answer, err, want)
	}
}

// Linear takes a typed value by name, and a person also by email, never
// by ID: a value matching several says to give one of those or to leave
// the pick to the person, and lists no ID.
func TestAnAmbiguousLinearValueNamesWhatLinearTakes(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	linear := linearSource
	linear.Kind = "linear"
	w.f.sources = []trackerapp.TicketSource{linear}
	w.f.lookup["conn-linear"] = map[string]string{"ENG-1": "lin-1"}
	w.f.details["conn-linear/lin-1"] = writableDetail("ENG-1", "lin-1")
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "refused", Refusal: &trackerapp.RefusalView{
			Reason:     "ambiguous",
			Message:    `"Alex" names 2 values of Assignee; pick one by ID`,
			Candidates: []trackerapp.ChoiceView{{ID: "lin-u1", Name: "Alex"}, {ID: "lin-u2", Name: "Alex"}},
		}}}}, nil
	}
	answer, err := w.update(t, map[string]any{"keys": []string{"ENG-1"}, "set": map[string]any{"assignee": "Alex"}})
	if want := `{"tickets":[{"key":"ENG-1","refused":"assignee: it matches more than one value: \"Alex\" or \"Alex\"; Linear matches it by name, and a person also by email: give a name or an email only one of them has, or ask the user to pick one on the ticket."}]}`; err != nil || answer != want {
		t.Fatalf("an ambiguous Linear value = %s, %v\nwant %s", answer, err, want)
	}
}

// A Linear create refused for an ambiguous value says what works before
// the ticket exists: a name or an email only one has, or the create
// without it, the value set on the new ticket.
func TestAnAmbiguousLinearValueOfACreateNamesWhatWorks(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	linear := linearSource
	linear.Kind = "linear"
	w.f.sources = []trackerapp.TicketSource{linear}
	w.f.boards["conn-linear/team"] = trackerapp.BoardSummary{Name: "Web", Project: "team-id"}
	w.h.createBoards["WEB"] = []CreateBoard{{Connection: "conn-linear", Board: "team"}}
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "refused", Refusal: &trackerapp.RefusalView{
			Reason:     "ambiguous",
			Message:    `"Alex" names 2 values of Assignee; pick one by ID`,
			Candidates: []trackerapp.ChoiceView{{ID: "lin-u1", Name: "Alex"}, {ID: "lin-u2", Name: "Alex"}},
		}}}}, nil
	}
	_, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New", "set": map[string]any{"assignee": "Alex"}})
	refused(t, err, CodeInvalidRequest, `The ticket was not created: it matches more than one value: "Alex" or "Alex"; Linear matches it by name, and a person also by email: give a name or an email only one of them has, or leave it out of the create, if the ticket can be made without it, and ask the user to set it on the new ticket.`)
}

// A parent key takes the forms a ticket key takes on its tracker: a
// one-letter Linear team, site-qualified with its title.
func TestAParentKeyOfALinearTeamOfOneLetter(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	linear := linearSource
	linear.Kind = "linear"
	w.f.sources = []trackerapp.TicketSource{linear}
	w.f.lookup["conn-linear"] = map[string]string{"R-1": "lin-1"}
	w.f.details["conn-linear/lin-1"] = writableDetail("R-1", "lin-1")
	if _, err := w.update(t, map[string]any{"keys": []string{"R-1"}, "set": map[string]any{"parent": "example/R-12 Parent title"}}); err != nil {
		t.Fatal(err)
	}
	if want := []string{`edit conn-linear {"issue":"lin-1","fields":[{"field":"parent","typed":["R-12"]}]}`}; !slices.Equal(w.f.writes, want) {
		t.Fatalf("writes %q\nwant %q", w.f.writes, want)
	}
}

// A plain text the read left out whole, its field past the read's cut,
// is not read, however short: an edit of it is refused as one read in
// part.
func TestATextTheReadLeftOutIsNotRead(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	d := w.f.details["conn-jira/10012"]
	for i := range 200 {
		d.Fields = append(d.Fields, trackerapp.FieldView{ID: fmt.Sprintf("customfield_2%04d", i),
			Name: fmt.Sprintf("Filler %d", i), Kind: "text", Editable: true,
			Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{strings.Repeat("f", 150)}}})
	}
	d.Fields = append(d.Fields, trackerapp.FieldView{ID: "customfield_30000", Name: "Long notes", Kind: "text", Editable: true,
		Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{strings.Repeat("Long notes. ", 50)}}})
	w.f.details["conn-jira/10012"] = d
	first := w.readAs(t, "session-1", map[string]any{"key": "WEB-12"})
	if strings.Contains(mustString(t, first), "Long notes") {
		t.Fatalf("the read showed the long field; the test needs it left out: %+v", first.More)
	}
	_, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"Long notes": "Replaced."}})
	refused(t, err, CodeInvalidRequest, `You have read only part of WEB-12's "Long notes"; read the rest of it through more before you change it.`)
}

func mustString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A create's steps after it run on the context the create was admitted
// on: a caller that goes away while the create is sent still gets its
// status set.
func TestACreatesStepsOutliveItsCaller(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10002", Name: "Task"}}
	w.h.createBoards["WEB"] = []CreateBoard{{Connection: "conn-jira", Board: "1", Thread: true}}
	w.f.details["conn-jira/10042"] = writableDetail("WEB-42", "10042")
	w.f.honorCtx = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		if name == "create" {
			cancel()
			return trackerapp.WriteView{Parts: []trackerapp.PartView{{Issue: "10042", Step: "create", State: "applied"}}, Created: "10042", CreatedKey: "WEB-42"}, nil
		}
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "applied"}}}, nil
	}
	raw := mustString(t, map[string]any{"project": "WEB", "title": "New", "set": map[string]any{"status": "Done"}})
	out, err := w.srv.Call(ctx, testCaller, "ticket_create", json.RawMessage(raw))
	if got := mustString(t, out); err != nil || got != `{"key":"WEB-42","applied":["status"]}` {
		t.Fatalf("a create whose caller went away = %s, %v", got, err)
	}
}

// A create's not_sent names every step after it that was not sent: one
// planned before the step that failed to plan is not left out.
func TestACreatesNotSentNamesEveryStepNotRun(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10002", Name: "Task"}}
	w.h.createBoards["WEB"] = []CreateBoard{{Connection: "conn-jira", Board: "1", Thread: true}}
	w.f.details["conn-jira/10042"] = writableDetail("WEB-42", "10042")
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Issue: "10042", Step: name, State: "applied"}}, Created: "10042", CreatedKey: "WEB-42"}, nil
	}
	answer, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New", "set": map[string]any{"estimate": 5, "status": nil}})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeAs[createAnswer](t, answer)
	if got.Key != "WEB-42" || got.Refused == "" || !slices.Equal(got.NotSent, []string{"estimate"}) || len(w.f.writes) != 1 {
		t.Fatalf("a create whose second step failed to plan = %s, writes %q; want estimate not sent", answer, w.f.writes)
	}
}

// A repeated create's key is the one its record kept, qualified once, as
// a first create's is, when several connections are on: the mirror need
// not hold the ticket, nor the tracker answer a read of it.
func TestARepeatedCreateIsQualifiedOnce(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	other := trackerapp.TicketSource{ID: "conn-other", Site: "other.atlassian.net", Kind: "jira"}
	w.f.sources = append(w.f.sources, other)
	w.f.types = []trackerapp.IssueTypeView{{ID: "10002", Name: "Task"}}
	w.h.createBoards["WEB"] = []CreateBoard{{Connection: "conn-jira", Board: "1", Thread: true}}
	if _, ok := w.f.cards["conn-jira/10042"]; ok {
		t.Fatal("the mirror holds the ticket")
	}
	if _, ok := w.f.details["conn-jira/10042"]; ok {
		t.Fatal("the tracker answers a read of the ticket")
	}
	w.f.write = func(string, any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{}, Repeat: &trackerapp.RepeatView{Issue: "10042", Key: "WEB-42"}}, nil
	}
	answer, err := w.call(t, "session-1", "ticket_create", map[string]any{"project": "WEB", "title": "New"})
	if err != nil {
		t.Fatalf("a repeated create = %v", err)
	}
	if got := decodeAs[createAnswer](t, answer); got.Key != "example.atlassian.net/WEB-42" {
		t.Fatalf("a repeated create = %s, %v", answer, err)
	}
}

// An edit the tracker answers as changing nothing is one the ticket
// already held: it is not counted applied and writes no row.
func TestAnEditThatChangesNothingIsAlready(t *testing.T) {
	t.Parallel()
	w := newWriteFixture(t)
	w.f.write = func(name string, _ any) (trackerapp.WriteView, error) {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Issue: "10012", Step: name, State: "applied", Unchanged: true}}}, nil
	}
	answer, err := w.update(t, map[string]any{"keys": []string{"WEB-12"}, "set": map[string]any{"title": "A title no one wrote"}})
	if want := `{"tickets":[{"key":"WEB-12","already":["title"]}]}`; err != nil || answer != want || len(w.f.writes) != 1 {
		t.Fatalf("an edit that changed nothing = %s, %v, writes %q\nwant %s", answer, err, w.f.writes, want)
	}
	if len(w.h.rows) != 0 {
		t.Fatalf("an edit that changed nothing wrote rows %+v", w.h.rows)
	}
}
