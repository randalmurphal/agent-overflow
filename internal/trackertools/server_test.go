package trackertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackerdash"
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
	// lookupErrAfter is how many lookups answer before lookupErr does.
	lookupErrAfter int
	// honorCtx, when set, makes a detail read fail on an ended context, as
	// the service's does.
	honorCtx bool
	details  map[string]trackerapp.DetailView // connection/issue
	// onDetail, when set, runs as each detail read starts; onLookup as
	// each key lookup does.
	onDetail func()
	onLookup func()
	comments func(connection, issue, page string) (trackerapp.CommentPageView, error)
	asked    []string

	// keyed are the tickets the mirrors hold of a key; boards the
	// mirrors' boards, by connection/board; cards the mirrors' cards, by
	// connection/issue.
	keyed  map[string][]trackerapp.KeyedTicket
	boards map[string]trackerapp.BoardSummary
	// boardReads counts the board reads, by connection/board.
	boardReads map[string]int
	cards      map[string]trackerapp.CardView
	// offBoard are the rows, by connection/issue/board, a rows read no
	// longer finds (IssueRows), though the mirror still holds them.
	offBoard map[string]bool
	// mirrorCards counts the mirror's card reads (MirrorIssue).
	mirrorCards int
	types       []trackerapp.IssueTypeView
	fields      []trackerapp.FieldView
	// write answers each write; writes are those made, as name connection
	// input, and agents the agent each carried.
	write  func(name string, in any) (trackerapp.WriteView, error)
	writes []string
	agents []context.Context
	// ends counts the write sessions ended.
	ends int
	// try answers a widget's count; tries are those asked, as project
	// connection/board width show thread.
	try   func(w trackerapp.KeptWidget) (trackerdash.Tried, error)
	tries []string
	// time answers ReadTime by connection; draft answers DraftTime, and
	// drafts are the drafts asked, by connection.
	time   map[string]trackerapp.WorklogDays
	draft  func(connection string, in trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error)
	drafts []string
	// open answers OpenTimeEntries by connection; post answers PostTime,
	// and posts are the posts asked, by connection.
	open  map[string][]trackerapp.TimeEntryView
	post  func(connection string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error)
	posts []string
	// postCtx is each post's context error as the post began.
	postCtx []error
}

func (f *fakeTracker) OpenTimeEntries(connection string, days []string) ([]trackerapp.TimeEntryView, error) {
	f.asked = append(f.asked, "open time "+connection+" "+strings.Join(days, " "))
	got, ok := f.open[connection]
	if !ok {
		return nil, errorsx.Public("tracker_connection_unverified", "this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings", nil)
	}
	return slices.DeleteFunc(slices.Clone(got), func(e trackerapp.TimeEntryView) bool { return !slices.Contains(days, e.Day) }), nil
}

func (f *fakeTracker) PostTime(ctx context.Context, connection string, in trackerapp.PostTimeRequest) (trackerapp.PostTimeView, error) {
	raw, _ := json.Marshal(in)
	f.posts = append(f.posts, connection+" "+string(raw))
	f.postCtx = append(f.postCtx, ctx.Err())
	if f.post == nil {
		return postedAsRead(f.open[connection], in), nil
	}
	return f.post(connection, in)
}

// postedAsRead is a post of in that posts each entry of open it names
// that a post sends, and gives each other one the reason it is not sent.
func postedAsRead(open []trackerapp.TimeEntryView, in trackerapp.PostTimeRequest) trackerapp.PostTimeView {
	var out trackerapp.PostTimeView
	for _, ref := range in.Entries {
		o := trackerapp.PostOutcome{ID: ref.ID, State: trackerapp.OutcomePosted}
		if i := slices.IndexFunc(open, func(e trackerapp.TimeEntryView) bool { return e.ID == ref.ID }); i >= 0 && trackerapp.NotSentWhy(open[i]) != "" {
			o = trackerapp.NotSentOutcome(open[i])
		}
		if o.State == trackerapp.OutcomePosted {
			out.Posted++
		}
		out.Outcomes = append(out.Outcomes, o)
	}
	return out
}

func (f *fakeTracker) TryWidget(_ context.Context, project, connection, board string, w trackerapp.KeptWidget, thread string, show bool) (trackerdash.Tried, error) {
	f.tries = append(f.tries, fmt.Sprintf("%s %s/%s %d %v %s", project, connection, board, w.Width, show, thread))
	return f.try(w)
}

func (f *fakeTracker) ReadTime(_ context.Context, connection, from, to string) (trackerapp.WorklogDays, error) {
	f.asked = append(f.asked, "time "+connection+" "+from+" "+to)
	got, ok := f.time[connection]
	if !ok {
		return got, errorsx.Public("tracker_connection_unverified", "this connection has no verified account, so whose time it is is not known; check the connection in the Issues settings", nil)
	}
	return got, nil
}

