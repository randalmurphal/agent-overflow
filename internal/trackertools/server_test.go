package trackertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

// fakeTracker answers the tools' reads from what a test set, and records
// every tracker read it was asked.
type fakeTracker struct {
	sources   []trackerapp.TicketSource
	link      *trackerapp.LinkedTicket
	linkErr   error
	search    func(connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error)
	lookup    map[string]map[string]string // connection, key: issue
	lookupErr map[string]error
	details   map[string]trackerapp.DetailView // connection/issue
	comments  func(connection, issue, page string) (trackerapp.CommentPageView, error)
	asked     []string
}

func (f *fakeTracker) TicketSources() ([]trackerapp.TicketSource, error) { return f.sources, nil }

func (f *fakeTracker) LinkedTicket(string) (trackerapp.LinkedTicket, bool, error) {
	if f.linkErr != nil {
		return trackerapp.LinkedTicket{}, false, f.linkErr
	}
	if f.link == nil {
		return trackerapp.LinkedTicket{}, false, nil
	}
	return *f.link, true, nil
}

func (f *fakeTracker) Lookup(_ context.Context, connection string, keys []string) (trackerapp.LookupView, error) {
	f.asked = append(f.asked, "lookup "+connection+" "+strings.Join(keys, ","))
	if err := f.lookupErr[connection]; err != nil {
		return trackerapp.LookupView{}, err
	}
	var out trackerapp.LookupView
	for _, k := range keys {
		if issue, ok := f.lookup[connection][k]; ok {
			out.Found = append(out.Found, trackerapp.KeyMatchView{Key: k, Card: trackerapp.CardView{ID: issue, Key: k}})
		} else {
			out.Unmatched = append(out.Unmatched, k)
		}
	}
	return out, nil
}

func (f *fakeTracker) Search(_ context.Context, connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
	f.asked = append(f.asked, fmt.Sprintf("search %s %q limit %d page %q", connection, in.Text, in.Limit, in.Page))
	return f.search(connection, in)
}

func (f *fakeTracker) IssueDetail(_ context.Context, connection, issue string) (trackerapp.DetailView, error) {
	f.asked = append(f.asked, "detail "+connection+" "+issue)
	d, ok := f.details[connection+"/"+issue]
	if !ok {
		return trackerapp.DetailView{}, fmt.Errorf("%w: issue %s", trackerapp.ErrIssueNotFound, issue)
	}
	return d, nil
}

func (f *fakeTracker) Comments(_ context.Context, connection, issue, page string) (trackerapp.CommentPageView, error) {
	f.asked = append(f.asked, fmt.Sprintf("comments %s %s page %q", connection, issue, page))
	return f.comments(connection, issue, page)
}

var (
	jiraSource   = trackerapp.TicketSource{ID: "conn-jira", Kind: "jira", Site: "example.atlassian.net"}
	linearSource = trackerapp.TicketSource{ID: "conn-linear", Kind: "linear", Site: "example"}
)

// call runs one tool as thread-1 and answers its result as the JSON the
// transport sends, or its error.
func call(t *testing.T, f *fakeTracker, name string, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := New(f).Call(t.Context(), "thread-1", name, raw)
	if err != nil {
		return "", err
	}
	answer, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(answer), nil
}

// refused fails unless err is the public refusal code saying want.
func refused(t *testing.T, err error, code, want string) {
	t.Helper()
	got, message, ok := errorsx.PublicDetails(err)
	if !ok || got != code || message != want {
		t.Fatalf("error = %v (public %v, code %q)\nwant [%s] %s", err, ok, got, code, want)
	}
}

func decodeAs[T any](t *testing.T, answer string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(answer), &v); err != nil {
		t.Fatalf("decode %s: %v", answer, err)
	}
	return v
}

func cards(prefix string, n int) []trackerapp.CardView {
	out := make([]trackerapp.CardView, n)
	for i := range out {
		out[i] = trackerapp.CardView{ID: fmt.Sprint(i), Key: fmt.Sprintf("%s-%d", prefix, i+1), Title: "Title", Status: trackerapp.StatusView{ID: "1", Name: "To Do"}}
	}
	return out
}

