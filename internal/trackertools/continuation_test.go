package trackertools

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

// staleWEB12 is the refusal of a continuation the ticket as it is now
// cannot honor.
const staleWEB12 = "more is from an earlier version of WEB-12, which has changed since; read it again."

// follow passes c as more, turning a panic into an error so each case
// reports.
func follow(t *testing.T, f *fakeTracker, c moreCursor) (answer string, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return call(t, f, "ticket_read", map[string]any{"more": encodeCursor(c)})
}

func cursorOf(t *testing.T, value string) moreCursor {
	t.Helper()
	var c moreCursor
	if err := decodeCursor("more", value, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// wideText is n bytes of two-byte runes, so an odd offset is inside one.
func wideText(n int) string { return strings.Repeat("é", n/2) }

// textTicket is a ticket whose description, first comment and one field
// are each long enough to be cut, in two-byte runes, with the comment
// edited once.
func textTicket() trackerapp.DetailView {
	d := detail("WEB-12")
	d.Card.Updated = 1_764_000_000_000
	d.Description = wideText(60 << 10)
	d.Comments = []trackerapp.CommentView{{ID: "c1", Author: trackerapp.UserView{Name: "User 1"}, Created: 1_763_990_000_000, Updated: 1_763_995_000_000, Body: wideText(30 << 10)}}
	d.Fields = []trackerapp.FieldView{{ID: "customfield_1", Name: "Acceptance", Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{wideText(8 << 10)}}}}
	return d
}

// A continuation of a text is refused, never sliced, when the text as it
// is now cannot honor its offset: a text shortened since, a comment edited
// since (on Linear an edit leaves the ticket's update time), or a value a
// client made with an offset past the end, inside a rune or below zero.
func TestAContinuationTheTextCannotHonorIsRefused(t *testing.T) {
	t.Parallel()
	d := textTicket()
	f := longTicketTracker(d)
	raw, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	first := decodeAs[readAnswer](t, raw)
	parts := map[string]moreCursor{
		"description": cursorOf(t, moreFor(t, first, "the rest of the description (")),
		"comment":     cursorOf(t, moreFor(t, first, "the rest of the comment by User 1 ")),
		"field":       cursorOf(t, moreFor(t, first, "the rest of Acceptance (")),
	}
	for name, c := range parts {
		for _, bad := range []struct {
			what   string
			offset int
			want   string
		}{
			{"past the end", 1 << 30, staleWEB12},
			{"inside a rune", c.Offset + 1, staleWEB12},
			{"below zero", -1, "more is not one these tools gave; read the ticket again."},
		} {
			at := c
			at.Offset = bad.offset
			_, err := follow(t, f, at)
			if _, message, ok := errorsx.PublicDetails(err); !ok || message != bad.want {
				t.Errorf("the %s's rest with an offset %s: %v\nwant %s", name, bad.what, err, bad.want)
			}
		}
	}

	// Each text shortened since, the ticket's update time left as it was.
	shorter := textTicket()
	shorter.Description = wideText(2 << 10)
	shorter.Comments[0].Body = wideText(2 << 10)
	shorter.Fields[0].Value.Texts = []string{wideText(1 << 10)}
	f.details["conn-jira/10012"] = shorter
	for name, c := range parts {
		if _, err := follow(t, f, c); publicMessage(err) != staleWEB12 {
			t.Errorf("the %s's rest after the text was shortened: %v\nwant %s", name, err, staleWEB12)
		}
	}

	// The comment edited since to a longer body: the ticket's update time
	// stays (Linear), the comment's own moves.
	edited := textTicket()
	edited.Comments[0].Body += " and more"
	edited.Comments[0].Updated += 60_000
	f.details["conn-jira/10012"] = edited
	if _, err := follow(t, f, parts["comment"]); publicMessage(err) != staleWEB12 {
		t.Errorf("the comment's rest after an edit: %v\nwant %s", err, staleWEB12)
	}
	f.details["conn-jira/10012"] = d
	if _, err := follow(t, f, parts["comment"]); err != nil {
		t.Errorf("the comment's rest of the ticket as it was: %v", err)
	}
}

func publicMessage(err error) string {
	_, message, _ := errorsx.PublicDetails(err)
	return message
}

// A continuation of a list or of fields whose position is past what the
// ticket holds now is refused, for every part, and one on a connection no
// longer on is refused saying so.
func TestAContinuationPastAListIsRefused(t *testing.T) {
	t.Parallel()
	d := textTicket()
	d.Links = []trackerapp.LinkView{{Relation: "relates to", Issue: trackerapp.LinkedIssueView{Key: "WEB-1"}}}
	d.Attachments = []trackerapp.AttachmentView{{Name: "a.png"}}
	d.Card.Labels = []trackerapp.ChoiceView{{ID: "l", Name: "frontend"}}
	d.History = []trackerapp.EventView{{At: 1, Field: "Status", From: "To Do", To: "Done"}}
	f := longTicketTracker(d)
	f.comments = func(_, _, _ string) (trackerapp.CommentPageView, error) {
		return trackerapp.CommentPageView{Comments: []trackerapp.CommentView{{ID: "c9", Body: "older"}}}, nil
	}
	base := moreCursor{Connection: "conn-jira", Issue: "10012", Updated: d.Card.Updated}
	huge := 1 << 30
	for _, c := range []moreCursor{
		{Part: partFields, Index: huge},
		{Part: partField, Index: huge},
		{Part: partField, Item: huge},
		{Part: partLinks, Index: huge},
		{Part: partAttachments, Index: huge},
		{Part: partLabels, Index: huge},
		{Part: partHistory, Index: huge},
		{Part: partComments, Index: huge},
		{Part: partComments, Page: "50", Index: huge},
		{Part: partComment, Comment: "c1", Offset: huge},
	} {
		c.Connection, c.Issue, c.Updated = base.Connection, base.Issue, base.Updated
		if _, err := follow(t, f, c); publicMessage(err) != staleWEB12 {
			t.Errorf("part %q index %d item %d page %q: %v\nwant %s", c.Part, c.Index, c.Item, c.Page, err, staleWEB12)
		}
	}
	gone := base
	gone.Connection, gone.Part = "conn-gone", partDescription
	_, err := follow(t, f, gone)
	refused(t, err, CodeInvalidRequest, "more is from a connection that is no longer on; read the ticket again.")
}

// Two accounts on one Jira site: a site/KEY is looked up on each of that
// site's connections in order and read from the first that has it; a bare
// key both accounts reach is that one ticket; a refusal names a site once.
func TestAKeyOnASiteTwoAccountsReach(t *testing.T) {
	t.Parallel()
	first := trackerapp.TicketSource{ID: "conn-a", Site: "example.atlassian.net"}
	second := trackerapp.TicketSource{ID: "conn-b", Site: "example.atlassian.net"}
	restricted := detail("ENG-12")
	restricted.Card.Title = "Only the second account sees it"
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{first, second, linearSource},
		lookup: map[string]map[string]string{
			"conn-a": {"WEB-7": "10007"},
			"conn-b": {"ENG-12": "10012", "WEB-7": "10007"},
		},
		details: map[string]trackerapp.DetailView{"conn-b/10012": restricted, "conn-a/10007": detail("WEB-7"), "conn-b/10007": detail("WEB-7")},
	}
	answer, err := call(t, f, "ticket_read", map[string]any{"key": "example.atlassian.net/ENG-12"})
	if err != nil || !strings.HasPrefix(answer, `{"key":"example.atlassian.net/ENG-12","title":"Only the second account sees it"`) {
		t.Fatalf("a site/KEY only the second account reaches = %s, %v", answer, err)
	}
	if want := []string{"lookup conn-a ENG-12", "lookup conn-b ENG-12", "detail conn-b 10012"}; !slices.Equal(f.asked, want) {
		t.Fatalf("asked %q\nwant %q", f.asked, want)
	}

	f.asked = nil
	answer, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-7"})
	if err != nil || !strings.HasPrefix(answer, `{"key":"example.atlassian.net/WEB-7",`) {
		t.Fatalf("a bare key two accounts of one site reach = %s, %v", answer, err)
	}
	if want := "detail conn-a 10007"; !slices.Contains(f.asked, want) {
		t.Fatalf("asked %q\nwant the first account's read, %q", f.asked, want)
	}

	_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-99"})
	refused(t, err, CodeNotFound, "No ticket WEB-99 on example.atlassian.net or example.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": "example.atlassian.net/WEB-99"})
	refused(t, err, CodeNotFound, "No ticket WEB-99 on example.atlassian.net.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": "other.atlassian.net/WEB-12"})
	refused(t, err, CodeNotFound, "No connection on this computer reaches other.atlassian.net; the sites on are example.atlassian.net or example.")

	f.lookup["conn-linear"] = map[string]string{"WEB-7": "uuid-7"}
	_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-7"})
	refused(t, err, CodeAmbiguous, "WEB-7 is on example.atlassian.net and example; name one: example.atlassian.net/WEB-7 or example/WEB-7.")
}