func (f *fakeTracker) DraftTime(connection string, in trackerapp.TimeDraftRequest) (trackerapp.TimeDraftView, error) {
	raw, _ := json.Marshal(in.Entries)
	f.drafts = append(f.drafts, connection+" "+string(raw))
	if f.draft == nil {
		return trackerapp.TimeDraftView{Author: "acc-user1", Made: []trackerapp.TimeEntryView{}, Logged: []trackerapp.LoggedDay{}}, nil
	}
	return f.draft(connection, in)
}

func (f *fakeTracker) TicketsOfKey(key string) ([]trackerapp.KeyedTicket, error) {
	return f.keyed[key], nil
}

func (f *fakeTracker) BoardSummary(connection, board string) (trackerapp.BoardSummary, error) {
	if f.boardReads == nil {
		f.boardReads = map[string]int{}
	}
	f.boardReads[connection+"/"+board]++
	b, ok := f.boards[connection+"/"+board]
	if !ok {
		return b, fmt.Errorf("%w: board %s", trackerapp.ErrNotMirrored, board)
	}
	return b, nil
}

// MirrorIssue answers the mirror's card, with the boards of the issue's
// detail's rows.
func (f *fakeTracker) MirrorIssue(connection, issue string) (trackerapp.IssueView, bool, error) {
	f.mirrorCards++
	c, ok := f.cards[connection+"/"+issue]
	v := trackerapp.IssueView{Card: c}
	for _, r := range f.details[connection+"/"+issue].Rows {
		v.Boards = append(v.Boards, trackerapp.IssueRowView{Board: r.Board, Estimate: r.Estimate})
	}
	return v, ok, nil
}

func (f *fakeTracker) IssueTypes(_ context.Context, connection, project string) ([]trackerapp.IssueTypeView, error) {
	f.asked = append(f.asked, "types "+connection+" "+project)
	return f.types, nil
}

func (f *fakeTracker) CreateFields(_ context.Context, connection, project, issueType string) ([]trackerapp.FieldView, error) {
	f.asked = append(f.asked, "create fields "+connection+" "+project+" "+issueType)
	return f.fields, nil
}

func (f *fakeTracker) written(ctx context.Context, name, connection string, in any) (trackerapp.WriteView, error) {
	raw, _ := json.Marshal(in)
	f.writes = append(f.writes, name+" "+connection+" "+string(raw))
	f.agents = append(f.agents, ctx)
	if f.write == nil {
		return trackerapp.WriteView{Parts: []trackerapp.PartView{{Step: name, State: "applied"}}}, nil
	}
	return f.write(name, in)
}

func (f *fakeTracker) Create(ctx context.Context, connection string, in trackerapp.CreateRequest) (trackerapp.WriteView, error) {
	return f.written(ctx, "create", connection, in)
}

func (f *fakeTracker) Edit(ctx context.Context, connection string, in trackerapp.EditRequest) (trackerapp.WriteView, error) {
	return f.written(ctx, "edit", connection, in)
}

func (f *fakeTracker) Move(ctx context.Context, connection string, in trackerapp.MoveRequest) (trackerapp.WriteView, error) {
	return f.written(ctx, "move", connection, in)
}

func (f *fakeTracker) SetEstimate(ctx context.Context, connection string, in trackerapp.EstimateRequest) (trackerapp.WriteView, error) {
	return f.written(ctx, "estimate", connection, in)
}

func (f *fakeTracker) MoveToSprint(ctx context.Context, connection string, in trackerapp.SprintMoveRequest) (trackerapp.WriteView, error) {
	return f.written(ctx, "sprint_move", connection, in)
}

func (f *fakeTracker) AddComment(ctx context.Context, connection string, in trackerapp.CommentRequest) (trackerapp.WriteView, error) {
	return f.written(ctx, "comment", connection, in)
}

// fakeHost answers the writing tools' questions about the thread.
type fakeHost struct {
	readOnly bool
	// noAsk is a project whose Agents settings let a change to several
	// tickets go without the person's approval.
	noAsk bool
	// onPolicy runs before each WritePolicy read with its count, from 1.
	onPolicy func(n int)
	policies int
	// approve answers an approval; nil approves.
	approve   func(Approval) error
	approvals []Approval
	rows      []WriteRow
	timeRows  []TimeRow
	postRows  []TimePostRow
	// createBoards are the boards a create can go through, by project;
	// readingProject, when set, runs as a create reads them.
	createBoards   map[string][]ListedBoard
	readingProject func()
	// project and boards are the thread's project and the boards it lists
	// beside createBoards'.
	project string
	boards  []ListedBoard
	// ended are the sessions the host no longer holds.
	ended map[string]bool
}