// The tool list and guide are what every session with the server reads
// before it calls anything, so their size is a decision: this fails past
// the bound and says the size it found.
func TestTheToolListAndGuideStayWithinTheirBound(t *testing.T) {
	t.Parallel()
	const bound = 900
	list, err := json.Marshal(New(nil).Tools())
	if err != nil {
		t.Fatal(err)
	}
	if size := len(list) + len(Instructions); size > bound {
		t.Fatalf("the tool list (%d bytes) and guide (%d bytes) are %d bytes, over the bound of %d", len(list), len(Instructions), size, bound)
	}
}

// Every schema is closed, with required an array, so a misspelt argument
// is a refusal the model reads, and the names are the table's.
func TestToolSchemasAreClosed(t *testing.T) {
	t.Parallel()
	tools := New(nil).Tools()
	var names []string
	for _, tool := range tools {
		names = append(names, tool["name"].(string))
		schema := tool["inputSchema"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Errorf("%s is open", tool["name"])
		}
		if _, ok := schema["required"].([]string); !ok {
			t.Errorf("%s's required is %#v", tool["name"], schema["required"])
		}
	}
	if !slices.Equal(names, []string{"ticket_find", "ticket_read"}) || !slices.Equal(names, ToolNames()) {
		t.Fatalf("tools = %v", names)
	}
	f := &fakeTracker{sources: []trackerapp.TicketSource{jiraSource}}
	_, err := call(t, f, "ticket_find", map[string]any{"text": "login", "query": "x"})
	if code, message, _ := errorsx.PublicDetails(err); code != CodeInvalidRequest || !strings.Contains(message, "query") {
		t.Fatalf("an unknown argument = %v", err)
	}
	_, err = call(t, f, "ticket_move", map[string]any{})
	refused(t, err, CodeInvalidRequest, `There is no tool named "ticket_move". The tracker tools are: ticket_find, ticket_read.`)
}

// A search pages 20 at a time through next, which the answer carries
// only while more exists, and which takes the same text again.
func TestFindPagesThroughNext(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{jiraSource}}
	f.search = func(_ string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		if in.Page == "" {
			page := cards("WEB", in.Limit)
			page[0].Assignee = &trackerapp.UserView{ID: "acc-user1", Name: "User 1"}
			return trackerapp.SearchPageView{Cards: page, Next: "page-2"}, nil
		}
		return trackerapp.SearchPageView{Cards: cards("WEB", 3)}, nil
	}
	answer, err := call(t, f, "ticket_find", map[string]any{"text": " safari login "})
	if err != nil {
		t.Fatal(err)
	}
	first := decodeAs[findAnswer](t, answer)
	if len(first.Tickets) != pageSize || first.Next == "" {
		t.Fatalf("the first page = %s", answer)
	}
	if !strings.HasPrefix(answer, `{"tickets":[{"key":"WEB-1","title":"Title","status":"To Do","assignee":"User 1"},{"key":"WEB-2","title":"Title","status":"To Do"},`) {
		t.Fatalf("the first page's rows = %s", answer)
	}
	answer, err = call(t, f, "ticket_find", map[string]any{"text": "safari login", "next": first.Next})
	if err != nil {
		t.Fatal(err)
	}
	if last := decodeAs[findAnswer](t, answer); len(last.Tickets) != 3 || strings.Contains(answer, `"next"`) {
		t.Fatalf("the last page = %s", answer)
	}
	want := []string{`search conn-jira "safari login" limit 20 page ""`, `search conn-jira "safari login" limit 20 page "page-2"`}
	if !slices.Equal(f.asked, want) {
		t.Fatalf("asked %q\nwant %q", f.asked, want)
	}

	_, err = call(t, f, "ticket_find", map[string]any{"text": "another search", "next": first.Next})
	refused(t, err, CodeInvalidRequest, "next is from a search for other text; leave it out to search for this text.")
	_, err = call(t, f, "ticket_find", map[string]any{"text": "safari login", "next": "not-a-cursor"})
	refused(t, err, CodeInvalidRequest, "next is not one these tools gave; leave it out to start again.")
	_, err = call(t, f, "ticket_find", map[string]any{"text": "  "})
	refused(t, err, CodeInvalidRequest, "text is required.")
	if len(f.asked) != 2 {
		t.Fatalf("refused calls asked the tracker: %q", f.asked)
	}
}