// The rest of one field comes in one answer when the budget holds it,
// not in pieces of the first answer's head size.
func TestTheRestOfOneFieldFillsTheAnswer(t *testing.T) {
	t.Parallel()
	d := detail("WEB-12")
	long := strings.Repeat("Acceptance criterion that the team wrote out. ", 400) // 18 KB
	d.Fields = []trackerapp.FieldView{{ID: "customfield_1", Name: "Acceptance", Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{long}}}}
	f := longTicketTracker(d)
	raw, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	rest := readAll(t, f, moreFor(t, decodeAs[readAnswer](t, raw), "the rest of Acceptance ("), "the rest of Acceptance (")
	if len(rest) != 1 {
		t.Fatalf("the rest of an 18 KB field took %d answers, want 1", len(rest))
	}
}

// A search answer fits the budget whatever the trackers name: a status, an
// assignee and a title are cut, and so is a refusal, and every row stays,
// though eight refusals and twenty rows at those cuts are over it.
func TestAFindAnswerFitsTheBudget(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("Name ", 1000)
	f := &fakeTracker{}
	for i := range 8 {
		f.sources = append(f.sources, trackerapp.TicketSource{ID: fmt.Sprint("conn-off-", i), Site: fmt.Sprintf("off%d.example.com", i)})
	}
	f.sources = append(f.sources, jiraSource)
	f.search = func(connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		if connection != jiraSource.ID {
			return trackerapp.SearchPageView{}, errorsx.Public("tracker_failed", huge, nil)
		}
		page := trackerapp.SearchPageView{Cards: cards("WEB", in.Limit), Next: "more"}
		for i := range page.Cards {
			page.Cards[i].Title = huge
			page.Cards[i].Status.Name = huge
			page.Cards[i].Assignee = &trackerapp.UserView{Name: huge}
		}
		return page, nil
	}
	raw, err := call(t, f, "ticket_find", map[string]any{"text": "name"})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > answerBudget {
		t.Fatalf("the find answer is %d bytes, over the budget of %d", len(raw), answerBudget)
	}
	page := decodeAs[findAnswer](t, raw)
	if len(page.Tickets) != pageSize || page.Next == "" || len(page.Unsearched) != 8 {
		t.Fatalf("the find answer has %d rows, next %q and %d unsearched", len(page.Tickets), page.Next, len(page.Unsearched))
	}
	for _, row := range page.Tickets {
		if !strings.HasSuffix(row.Status, cut) || !strings.HasSuffix(row.Assignee, cut) || !strings.HasSuffix(row.Title, cut) {
			t.Fatalf("a row's names are not cut: %+v", row)
		}
	}
}