func (h *fakeHost) WritePolicy(string) (WritePolicy, error) {
	h.policies++
	if h.onPolicy != nil {
		h.onPolicy(h.policies)
	}
	if h.readOnly {
		return WritePolicy{}, errorsx.Public("tracker_read_only", "This thread is read-only, so it cannot change tickets or log time; ask the user to change its access.", nil)
	}
	return WritePolicy{AskSeveral: !h.noAsk}, nil
}

func (h *fakeHost) Approve(_ context.Context, _ string, a Approval) error {
	h.approvals = append(h.approvals, a)
	if h.approve == nil {
		return nil
	}
	return h.approve(a)
}

func (h *fakeHost) Wrote(_ string, row WriteRow) { h.rows = append(h.rows, row) }

func (h *fakeHost) Drafted(_ string, row TimeRow) { h.timeRows = append(h.timeRows, row) }

func (h *fakeHost) Posted(_ string, row TimePostRow) { h.postRows = append(h.postRows, row) }

func (h *fakeHost) Boards(string) (string, []ListedBoard, error) {
	if h.readingProject != nil {
		h.readingProject()
	}
	var out []ListedBoard
	for _, project := range slices.Sorted(maps.Keys(h.createBoards)) {
		for _, b := range h.createBoards[project] {
			b.TrackerProject = project
			out = append(out, b)
		}
	}
	return h.project, append(out, h.boards...), nil
}