// With two connections, a page takes what the first has and goes on to
// the second only once the first has no more; a connection that fails
// with a refusal the model can read is named in the answer while the
// other is searched, and the call fails only when every one it asked
// failed.
func TestFindGoesFromOneSourceToTheNext(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{jiraSource, linearSource}}
	f.search = func(connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		if connection == jiraSource.ID {
			return trackerapp.SearchPageView{Cards: cards("WEB", 5)}, nil
		}
		return trackerapp.SearchPageView{Cards: cards("ENG", in.Limit)}, nil
	}
	answer, err := call(t, f, "ticket_find", map[string]any{"text": "login"})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeAs[findAnswer](t, answer)
	if len(got.Tickets) != pageSize || got.Tickets[4].Key != "WEB-5" || got.Tickets[5].Key != "ENG-1" || got.Next != "" {
		t.Fatalf("the page across two sources = %s", answer)
	}
	if want := []string{`search conn-jira "login" limit 20 page ""`, `search conn-linear "login" limit 15 page ""`}; !slices.Equal(f.asked, want) {
		t.Fatalf("asked %q\nwant %q", f.asked, want)
	}

	off := errorsx.Public("tracker_connection_off", "this tracker connection is turned off; turn it on in the Issues settings, then try again", trackerapp.ErrDisabled)
	f.search = func(connection string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		if connection == jiraSource.ID {
			return trackerapp.SearchPageView{}, off
		}
		return trackerapp.SearchPageView{Cards: cards("ENG", 2)}, nil
	}
	answer, err = call(t, f, "ticket_find", map[string]any{"text": "login"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"tickets":[{"key":"ENG-1","title":"Title","status":"To Do"},{"key":"ENG-2","title":"Title","status":"To Do"}],` +
		`"unsearched":[{"site":"example.atlassian.net","error":"this tracker connection is turned off; turn it on in the Issues settings, then try again"}]}`; answer != want {
		t.Fatalf("a page with one source failing = %s\nwant %s", answer, want)
	}

	f.search = func(string, trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		return trackerapp.SearchPageView{}, off
	}
	_, err = call(t, f, "ticket_find", map[string]any{"text": "login"})
	refused(t, err, "tracker_connection_off", "example.atlassian.net: this tracker connection is turned off; turn it on in the Issues settings, then try again")

	broken := errors.New("store: disk I/O error")
	f.search = func(string, trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		return trackerapp.SearchPageView{}, broken
	}
	if _, err = call(t, f, "ticket_find", map[string]any{"text": "login"}); !errors.Is(err, broken) {
		t.Fatalf("a failure the model cannot act on = %v", err)
	}
	if _, _, public := errorsx.PublicDetails(err); public {
		t.Fatalf("a failure the model cannot act on was public: %v", err)
	}
}

// A search continued on a connection turned off since is refused, and a
// computer with no connection says how to get one.
func TestFindWithoutItsSource(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{sources: []trackerapp.TicketSource{jiraSource}}
	f.search = func(string, trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		return trackerapp.SearchPageView{Cards: cards("WEB", 20), Next: "page-2"}, nil
	}
	answer, err := call(t, f, "ticket_find", map[string]any{"text": "login"})
	if err != nil {
		t.Fatal(err)
	}
	f.sources = []trackerapp.TicketSource{linearSource}
	_, err = call(t, f, "ticket_find", map[string]any{"text": "login", "next": decodeAs[findAnswer](t, answer).Next})
	refused(t, err, CodeInvalidRequest, "next continues on a connection that is no longer on; leave it out to search again.")
	f.sources = nil
	_, err = call(t, f, "ticket_find", map[string]any{"text": "login"})
	refused(t, err, CodeNoConnection, "This computer has no Jira or Linear connection turned on. The user adds or turns one on in the Issues settings.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-1"})
	refused(t, err, CodeNoConnection, "This computer has no Jira or Linear connection turned on. The user adds or turns one on in the Issues settings.")
}

func detail(key string) trackerapp.DetailView {
	return trackerapp.DetailView{Card: trackerapp.CardView{ID: "10012", Key: key, Title: "Login fails on Safari", Status: trackerapp.StatusView{ID: "3", Name: "In Progress"}},
		URL: "https://example.atlassian.net/browse/" + key}
}

// A read with no key reads the thread's linked ticket; a key the link
// names reads it with no lookup; any other key is looked up on each
// connection in turn until one finds it, and one none finds is refused
// naming every site asked.
func TestReadResolvesAKey(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource, linearSource},
		lookup:  map[string]map[string]string{"conn-linear": {"ENG-4": "uuid-4"}},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": detail("WEB-12"), "conn-linear/uuid-4": detail("ENG-4")},
	}
	_, err := call(t, f, "ticket_read", map[string]any{})
	refused(t, err, CodeNoTicket, "This thread is not linked to a ticket; name one with key.")

	f.link = &trackerapp.LinkedTicket{Connection: "conn-jira", Site: "example.atlassian.net", Issue: "10012", Key: "WEB-12"}
	for _, args := range []map[string]any{{}, {"key": "web-12"}} {
		f.asked = nil
		answer, err := call(t, f, "ticket_read", args)
		if err != nil || !strings.HasPrefix(answer, `{"key":"WEB-12",`) || !slices.Equal(f.asked, []string{"detail conn-jira 10012"}) {
			t.Fatalf("a read of the linked ticket with %v = %s, %v; asked %q", args, answer, err, f.asked)
		}
	}

	f.asked = nil
	answer, err := call(t, f, "ticket_read", map[string]any{"key": "ENG-4"})
	if err != nil || !strings.HasPrefix(answer, `{"key":"ENG-4",`) {
		t.Fatalf("a read by key = %s, %v", answer, err)
	}
	if want := []string{"lookup conn-jira ENG-4", "lookup conn-linear ENG-4", "detail conn-linear uuid-4"}; !slices.Equal(f.asked, want) {
		t.Fatalf("asked %q\nwant %q", f.asked, want)
	}
	_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-99"})
	refused(t, err, CodeNotFound, "No ticket WEB-99 on example.atlassian.net or example.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": strings.Repeat("A", 60) + "-12345"})
	refused(t, err, CodeInvalidRequest, "key is longer than any ticket key.")

	// A link this computer cannot read refuses a read of it, and leaves a
	// read of another key to the connections.
	f.linkErr = &trackerapp.LinkRefusal{Code: trackerapp.LinkConnectionOff, Message: "the Jira connection to example.atlassian.net is turned off; turn it on in the Issues settings, then read this thread's ticket WEB-12 again"}
	_, err = call(t, f, "ticket_read", map[string]any{})
	refused(t, err, trackerapp.LinkConnectionOff, "the Jira connection to example.atlassian.net is turned off; turn it on in the Issues settings, then read this thread's ticket WEB-12 again")
	if answer, err := call(t, f, "ticket_read", map[string]any{"key": "ENG-4"}); err != nil || !strings.HasPrefix(answer, `{"key":"ENG-4",`) {
		t.Fatalf("a read by key beside a refused link = %s, %v", answer, err)
	}

	// A connection that cannot answer may hold the key: when no other has
	// it, its refusal is the answer.
	f.linkErr = nil
	f.lookupErr = map[string]error{"conn-jira": errorsx.Public("tracker_failed_network", "Jira could not be reached; check the network, then try again", nil)}
	_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-99"})
	refused(t, err, "tracker_failed_network", "example.atlassian.net: Jira could not be reached; check the network, then try again")
}

// A ticket the tracker no longer returns is refused saying so, for the
// linked ticket and for a key.
func TestReadOfATicketGone(t *testing.T) {
	t.Parallel()
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource},
		lookup:  map[string]map[string]string{"conn-jira": {"WEB-7": "10007"}},
		link:    &trackerapp.LinkedTicket{Connection: "conn-jira", Site: "example.atlassian.net", Issue: "10012", Key: "WEB-12"},
	}
	_, err := call(t, f, "ticket_read", map[string]any{})
	refused(t, err, CodeNotFound, "WEB-12, the ticket this thread is linked to, is not on example.atlassian.net any more, or the user's account cannot see it.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-7"})
	refused(t, err, CodeNotFound, "No ticket WEB-7 on example.atlassian.net.")
}

// A read shows each filled field once, in the tracker's order, and leaves
// out what the ticket does not have.
func TestReadShowsATicket(t *testing.T) {
	t.Parallel()
	d := detail("WEB-12")
	d.Card.Type = &trackerapp.IssueTypeView{ID: "1", Name: "Bug"}
	d.Card.Assignee = &trackerapp.UserView{ID: "acc-user1", Name: "User 1"}
	d.Card.Priority = &trackerapp.ChoiceView{ID: "2", Name: "High"}
	d.Card.Labels = []trackerapp.ChoiceView{{ID: "frontend", Name: "frontend"}}
	d.Card.Parent = &trackerapp.ParentView{ID: "10004", Key: "WEB-4", Title: "Sign-in rework"}
	d.Description = "Steps: open /login & submit <form>."
	d.Fields = []trackerapp.FieldView{
		{ID: "components", Name: "Components", List: true, Value: trackerapp.FieldValueView{Kind: "choice", Choices: []trackerapp.ChoiceView{{ID: "1", Name: "Web"}}}},
		{ID: "customfield_1", Name: "Team", Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{"Web"}}},
		{ID: "customfield_2", Name: "Team", Value: trackerapp.FieldValueView{Kind: "number", Numbers: []float64{3}}},
		{ID: "duedate", Name: "Due date", Value: trackerapp.FieldValueView{}},
		{ID: "customfield_3", Name: "Reviewed", Value: trackerapp.FieldValueView{Kind: "time", Times: []int64{1_764_064_320_000}}},
	}
	d.Links = []trackerapp.LinkView{{ID: "1", Type: "Blocks", Relation: "blocks", Issue: trackerapp.LinkedIssueView{ID: "10015", Key: "WEB-15", Title: "Release 2.4", Status: trackerapp.StatusView{Name: "To Do"}}}}
	d.Attachments = []trackerapp.AttachmentView{{ID: "1", Name: "safari-console.png"}}
	d.Comments = []trackerapp.CommentView{{ID: "c1", Author: trackerapp.UserView{Name: "User 2"}, Body: "Repro on 18.1 too.", Created: 1_764_000_300_000, Internal: true}}
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource},
		link:    &trackerapp.LinkedTicket{Connection: "conn-jira", Site: "example.atlassian.net", Issue: "10012", Key: "WEB-12"},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": d},
	}
	answer, err := call(t, f, "ticket_read", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"WEB-12","title":"Login fails on Safari","status":"In Progress","type":"Bug","assignee":"User 1","priority":"High","labels":["frontend"],` +
		`"parent":"WEB-4 Sign-in rework","url":"https://example.atlassian.net/browse/WEB-12","description":"Steps: open /login \u0026 submit \u003cform\u003e.",` +
		`"fields":{"Components":["Web"],"Team (customfield_1)":"Web","Team (customfield_2)":3,"Reviewed":"2025-11-25T09:52Z"},` +
		`"links":[{"relation":"blocks","key":"WEB-15","title":"Release 2.4","status":"To Do"}],"attachments":["safari-console.png"],` +
		`"comments":[{"by":"User 2","at":"2025-11-24T16:05Z","body":"Repro on 18.1 too.","internal":true}]}`
	if answer != want {
		t.Fatalf("the read =\n%s\nwant\n%s", answer, want)
	}
}

// A read shows the 10 newest comments and holds the rest back behind
// more_comments, which pages through the stored first page and then the
// tracker's later pages, and is absent once there are no more.
func TestReadHoldsOlderCommentsBack(t *testing.T) {
	t.Parallel()
	page := func(from, n int) []trackerapp.CommentView {
		out := make([]trackerapp.CommentView, n)
		for i := range out {
			out[i] = trackerapp.CommentView{ID: fmt.Sprint(from + i), Body: fmt.Sprint("comment ", from+i), Created: 1_764_000_000_000}
		}
		return out
	}
	d := detail("WEB-12")
	d.Comments, d.MoreComments = page(1, 12), "12"
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource},
		lookup:  map[string]map[string]string{"conn-jira": {"WEB-12": "10012"}},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": d},
	}
	f.comments = func(_, _, token string) (trackerapp.CommentPageView, error) {
		if token == "" {
			return trackerapp.CommentPageView{Comments: page(1, 12), Next: "12"}, nil
		}
		return trackerapp.CommentPageView{Comments: page(13, 4)}, nil
	}
	answer, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	read := decodeAs[readAnswer](t, answer)
	if len(read.Comments) != commentsShown || read.Comments[9].Body != "comment 10" || read.MoreComments == "" {
		t.Fatalf("the read's comments = %s", answer)
	}
	var bodies []string
	next := read.MoreComments
	for next != "" {
		answer, err := call(t, f, "ticket_read", map[string]any{"more_comments": next})
		if err != nil {
			t.Fatal(err)
		}
		more := decodeAs[commentsAnswer](t, answer)
		for _, c := range more.Comments {
			bodies = append(bodies, c.Body)
		}
		next = more.MoreComments
		if len(bodies) > 20 {
			t.Fatalf("more_comments does not end: %v", bodies)
		}
	}
	if want := []string{"comment 11", "comment 12", "comment 13", "comment 14", "comment 15", "comment 16"}; !slices.Equal(bodies, want) {
		t.Fatalf("older comments = %v\nwant %v", bodies, want)
	}

	_, err = call(t, f, "ticket_read", map[string]any{"more_comments": read.MoreComments, "key": "WEB-12"})
	refused(t, err, CodeInvalidRequest, "more_comments answers those comments alone; leave out key and history.")
	_, err = call(t, f, "ticket_read", map[string]any{"more_comments": "garbage!"})
	refused(t, err, CodeInvalidRequest, "more_comments is not one these tools gave; leave it out to start again.")

	// A first page of no more than 10 with a later page hands over the
	// tracker's token.
	d.Comments, d.MoreComments = page(1, 3), "12"
	f.details["conn-jira/10012"] = d
	answer, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	f.asked = nil
	if _, err := call(t, f, "ticket_read", map[string]any{"more_comments": decodeAs[readAnswer](t, answer).MoreComments}); err != nil {
		t.Fatal(err)
	}
	if want := []string{`comments conn-jira 10012 page "12"`}; !slices.Equal(f.asked, want) {
		t.Fatalf("asked %q\nwant %q", f.asked, want)
	}
	d.MoreComments = ""
	f.details["conn-jira/10012"] = d
	if answer, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"}); err != nil || strings.Contains(answer, "more_comments") {
		t.Fatalf("a read with every comment shown = %s, %v", answer, err)
	}
}