func (h *fakeHost) Live(_, session string) bool { return !h.ended[session] }

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
	if f.onLookup != nil {
		f.onLookup()
	}
	if err := f.lookupErr[connection]; err != nil && f.lookupErrAfter == 0 {
		return trackerapp.LookupView{}, err
	}
	f.lookupErrAfter = max(f.lookupErrAfter-1, 0)
	var out trackerapp.LookupView
	for _, k := range keys {
		if issue, ok := f.lookup[connection][k]; ok {
			// The card as the tracker holds it now, as a detail read gives it.
			card := f.details[connection+"/"+issue].Card
			card.ID, card.Key = issue, cmpOr(card.Key, k)
			out.Found = append(out.Found, trackerapp.KeyMatchView{Key: k, Card: card})
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

func (f *fakeTracker) IssueDetail(ctx context.Context, connection, issue string) (trackerapp.DetailView, error) {
	f.asked = append(f.asked, "detail "+connection+" "+issue)
	if f.onDetail != nil {
		f.onDetail()
	}
	if err := ctx.Err(); f.honorCtx && err != nil {
		return trackerapp.DetailView{}, err
	}
	d, ok := f.details[connection+"/"+issue]
	if !ok {
		return trackerapp.DetailView{}, fmt.Errorf("%w: issue %s", trackerapp.ErrIssueNotFound, issue)
	}
	return d, nil
}

// IssueDetails asks each issue's detail as IssueDetail does.
func (f *fakeTracker) IssueDetails(ctx context.Context, connection string, issues []string) ([]trackerapp.TicketRead, error) {
	out := make([]trackerapp.TicketRead, len(issues))
	for i, issue := range issues {
		out[i].View, out[i].Err = f.IssueDetail(ctx, connection, issue)
		if err := ctx.Err(); f.honorCtx && err != nil {
			return nil, err
		}
	}
	return out, nil
}

// IssueCards answers each issue's card and rows from its detail, asked as
// "card connection issue".
func (f *fakeTracker) IssueCards(_ context.Context, connection string, issues []string) ([]trackerapp.TicketCardRead, error) {
	out := make([]trackerapp.TicketCardRead, len(issues))
	for i, issue := range issues {
		f.asked = append(f.asked, "card "+connection+" "+issue)
		d, ok := f.details[connection+"/"+issue]
		if !ok {
			out[i].Err = fmt.Errorf("%w: issue %s", trackerapp.ErrIssueNotFound, issue)
			continue
		}
		out[i].View = trackerapp.TicketCardView{Card: d.Card, Rows: d.Rows}
	}
	return out, nil
}

// IssueRows answers as IssueCards does, asked as "rows connection issue
// board", and fails one whose detail has no row on its board with
// trackerapp.ErrNotOnBoard.
func (f *fakeTracker) IssueRows(ctx context.Context, connection string, rows []trackerapp.IssueRow) ([]trackerapp.TicketCardRead, error) {
	out := make([]trackerapp.TicketCardRead, len(rows))
	for i, row := range rows {
		n := len(f.asked)
		got, err := f.IssueCards(ctx, connection, []string{string(row.Issue)})
		if err != nil {
			return nil, err
		}
		f.asked[n] = "rows" + strings.TrimPrefix(f.asked[n], "card") + " " + string(row.Board)
		out[i] = got[0]
		on := slices.ContainsFunc(got[0].View.Rows, func(r trackerapp.TicketRowView) bool { return r.Board == string(row.Board) })
		if got[0].Err == nil && (!on || f.offBoard[connection+"/"+string(row.Issue)+"/"+string(row.Board)]) {
			out[i] = trackerapp.TicketCardRead{Err: fmt.Errorf("%w: issue %s on board %s", trackerapp.ErrNotOnBoard, row.Issue, row.Board)}
		}
	}
	return out, nil
}

// EndWrites counts the sessions ended.
func (f *fakeTracker) EndWrites(context.Context) error {
	f.ends++
	return nil
}

func (f *fakeTracker) Comments(_ context.Context, connection, issue, page string) (trackerapp.CommentPageView, error) {
	f.asked = append(f.asked, fmt.Sprintf("comments %s %s page %q", connection, issue, page))
	return f.comments(connection, issue, page)
}

var (
	jiraSource   = trackerapp.TicketSource{ID: "conn-jira", Site: "example.atlassian.net"}
	linearSource = trackerapp.TicketSource{ID: "conn-linear", Site: "example"}
)

// testCaller is the thread and session every test calls as.
var testCaller = Caller{Thread: "thread-1", Session: "session-1"}

// call runs one tool as thread-1 and answers its result as the JSON the
// transport sends, or its error.
func call(t *testing.T, f *fakeTracker, name string, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := New(f, &fakeHost{}).Call(t.Context(), testCaller, name, raw)
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

// Every schema is closed, with required an array, so a misspelt argument
// is a refusal the model reads, and the names are the table's.
func TestToolSchemasAreClosed(t *testing.T) {
	t.Parallel()
	tools := New(nil, nil).Tools(false)
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
	if !slices.Equal(names, []string{"ticket_find", "ticket_read", "ticket_create", "ticket_update", "dashboard_widget"}) || !slices.Equal(names, ToolNames(false)) {
		t.Fatalf("tools = %v", names)
	}
	f := &fakeTracker{sources: []trackerapp.TicketSource{jiraSource}}
	_, err := call(t, f, "ticket_find", map[string]any{"text": "login", "query": "x"})
	if code, message, _ := errorsx.PublicDetails(err); code != CodeInvalidRequest || !strings.Contains(message, "query") {
		t.Fatalf("an unknown argument = %v", err)
	}
	_, err = call(t, f, "ticket_move", map[string]any{})
	refused(t, err, CodeInvalidRequest, `There is no tool named "ticket_move". The tracker tools are: ticket_find, ticket_read, ticket_create, ticket_update, dashboard_widget.`)
}

// A search pages 20 at a time through next, which the answer carries
// only while more exists, and which takes the same text again.
// The filters reach every source's search as the tracker-neutral
// request: a status as its name, whatever it reads, a group as the group,
// not_done among them, and the next page with the same filters.
func TestFindAsksEachSourceTheFilters(t *testing.T) {
	t.Parallel()
	var asked []trackerapp.SearchRequest
	f := &fakeTracker{sources: []trackerapp.TicketSource{jiraSource}}
	f.search = func(_ string, in trackerapp.SearchRequest) (trackerapp.SearchPageView, error) {
		asked = append(asked, in)
		if in.Page == "" {
			return trackerapp.SearchPageView{Cards: cards("WEB", in.Limit), Next: "page-2"}, nil
		}
		return trackerapp.SearchPageView{Cards: cards("WEB", 1)}, nil
	}
	for _, c := range []struct {
		args map[string]any
		want trackerapp.SearchRequest
	}{
		{map[string]any{"mine": true}, trackerapp.SearchRequest{Mine: true, Limit: pageSize}},
		{map[string]any{"status": " In Progress "}, trackerapp.SearchRequest{Status: "In Progress", Limit: pageSize}},
		{map[string]any{"group": "in_progress", "sprint": "active"}, trackerapp.SearchRequest{Group: "in_progress", ActiveSprint: true, Limit: pageSize}},
		{map[string]any{"group": "backlog"}, trackerapp.SearchRequest{Group: "backlog", Limit: pageSize}},
		{map[string]any{"group": "not_done", "mine": true}, trackerapp.SearchRequest{Mine: true, Group: trackerapp.GroupNotDone, Limit: pageSize}},
		{map[string]any{"status": "In Review", "text": " login "}, trackerapp.SearchRequest{Text: "login", Status: "In Review", Limit: pageSize}},
	} {
		asked = nil
		answer, err := call(t, f, "ticket_find", c.args)
		if err != nil {
			t.Fatalf("ticket_find %v: %v", c.args, err)
		}
		next := c.args
		next["next"] = decodeAs[findAnswer](t, answer).Next
		if _, err := call(t, f, "ticket_find", next); err != nil {
			t.Fatalf("ticket_find %v: %v", next, err)
		}
		page := c.want
		page.Page = "page-2"
		if want := []trackerapp.SearchRequest{c.want, page}; !slices.Equal(asked, want) {
			t.Errorf("ticket_find %v asked %+v\nwant %+v", c.args, asked, want)
		}
	}
	_, err := call(t, f, "ticket_find", map[string]any{"sprint": "Sprint 1"})
	refused(t, err, CodeInvalidRequest, `sprint takes "active" only.`)
	_, err = call(t, f, "ticket_find", map[string]any{"mine": false, "status": " "})
	refused(t, err, CodeInvalidRequest, "Give text, mine, status, group or sprint.")
	_, err = call(t, f, "ticket_find", map[string]any{"group": "in progress"})
	refused(t, err, CodeInvalidRequest, "group takes backlog, todo, in_progress, done, canceled, not_done.")
	_, err = call(t, f, "ticket_find", map[string]any{"status": "In Review", "group": "in_progress"})
	refused(t, err, CodeInvalidRequest, "Give status or group, not both.")
}

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

	// A next is for the search that gave it: other text, or the same text
	// with a filter more, is refused.
	_, err = call(t, f, "ticket_find", map[string]any{"text": "another search", "next": first.Next})
	refused(t, err, CodeInvalidRequest, "next is from another search; leave it out to search with these arguments.")
	for _, other := range []map[string]any{
		{"text": "safari login", "mine": true}, {"text": "safari login", "status": "Done"}, {"text": "safari login", "group": "done"},
		{"text": "safari login", "sprint": "active"},
	} {
		other["next"] = first.Next
		_, err = call(t, f, "ticket_find", other)
		refused(t, err, CodeInvalidRequest, "next is from another search; leave it out to search with these arguments.")
	}
	_, err = call(t, f, "ticket_find", map[string]any{"text": "safari login", "next": "not-a-cursor"})
	refused(t, err, CodeInvalidRequest, "next is not one these tools gave; leave it out to start again.")
	_, err = call(t, f, "ticket_find", map[string]any{"text": "  "})
	refused(t, err, CodeInvalidRequest, "Give text, mine, status, group or sprint.")
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
	if len(got.Tickets) != pageSize || got.Tickets[4].Key != "example.atlassian.net/WEB-5" || got.Tickets[5].Key != "example/ENG-1" || got.Next != "" {
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
	if want := `{"tickets":[{"key":"example/ENG-1","title":"Title","status":"To Do"},{"key":"example/ENG-2","title":"Title","status":"To Do"}],` +
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
// names reads it with no lookup; any other key is looked up on every
// connection, and one none finds is refused naming every site asked. With
// two connections on, every key an answer gives names its site.
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
	for _, args := range []map[string]any{{}, {"key": "web-12"}, {"key": "Example.Atlassian.net/web-12"}} {
		f.asked = nil
		answer, err := call(t, f, "ticket_read", args)
		if err != nil || !strings.HasPrefix(answer, `{"key":"example.atlassian.net/WEB-12",`) || !slices.Equal(f.asked, []string{"detail conn-jira 10012"}) {
			t.Fatalf("a read of the linked ticket with %v = %s, %v; asked %q", args, answer, err, f.asked)
		}
	}

	f.asked = nil
	answer, err := call(t, f, "ticket_read", map[string]any{"key": "ENG-4"})
	if err != nil || !strings.HasPrefix(answer, `{"key":"example/ENG-4",`) {
		t.Fatalf("a read by key = %s, %v", answer, err)
	}
	if want := []string{"lookup conn-jira ENG-4", "lookup conn-linear ENG-4", "detail conn-linear uuid-4"}; !slices.Equal(f.asked, want) {
		t.Fatalf("asked %q\nwant %q", f.asked, want)
	}
	f.asked = nil
	if answer, err := call(t, f, "ticket_read", map[string]any{"key": "example/eng-4"}); err != nil || !strings.HasPrefix(answer, `{"key":"example/ENG-4",`) ||
		!slices.Equal(f.asked, []string{"lookup conn-linear ENG-4", "detail conn-linear uuid-4"}) {
		t.Fatalf("a read by site and key = %s, %v; asked %q", answer, err, f.asked)
	}
	_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-99"})
	refused(t, err, CodeNotFound, "No ticket WEB-99 on example.atlassian.net or example.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": "example/WEB-99"})
	refused(t, err, CodeNotFound, "No ticket WEB-99 on example.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": "other.atlassian.net/WEB-12"})
	refused(t, err, CodeNotFound, "No connection on this computer reaches other.atlassian.net; the sites on are example.atlassian.net or example.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": "example/"})
	refused(t, err, CodeInvalidRequest, "key names a site and no ticket; write it as site/KEY.")
	_, err = call(t, f, "ticket_read", map[string]any{"key": strings.Repeat("A", 124) + "-12345"})
	refused(t, err, CodeInvalidRequest, "key is longer than any ticket key.")

	// A key two connections hold is refused naming both.
	f.lookup["conn-jira"] = map[string]string{"ENG-4": "10004"}
	_, err = call(t, f, "ticket_read", map[string]any{"key": "ENG-4"})
	refused(t, err, CodeAmbiguous, "ENG-4 is on example.atlassian.net and example; name one: example.atlassian.net/ENG-4 or example/ENG-4.")
	delete(f.lookup, "conn-jira")

	// A link this computer cannot read refuses a read of it, and leaves a
	// read of another key to the connections.
	f.linkErr = &trackerapp.LinkRefusal{Code: trackerapp.LinkConnectionOff, Message: "the Jira connection to example.atlassian.net is turned off; turn it on in the Issues settings, then read this thread's ticket WEB-12 again"}
	_, err = call(t, f, "ticket_read", map[string]any{})
	refused(t, err, trackerapp.LinkConnectionOff, "the Jira connection to example.atlassian.net is turned off; turn it on in the Issues settings, then read this thread's ticket WEB-12 again")
	if answer, err := call(t, f, "ticket_read", map[string]any{"key": "ENG-4"}); err != nil || !strings.HasPrefix(answer, `{"key":"example/ENG-4",`) {
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
// linked ticket and for a key, naming its site once whether one connection
// is on or two.
func TestReadOfATicketGone(t *testing.T) {
	t.Parallel()
	for _, sources := range [][]trackerapp.TicketSource{{jiraSource}, {jiraSource, linearSource}} {
		f := &fakeTracker{
			sources: sources,
			lookup:  map[string]map[string]string{"conn-jira": {"WEB-7": "10007"}},
			link:    &trackerapp.LinkedTicket{Connection: "conn-jira", Site: "example.atlassian.net", Issue: "10012", Key: "WEB-12"},
		}
		_, err := call(t, f, "ticket_read", map[string]any{})
		refused(t, err, CodeNotFound, "WEB-12, the ticket this thread is linked to, is not on example.atlassian.net any more, or the user's account cannot see it.")
		_, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-7"})
		refused(t, err, CodeNotFound, "No ticket WEB-7 on example.atlassian.net.")
	}
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

// moreFor is the value of the answer's more whose label starts with
// label, failing when it has none.
func moreFor(t *testing.T, answer readAnswer, label string) string {
	t.Helper()
	for k, v := range answer.More {
		if strings.HasPrefix(k, label) {
			return v
		}
	}
	t.Fatalf("more has no %q: %v", label, slices.Collect(maps.Keys(answer.More)))
	return ""
}

// readAll follows the continuation more names, and each continuation of
// the same part after it, checking every answer against the budget, and
// returns the answers.
func readAll(t *testing.T, f *fakeTracker, more, label string) []readAnswer {
	t.Helper()
	var out []readAnswer
	for more != "" {
		raw, err := call(t, f, "ticket_read", map[string]any{"more": more})
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > answerBudget {
			t.Fatalf("a continuation is %d bytes, over the budget", len(raw))
		}
		answer := decodeAs[readAnswer](t, raw)
		out = append(out, answer)
		more = ""
		for k, v := range answer.More {
			if strings.HasPrefix(k, label) {
				more = v
			}
		}
		if len(out) > 100 {
			t.Fatalf("the continuations of %q do not end", label)
		}
	}
	return out
}

// joined is a text shown in pieces as one: each piece's cut marks
// removed.
func joined(pieces []string) string {
	var b strings.Builder
	for _, p := range pieces {
		b.WriteString(strings.TrimSuffix(strings.TrimPrefix(p, cut), cut))
	}
	return b.String()
}

func longTicketTracker(d trackerapp.DetailView) *fakeTracker {
	return &fakeTracker{
		sources: []trackerapp.TicketSource{jiraSource},
		lookup:  map[string]map[string]string{"conn-jira": {"WEB-12": "10012"}},
		details: map[string]trackerapp.DetailView{"conn-jira/10012": d},
	}
}

// The rest of the description is reachable on its own from the first
// answer, each answer of it as much as fits, and comes whole.
func TestTheRestOfTheDescriptionComesOnItsOwn(t *testing.T) {
	t.Parallel()
	d := longTicket()
	f := longTicketTracker(d)
	raw, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	first := decodeAs[readAnswer](t, raw)
	pieces := []string{first.Description}
	rest := readAll(t, f, moreFor(t, first, "the rest of the description ("), "the rest of the description (")
	for _, a := range rest {
		if a.Key != "WEB-12" || len(a.Comments) > 0 || a.Title != "" {
			t.Fatalf("a description continuation holds more than the key and the description: %+v", a)
		}
		pieces = append(pieces, a.Description)
	}
	if joined(pieces) != d.Description {
		t.Fatal("the description's pieces do not make the description")
	}
	if len(rest) > 10 {
		t.Fatalf("%d continuations for 200 KiB; each should be nearly the budget", len(rest))
	}
}

// The rest of one comment is reachable on its own, and older comments
// come without paging through any comment's rest.
func TestTheRestOfACommentAndOlderCommentsComeOnTheirOwn(t *testing.T) {
	t.Parallel()
	d := longTicket()
	for i := 10; i < 30; i++ {
		d.Comments = append(d.Comments, trackerapp.CommentView{
			ID: fmt.Sprint("c", i), Author: trackerapp.UserView{Name: "User 1"}, Created: 1_764_000_000_000 - int64(i)*60_000, Body: fmt.Sprint("short ", i),
		})
	}
	d.Comments[12].Body = strings.Repeat("A long older comment. ", 1000)
	d.MoreComments = "50"
	f := longTicketTracker(d)
	f.comments = func(_, _, token string) (trackerapp.CommentPageView, error) {
		if token != "50" {
			t.Fatalf("comments page %q read", token)
		}
		return trackerapp.CommentPageView{Comments: []trackerapp.CommentView{{ID: "c50", Body: "on the second page", Created: 1_763_000_000_000}}}, nil
	}
	raw, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	first := decodeAs[readAnswer](t, raw)
	third := readAll(t, f, moreFor(t, first, "the rest of the comment by User 3 "), "the rest of the comment by User 3 ")
	pieces := []string{first.Comments[2].Body}
	for _, a := range third {
		if len(a.Comments) != 1 || a.Comments[0].By != "User 3" || a.Description != "" {
			t.Fatalf("a comment's continuation = %+v", a)
		}
		pieces = append(pieces, a.Comments[0].Body)
	}
	if joined(pieces) != d.Comments[2].Body {
		t.Fatal("comment 3's pieces do not make its body")
	}

	f.asked = nil
	older := readAll(t, f, moreFor(t, first, "older comments (more than 20)"), "older comments")
	var bodies []string
	for _, a := range older {
		for _, c := range a.Comments {
			bodies = append(bodies, c.Body)
		}
		if a.Description != "" || a.Title != "" || len(a.History) > 0 {
			t.Fatalf("an older-comments answer = %+v", a)
		}
	}
	if len(bodies) != 21 || bodies[0] != "short 10" || bodies[20] != "on the second page" ||
		!strings.HasPrefix(bodies[2], "A long older comment.") || !strings.HasSuffix(bodies[2], cut) || len(bodies[2]) > listedComment {
		t.Fatalf("older comments = %d: %.60q ... %.60q", len(bodies), bodies[0], bodies[len(bodies)-1])
	}
	if want := []string{"detail conn-jira 10012", `comments conn-jira 10012 page "50"`}; !slices.Equal(f.asked, want) {
		t.Fatalf("asked %q\nwant %q: one answer of every older comment, each tracker page read once", f.asked, want)
	}
}

// A continuation of a ticket that changed since its answer is refused.
func TestAContinuationOfAChangedTicketIsRefused(t *testing.T) {
	t.Parallel()
	d := longTicket()
	d.Card.Updated = 1_764_000_000_000
	f := longTicketTracker(d)
	raw, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	more := moreFor(t, decodeAs[readAnswer](t, raw), "the rest of the description (")
	d.Card.Updated += 60_000
	f.details["conn-jira/10012"] = d
	_, err = call(t, f, "ticket_read", map[string]any{"more": more})
	refused(t, err, CodeInvalidRequest, "more is from an earlier version of WEB-12, which has changed since; read it again.")
	_, err = call(t, f, "ticket_read", map[string]any{"more": more, "key": "WEB-12"})
	refused(t, err, CodeInvalidRequest, "more answers that part alone; leave out key and history.")
	_, err = call(t, f, "ticket_read", map[string]any{"more": "garbage!"})
	refused(t, err, CodeInvalidRequest, "more is not one these tools gave; leave it out to start again.")
}

// A long field value shows its head in the first answer and its rest on
// its own; fields past the answer's share come as more fields; links and
// attachments past theirs come as more of them; every list continuation
// starts where the answer before ended.
func TestFieldsAndListsComeInParts(t *testing.T) {
	t.Parallel()
	d := detail("WEB-12")
	long := strings.Repeat("Acceptance criterion that the team wrote out. ", 400)
	d.Fields = []trackerapp.FieldView{{ID: "customfield_1", Name: "Acceptance", Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{long}}}}
	for i := range 60 {
		d.Fields = append(d.Fields, trackerapp.FieldView{ID: fmt.Sprint("cf", i), Name: fmt.Sprint("Field ", i),
			Value: trackerapp.FieldValueView{Kind: "text", Texts: []string{strings.Repeat("x", 300)}}})
	}
	for i := range 400 {
		d.Links = append(d.Links, trackerapp.LinkView{Relation: "relates to", Issue: trackerapp.LinkedIssueView{Key: fmt.Sprint("WEB-", 100+i), Title: "A related ticket"}})
		d.Attachments = append(d.Attachments, trackerapp.AttachmentView{Name: fmt.Sprintf("screenshot-%03d.png", i)})
	}
	f := longTicketTracker(d)
	raw, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > answerBudget {
		t.Fatalf("the first answer is %d bytes", len(raw))
	}
	first := decodeAs[readAnswer](t, raw)
	var fields map[string]any
	if err := json.Unmarshal(first.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	head, _ := fields["Acceptance"].(string)
	if len(head) > fieldHead || !strings.HasSuffix(head, cut) {
		t.Fatalf("the long field's head is %d bytes", len(head))
	}
	pieces := []string{head}
	for _, a := range readAll(t, f, moreFor(t, first, "the rest of Acceptance ("), "the rest of Acceptance (") {
		var rest map[string]string
		if err := json.Unmarshal(a.Fields, &rest); err != nil || len(rest) != 1 {
			t.Fatalf("a field's continuation = %s, %v", a.Fields, err)
		}
		pieces = append(pieces, rest["Acceptance"])
	}
	if joined(pieces) != long {
		t.Fatal("the field's pieces do not make its value")
	}
	shown := len(fields)
	for _, a := range readAll(t, f, moreFor(t, first, fmt.Sprintf("%d more fields", 61-shown)), "more fields") {
		var more map[string]string
		if err := json.Unmarshal(a.Fields, &more); err != nil {
			t.Fatal(err)
		}
		if _, ok := more[fmt.Sprint("Field ", shown-1)]; !ok {
			t.Fatalf("more fields does not start at Field %d: %s", shown-1, a.Fields)
		}
		shown += len(more)
	}
	if shown != 61 {
		t.Fatalf("the fields came to %d", shown)
	}
	links := len(first.Links)
	for _, a := range readAll(t, f, moreFor(t, first, fmt.Sprintf("%d more links", 400-links)), "more links") {
		if a.Links[0].Key != fmt.Sprint("WEB-", 100+links) {
			t.Fatalf("more links starts at %s, want WEB-%d", a.Links[0].Key, 100+links)
		}
		links += len(a.Links)
	}
	names := len(first.Attachments)
	for _, a := range readAll(t, f, moreFor(t, first, fmt.Sprintf("%d more attachments", 400-names)), "more attachments") {
		names += len(a.Attachments)
	}
	if links != 400 || names != 400 {
		t.Fatalf("links came to %d and attachments to %d", links, names)
	}
}

// With history, a read adds the newest changes, newest first, with who
// made each and when, each value cut at changeCut; the older changes come
// on their own.
func TestReadAddsHistory(t *testing.T) {
	t.Parallel()
	d := detail("WEB-12")
	for i := range 400 {
		e := trackerapp.EventView{At: 1_764_000_000_000 + int64(i)*60_000, Field: "status", From: "To Do", To: fmt.Sprint("step ", i)}
		if i%2 == 0 {
			e.Author = &trackerapp.UserView{ID: "acc-user1", Name: "User 1"}
		}
		d.History = append(d.History, e)
	}
	d.History[399].To = strings.Repeat("A whole old description. ", 500)
	d.HistoryFrom = 1_763_000_000_000
	f := longTicketTracker(d)
	answer, err := call(t, f, "ticket_read", map[string]any{"key": "WEB-12"})
	if err != nil || strings.Contains(answer, "history") || strings.Contains(answer, "changes") {
		t.Fatalf("a read without history = %s, %v", answer, err)
	}
	answer, err = call(t, f, "ticket_read", map[string]any{"key": "WEB-12", "history": true})
	if err != nil {
		t.Fatal(err)
	}
	read := decodeAs[readAnswer](t, answer)
	if read.HistorySince != "2025-11-13T02:13Z" || len(read.History) < 50 {
		t.Fatalf("history = %d changes since %q", len(read.History), read.HistorySince)
	}
	newest := read.History[0]
	if newest.At != "2025-11-24T22:39Z" || newest.By != "" || len(newest.To) > changeCut || !strings.HasSuffix(newest.To, cut) || read.History[1].By != "User 1" {
		t.Fatalf("the newest changes = %+v, %+v", newest, read.History[1])
	}
	seen := len(read.History)
	for _, a := range readAll(t, f, moreFor(t, read, fmt.Sprintf("%d older changes", 400-seen)), "older changes") {
		if a.History[0].To != fmt.Sprint("step ", 399-seen) {
			t.Fatalf("older changes start at %q, want step %d", a.History[0].To, 399-seen)
		}
		seen += len(a.History)
	}
	if seen != 400 {
		t.Fatalf("the changes came to %d", seen)
	}
}