// With history, a read adds the 50 newest changes, newest first, with who
// made each and when, and counts the older ones it leaves out.
func TestReadAddsHistory(t *testing.T) {
	t.Parallel()
	d := detail("WEB-12")
	for i := range 53 {
		e := trackerapp.EventView{At: 1_764_000_000_000 + int64(i)*60_000, Field: "status", From: "To Do", To: fmt.Sprint("step ", i)}
		if i%2 == 0 {
			e.Author = &trackerapp.UserView{ID: "acc-user1", Name: "User 1"}
		}
		d.History = append(d.History, e)
	}
	d.HistoryFrom = 1_763_000_000_000
	f := &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource},
		link:    &trackerapp.LinkedTicket{Connection: "conn-jira", Site: "example.atlassian.net", Issue: "10012", Key: "WEB-12"},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": d},
	}
	answer, err := call(t, f, "ticket_read", map[string]any{})
	if err != nil || strings.Contains(answer, "history") {
		t.Fatalf("a read without history = %s, %v", answer, err)
	}
	answer, err = call(t, f, "ticket_read", map[string]any{"history": true})
	if err != nil {
		t.Fatal(err)
	}
	read := decodeAs[readAnswer](t, answer)
	if len(read.History) != historyShown || read.OlderHistory != 3 || read.HistorySince != "2025-11-13T02:13Z" {
		t.Fatalf("history = %d changes, %d older, since %q", len(read.History), read.OlderHistory, read.HistorySince)
	}
	if first := read.History[0]; first != (readChange{At: "2025-11-24T16:52Z", By: "User 1", Field: "status", From: "To Do", To: "step 52"}) || read.History[1].By != "" || read.History[49].To != "step 3" {
		t.Fatalf("history = %+v ... %+v", read.History[0], read.History[49])
	}
}
