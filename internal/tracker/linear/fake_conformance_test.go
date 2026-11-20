package linear

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// The conformance test drives every exported Client method, and so every
// registered operation, against the stateful fake Linear in
// internal/harness/trackerfake.

type conformanceClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *conformanceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *conformanceClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fid is a fixture ID in Linear's lowercase UUID form.
func fid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

var (
	cfUser1, cfUser2, cfUser3              = fid(1), fid(2), fid(3)
	cfTeam                                 = fid(10)
	cfBacklog, cfTodo, cfStarted           = fid(20), fid(21), fid(22)
	cfDone, cfCanceled                     = fid(23), fid(24)
	cfPast, cfActive, cfUpcoming           = fid(30), fid(31), fid(32)
	cfBug, cfFeature                       = fid(40), fid(41)
	cfProject                              = fid(50)
	cfIssue1, cfIssue2, cfIssue3           = fid(60), fid(61), fid(62)
	cfComment, cfAttachment, cfRel         = fid(70), fid(71), fid(72)
	cfStart                                = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	cfDay                                  = 24 * time.Hour
	cfThree                        float64 = 3
)

func conformanceWorkspace() trackerfake.LinearWorkspace {
	at := func(days int) time.Time { return cfStart.Add(time.Duration(days) * cfDay) }
	sub := 1.0
	return trackerfake.LinearWorkspace{
		URLKey: "example", Name: "Example", APIKey: "lin_api_conformance1", Viewer: cfUser1,
		Users: []trackerfake.LinearUser{
			{ID: cfUser1, Name: "User 1", Email: "user1@example.com", Admin: true},
			{ID: cfUser2, Name: "User 2", Email: "user2@example.com"},
			{ID: cfUser3, Name: "User 3", Email: "user3@example.com", Disabled: true},
		},
		Teams: []trackerfake.LinearTeam{{
			ID: cfTeam, Key: "WEB", Name: "Web", Timezone: "UTC", Estimation: "fibonacci",
			CyclesEnabled: true, CycleWeeks: 2, DefaultState: cfTodo, Members: []string{cfUser1, cfUser2},
		}},
		States: []trackerfake.LinearState{
			{ID: cfBacklog, Team: cfTeam, Name: "Backlog", Type: "backlog", Position: 0},
			{ID: cfTodo, Team: cfTeam, Name: "Todo", Type: "unstarted", Position: 1},
			{ID: cfStarted, Team: cfTeam, Name: "In Progress", Type: "started", Position: 2},
			{ID: cfDone, Team: cfTeam, Name: "Done", Type: "completed", Position: 3},
			{ID: cfCanceled, Team: cfTeam, Name: "Canceled", Type: "canceled", Position: 4},
		},
		Cycles: []trackerfake.LinearCycle{
			{ID: cfPast, Team: cfTeam, Number: 1, StartsAt: at(-24), EndsAt: at(-10), CompletedAt: at(-10)},
			{ID: cfActive, Team: cfTeam, Number: 2, StartsAt: at(-10), EndsAt: at(4)},
			{ID: cfUpcoming, Team: cfTeam, Number: 3, StartsAt: at(4), EndsAt: at(18)},
		},
		Labels: []trackerfake.LinearLabel{
			{ID: cfBug, Name: "Bug", Team: cfTeam},
			{ID: cfFeature, Name: "Feature"},
		},
		Projects: []trackerfake.LinearProject{{ID: cfProject, Name: "Website", Teams: []string{cfTeam}, StatusName: "In Progress", StatusType: "started"}},
		Issues: []trackerfake.LinearIssue{
			{
				ID: cfIssue1, Team: cfTeam, Number: 1, Title: "Fix login", State: cfTodo, Assignee: cfUser1, Creator: cfUser1,
				Cycle: cfActive, Project: cfProject, Labels: []string{cfBug}, Priority: 2, Estimate: &cfThree, SortOrder: 100,
				CreatedAt: at(-18), UpdatedAt: at(-1),
				History: []trackerfake.LinearHistory{{CreatedAt: at(-9), Actor: cfUser1, FromState: cfBacklog, ToState: cfTodo}},
			},
			{ID: cfIssue2, Team: cfTeam, Number: 2, Title: "Add search", State: cfBacklog, Creator: cfUser2, SortOrder: 200, CreatedAt: at(-17), UpdatedAt: at(-2)},
			{
				ID: cfIssue3, Team: cfTeam, Number: 3, Title: "Login tests", State: cfStarted, Parent: cfIssue1, Creator: cfUser1,
				SortOrder: 300, SubIssueSortOrder: &sub, CreatedAt: at(-16), UpdatedAt: at(-3), StartedAt: at(-3),
			},
		},
		Comments:    []trackerfake.LinearComment{{ID: cfComment, Issue: cfIssue1, User: cfUser2, Body: "Looks good", CreatedAt: at(-2)}},
		Attachments: []trackerfake.LinearAttachment{{ID: cfAttachment, Issue: cfIssue1, Title: "Pull request 1", URL: "https://example.com/pr/1", Creator: cfUser1}},
		Relations:   []trackerfake.LinearRelation{{ID: cfRel, Type: "blocks", Issue: cfIssue1, Related: cfIssue2}},
	}
}

type conformanceEnv struct {
	e     *trackerfake.Engine
	c     *Client
	clock *conformanceClock
	ctx   context.Context
}

func newConformanceEnv(t *testing.T) *conformanceEnv {
	t.Helper()
	return newFakeEnv(t, conformanceWorkspace())
}

// newFakeEnv is a client of a stateful fake holding w, whose key is the
// conformance workspace's.
func newFakeEnv(t *testing.T, w trackerfake.LinearWorkspace) *conformanceEnv {
	t.Helper()
	clock := &conformanceClock{now: cfStart}
	e := trackerfake.New(trackerfake.Options{Now: clock.Now})
	t.Cleanup(e.Close)
	if err := e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{w}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	c, err := New(Options{
		BaseURL: srv.LinearURL(), Version: "test", Workspace: "example",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte("lin_api_conformance1")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s: %s", r.Operation, r.Detail)
		}
	})
	return &conformanceEnv{e: e, c: c, clock: clock, ctx: context.Background()}
}

func (f *conformanceEnv) state() trackerfake.LinearWorkspace { return f.e.Snapshot().Linear[0] }

func (f *conformanceEnv) issue(t *testing.T, id string) trackerfake.LinearIssue {
	t.Helper()
	for _, is := range f.state().Issues {
		if is.ID == id {
			return is
		}
	}
	t.Fatalf("issue %s is not in the fake", id)
	return trackerfake.LinearIssue{}
}

type callAnswer[T any] struct {
	v   T
	err error
}

func answered[T any](v T, err error) callAnswer[T] { return callAnswer[T]{v, err} }

func (r callAnswer[T]) must(t *testing.T) T {
	t.Helper()
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.v
}

func identifiers(is []Issue) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Identifier)
	}
	return out
}

func want(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

var linearCases = map[string]func(t *testing.T, f *conformanceEnv){
	"Viewer": func(t *testing.T, f *conformanceEnv) {
		v := answered(f.c.Viewer(f.ctx)).must(t)
		want(t, v.User.ID == cfUser1 && v.User.IsMe && v.Organization.URLKey == "example", "viewer %+v", v)
	},
	"Usage": func(t *testing.T, f *conformanceEnv) {
		answered(f.c.Teams(f.ctx)).must(t)
		u := f.c.Usage()
		want(t, u.Requests.Known && u.Requests.Remaining > 0 && u.Points.Known, "usage %+v", u)
	},
	"Close": func(t *testing.T, f *conformanceEnv) {
		f.c.Close()
		_, err := f.c.Teams(f.ctx)
		want(t, err != nil, "a closed client answered")
	},
	"ForgetCredential": func(t *testing.T, f *conformanceEnv) {
		answered(f.c.Teams(f.ctx)).must(t)
		f.c.ForgetCredential()
		answered(f.c.Teams(f.ctx)).must(t)
	},
	"Teams": func(t *testing.T, f *conformanceEnv) {
		ts := answered(f.c.Teams(f.ctx)).must(t)
		want(t, len(ts) == 1 && ts[0].Key == "WEB" && ts[0].ActiveCycleID == cfActive && ts[0].DefaultStateID == cfTodo, "teams %+v", ts)
	},
	"Team": func(t *testing.T, f *conformanceEnv) {
		tm := answered(f.c.Team(f.ctx, cfTeam)).must(t)
		want(t, tm.Key == "WEB" && tm.CyclesEnabled && tm.CycleWeeks == 2, "team %+v", tm)
	},
	"TeamsByID": func(t *testing.T, f *conformanceEnv) {
		ts := answered(f.c.TeamsByID(f.ctx, []string{cfTeam})).must(t)
		want(t, len(ts) == 1 && ts[0].ID == cfTeam, "teams %+v", ts)
	},
	"TeamsByKey": func(t *testing.T, f *conformanceEnv) {
		ts := answered(f.c.TeamsByKey(f.ctx, []string{"WEB"})).must(t)
		want(t, len(ts) == 1 && ts[0].ID == cfTeam, "teams %+v", ts)
	},
	"TeamMembers": func(t *testing.T, f *conformanceEnv) {
		us := answered(f.c.TeamMembers(f.ctx, cfTeam)).must(t)
		want(t, len(us) == 2, "members %+v", us)
	},
	"Users": func(t *testing.T, f *conformanceEnv) {
		active := answered(f.c.Users(f.ctx, false)).must(t)
		all := answered(f.c.Users(f.ctx, true)).must(t)
		want(t, len(active) == 2 && len(all) == 3, "users %d active, %d in all", len(active), len(all))
	},
	"UsersByID": func(t *testing.T, f *conformanceEnv) {
		us := answered(f.c.UsersByID(f.ctx, []string{cfUser3})).must(t)
		want(t, len(us) == 1 && !us[0].Active && us[0].Name == "User 3", "users %+v", us)
	},
	"WorkflowStates": func(t *testing.T, f *conformanceEnv) {
		ss := answered(f.c.WorkflowStates(f.ctx, cfTeam)).must(t)
		want(t, len(ss) == 5 && slices.ContainsFunc(ss, func(s WorkflowState) bool { return s.ID == cfDone && s.Type == StateCompleted }), "states %+v", ss)
	},
	"Cycles": func(t *testing.T, f *conformanceEnv) {
		all := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam})).must(t)
		active := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesActive})).must(t)
		upcoming := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesUpcoming})).must(t)
		past := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesPast})).must(t)
		want(t, len(all) == 3 && len(active) == 1 && active[0].ID == cfActive && active[0].IsActive &&
			len(upcoming) == 1 && upcoming[0].IsNext && len(past) == 1 && past[0].IsPrevious,
			"cycles %d, active %+v, upcoming %+v, past %+v", len(all), active, upcoming, past)
	},
	"Cycle": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.Cycle(f.ctx, cfUpcoming)).must(t)
		want(t, c.Number == 3 && c.IsFuture && c.TeamID == cfTeam, "cycle %+v", c)
	},
	"CyclesByID": func(t *testing.T, f *conformanceEnv) {
		cs := answered(f.c.CyclesByID(f.ctx, []string{cfPast, cfActive})).must(t)
		want(t, len(cs) == 2, "cycles %+v", cs)
	},
	"Labels": func(t *testing.T, f *conformanceEnv) {
		ls := answered(f.c.Labels(f.ctx, cfTeam)).must(t)
		want(t, slices.ContainsFunc(ls, func(l Label) bool { return l.ID == cfBug && l.TeamID == cfTeam }), "labels %+v", ls)
	},
	"LabelsByID": func(t *testing.T, f *conformanceEnv) {
		ls := answered(f.c.LabelsByID(f.ctx, []string{cfFeature})).must(t)
		want(t, len(ls) == 1 && ls[0].Name == "Feature" && ls[0].TeamID == "", "labels %+v", ls)
	},
	"Projects": func(t *testing.T, f *conformanceEnv) {
		ps := answered(f.c.Projects(f.ctx, cfTeam, false)).must(t)
		want(t, len(ps) == 1 && ps[0].Name == "Website", "projects %+v", ps)
	},
	"ProjectsByID": func(t *testing.T, f *conformanceEnv) {
		ps := answered(f.c.ProjectsByID(f.ctx, []string{cfProject})).must(t)
		want(t, len(ps) == 1 && ps[0].ID == cfProject, "projects %+v", ps)
	},
	"Issue": func(t *testing.T, f *conformanceEnv) {
		d := answered(f.c.Issue(f.ctx, "WEB-1")).must(t)
		want(t, d.ID == cfIssue1 && d.ProjectName == "Website" && len(d.Children) == 1 && d.Children[0].Identifier == "WEB-3" &&
			len(d.Comments.Items) == 1 && len(d.Attachments) == 1 && len(d.Relations) == 1 &&
			d.Estimate != nil && *d.Estimate == 3 && d.Priority == PriorityHigh && slices.Equal(d.LabelIDs, []string{cfBug}) &&
			len(d.History.Items) == 1 && !d.History.HasNextPage && d.History.Served.Equal(cfStart) && len(d.States) == 5 && !d.MoreStates,
			"issue %+v", d)
		// Past the first page of relations each way, the detail reads the
		// rest with IssueRelations and IssueInverseRelations.
		ws := conformanceWorkspace()
		for i := range 30 {
			ws.Relations = append(ws.Relations,
				trackerfake.LinearRelation{ID: fid(1000 + i), Type: "related", Issue: cfIssue1, Related: cfIssue3},
				trackerfake.LinearRelation{ID: fid(2000 + i), Type: "blocks", Issue: cfIssue2, Related: cfIssue1})
		}
		if err := f.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
			t.Fatal(err)
		}
		d = answered(f.c.Issue(f.ctx, "WEB-1")).must(t)
		want(t, len(d.Relations) == 61, "relations %d", len(d.Relations))
	},
	"IssueDetails": func(t *testing.T, f *conformanceEnv) {
		// An ID naming no issue is left out; each issue reads as Issue
		// reads it.
		ds := answered(f.c.IssueDetails(f.ctx, []string{cfIssue1, cfIssue2, fid(999)})).must(t)
		byID := map[string]*IssueDetail{}
		for _, d := range ds {
			byID[d.ID] = d
		}
		one := answered(f.c.Issue(f.ctx, "WEB-1")).must(t)
		want(t, len(ds) == 2 && byID[cfIssue2] != nil && byID[cfIssue1] != nil &&
			len(byID[cfIssue1].Comments.Items) == len(one.Comments.Items) && len(byID[cfIssue1].Relations) == len(one.Relations) &&
			len(byID[cfIssue1].Children) == 1 && len(byID[cfIssue1].States) == 5, "details %+v", ds)
		// Past the first page of relations, each issue's rest is read.
		ws := conformanceWorkspace()
		for i := range 30 {
			ws.Relations = append(ws.Relations,
				trackerfake.LinearRelation{ID: fid(1000 + i), Type: "related", Issue: cfIssue1, Related: cfIssue3},
				trackerfake.LinearRelation{ID: fid(2000 + i), Type: "related", Issue: cfIssue2, Related: cfIssue3})
		}
		if err := f.e.Seed(trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{ws}}); err != nil {
			t.Fatal(err)
		}
		ds = answered(f.c.IssueDetails(f.ctx, []string{cfIssue1, cfIssue2})).must(t)
		for _, d := range ds {
			byID[d.ID] = d
		}
		want(t, len(ds) == 2 && len(byID[cfIssue1].Relations) == 31 && len(byID[cfIssue2].Relations) >= 30,
			"relations %d and %d", len(byID[cfIssue1].Relations), len(byID[cfIssue2].Relations))
		_, err := f.c.IssueDetails(f.ctx, make([]string, MaxIssueDetails+1))
		want(t, errors.Is(err, trackerapi.ErrInvalidInput), "more than MaxIssueDetails IDs: %v", err)
	},
	"IssueStates": func(t *testing.T, f *conformanceEnv) {
		scope, ss, err := f.c.IssueStates(f.ctx, "WEB-3")
		want(t, err == nil && scope == (IssueScope{IssueID: cfIssue3, TeamID: cfTeam, StateID: cfStarted}) && len(ss) == 5, "scope %+v states %+v %v", scope, ss, err)
	},
	"IssueMembers": func(t *testing.T, f *conformanceEnv) {
		scope, us, err := f.c.IssueMembers(f.ctx, cfIssue1)
		want(t, err == nil && scope.TeamID == cfTeam && len(us) == 2, "scope %+v members %+v %v", scope, us, err)
	},
	"IssueLabels": func(t *testing.T, f *conformanceEnv) {
		scope, ls, err := f.c.IssueLabels(f.ctx, cfIssue1)
		ids := make([]string, 0, len(ls))
		for _, l := range ls {
			ids = append(ids, l.ID)
		}
		slices.Sort(ids)
		want(t, err == nil && scope.TeamID == cfTeam && slices.Equal(ids, []string{cfBug, cfFeature}), "scope %+v labels %v %v", scope, ids, err)
	},
	"IssueTeam": func(t *testing.T, f *conformanceEnv) {
		it := answered(f.c.IssueTeam(f.ctx, "WEB-2")).must(t)
		want(t, it.IssueID == cfIssue2 && !it.Archived && it.Team.ID == cfTeam && it.Team.Estimation.Scale == EstimateFibonacci, "issue team %+v", it)
	},
	"CyclePositions": func(t *testing.T, f *conformanceEnv) {
		q := IssueQuery{Filter: IssueFilter{Teams: []string{cfTeam}, IDs: []string{cfIssue1, cfIssue3, fid(99)}}, Limit: 3}
		c, p, err := f.c.CyclePositions(f.ctx, cfUpcoming, q)
		want(t, err == nil && c.ID == cfUpcoming && c.IsFuture && len(p.Items) == 2 && !p.HasNextPage, "cycle %+v positions %+v %v", c, p, err)
		_, _, err = f.c.CyclePositions(f.ctx, fid(99), q)
		want(t, errors.Is(err, trackerapi.ErrNotFound), "a missing cycle: %v", err)
	},
	"SetCycle": func(t *testing.T, f *conformanceEnv) {
		ws := answered(f.c.SetCycle(f.ctx, []string{cfIssue2, cfIssue3}, cfUpcoming)).must(t)
		want(t, len(ws) == 2 && ws[0].Err == nil && ws[1].Err == nil && ws[1].Issue.CycleID == cfUpcoming &&
			f.issue(t, cfIssue2).Cycle == cfUpcoming && f.issue(t, cfIssue3).Cycle == cfUpcoming, "set %+v", ws)
		// A missing issue fails the batch whole; the read after it finds
		// each issue unchanged.
		ws = answered(f.c.SetCycle(f.ctx, []string{cfIssue2, fid(99)}, cfActive)).must(t)
		want(t, len(ws) == 2 && WriteNotApplied(ws[0].Err) && WriteNotApplied(ws[1].Err) && f.issue(t, cfIssue2).Cycle == cfUpcoming, "failed batch %+v", ws)
	},
	"IssueSummary": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.IssueSummary(f.ctx, cfIssue3)).must(t)
		want(t, is.Identifier == "WEB-3" && is.ParentID == cfIssue1 && is.StartedAt != nil, "issue %+v", is)
	},
	"IssueComments": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.IssueComments(f.ctx, cfIssue1, "")).must(t)
		want(t, len(p.Items) == 1 && p.Items[0].Body == "Looks good" && !p.HasNextPage, "comments %+v", p)
	},
	"IssueHistory": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.IssueHistory(f.ctx, cfIssue1, "")).must(t)
		want(t, len(p.Items) == 1 && p.Items[0].State != nil && *p.Items[0].State == (Change{From: cfBacklog, To: cfTodo}), "history %+v", p)
	},
	"IssueHistories": func(t *testing.T, f *conformanceEnv) {
		hs := answered(f.c.IssueHistories(f.ctx, []string{cfIssue1, cfIssue2})).must(t)
		byID := map[string]IssueChanges{}
		for _, h := range hs {
			byID[h.IssueID] = h
		}
		want(t, len(byID) == 2 && len(byID[cfIssue1].Entries) == 1 && byID[cfIssue1].TeamID == cfTeam && byID[cfIssue1].Served.Equal(cfStart), "histories %+v", hs)
	},
	"IssueStateSpans": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.IssueStateSpans(f.ctx, cfIssue1, "")).must(t)
		n := len(p.Items)
		want(t, n == 2 && p.Items[n-1].StateID == cfTodo && p.Items[n-1].EndedAt == nil, "spans %+v", p)
	},
	"ListIssues": func(t *testing.T, f *conformanceEnv) {
		all := answered(f.c.ListIssues(f.ctx, IssueQuery{Filter: IssueFilter{Teams: []string{cfTeam}}}, "")).must(t)
		started := answered(f.c.ListIssues(f.ctx, IssueQuery{Filter: IssueFilter{StateTypes: []StateType{StateStarted}}}, "")).must(t)
		children := answered(f.c.ListIssues(f.ctx, IssueQuery{Filter: IssueFilter{Parent: Match{IDs: []string{cfIssue1}}}}, "")).must(t)
		want(t, slices.Equal(identifiers(all.Items), []string{"WEB-1", "WEB-2", "WEB-3"}) &&
			slices.Equal(identifiers(started.Items), []string{"WEB-3"}) && slices.Equal(identifiers(children.Items), []string{"WEB-3"}),
			"all %v, started %v, children %v", identifiers(all.Items), identifiers(started.Items), identifiers(children.Items))
	},
	"ListIssuePositions": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.ListIssuePositions(f.ctx, IssueQuery{Filter: IssueFilter{Teams: []string{cfTeam}}}, "")).must(t)
		want(t, len(p.Items) == 3 && p.Items[0].ID == cfIssue1 && p.Items[2].SortOrder == 300 && p.Items[1].TeamID == cfTeam, "positions %+v", p.Items)
	},
	"SearchIssues": func(t *testing.T, f *conformanceEnv) {
		p := answered(f.c.SearchIssues(f.ctx, SearchQuery{Term: "login", TeamID: cfTeam}, "")).must(t)
		got := identifiers(p.Items)
		slices.Sort(got)
		want(t, slices.Equal(got, []string{"WEB-1", "WEB-3"}), "search %v", got)
	},
	"CreateIssue": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.CreateIssue(f.ctx, CreateIssueInput{
			TeamID: cfTeam, Title: "New page", Description: "Body", AssigneeID: Set(cfUser2), Priority: Set(PriorityLow),
			Estimate: Set(2), LabelIDs: []string{cfFeature}, CycleID: Set(cfActive), ParentID: Set("WEB-2"), DueDate: Set(Date("2026-11-30")),
		})).must(t)
		stored := f.issue(t, is.ID)
		want(t, is.Identifier == "WEB-4" && is.StateID == cfTodo && stored.Assignee == cfUser2 && stored.Parent == cfIssue2 &&
			stored.DueDate == "2026-11-30" && stored.Cycle == cfActive && slices.Equal(stored.Labels, []string{cfFeature}),
			"created %+v, stored %+v", is, stored)
	},
	"UpdateIssue": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.UpdateIssue(f.ctx, cfIssue2, UpdateIssueInput{
			Title: Set("Add site search"), StateID: Set(cfStarted), AssigneeID: Set(cfUser1), AddedLabelIDs: []string{cfBug}, ProjectID: Set(cfProject),
		})).must(t)
		stored := f.issue(t, cfIssue2)
		want(t, is.Title == "Add site search" && is.StartedAt != nil && stored.State == cfStarted && stored.Project == cfProject &&
			slices.Equal(stored.Labels, []string{cfBug}) && len(stored.History) == 1 && stored.History[0].ToState == cfStarted,
			"updated %+v, stored %+v", is, stored)
		cleared := answered(f.c.UpdateIssue(f.ctx, cfIssue2, UpdateIssueInput{AssigneeID: Null[string](), ProjectID: Null[string]()})).must(t)
		want(t, cleared.AssigneeID == "" && cleared.ProjectID == "", "cleared %+v", cleared)
	},
	"ArchiveIssue": func(t *testing.T, f *conformanceEnv) {
		is := answered(f.c.ArchiveIssue(f.ctx, cfIssue2)).must(t)
		want(t, is.ArchivedAt != nil && !f.issue(t, cfIssue2).ArchivedAt.IsZero(), "archived %+v", is)
	},
	"UnarchiveIssue": func(t *testing.T, f *conformanceEnv) {
		answered(f.c.ArchiveIssue(f.ctx, cfIssue2)).must(t)
		is := answered(f.c.UnarchiveIssue(f.ctx, cfIssue2)).must(t)
		want(t, is.ArchivedAt == nil && f.issue(t, cfIssue2).ArchivedAt.IsZero(), "unarchived %+v", is)
	},
	"DeleteIssue": func(t *testing.T, f *conformanceEnv) {
		if err := f.c.DeleteIssue(f.ctx, cfIssue2, false); err != nil {
			t.Fatal(err)
		}
		want(t, f.issue(t, cfIssue2).Trashed, "not trashed")
		if err := f.c.DeleteIssue(f.ctx, cfIssue2, true); err != nil {
			t.Fatal(err)
		}
		want(t, !slices.ContainsFunc(f.state().Issues, func(is trackerfake.LinearIssue) bool { return is.ID == cfIssue2 }), "not deleted")
	},
	"MoveIssue": func(t *testing.T, f *conformanceEnv) {
		before, after := 100.0, 200.0
		is := answered(f.c.MoveIssue(f.ctx, MoveInput{IssueID: cfIssue3, Field: OrderSort, Before: &before, After: &after})).must(t)
		want(t, is.SortOrder > before && is.SortOrder < after && f.issue(t, cfIssue3).SortOrder == is.SortOrder, "moved %+v", is)
	},
	"RenumberIssues": func(t *testing.T, f *conformanceEnv) {
		after := 300.0
		ws := answered(f.c.RenumberIssues(f.ctx, RenumberInput{Field: OrderSort, IssueIDs: []string{cfIssue2, cfIssue1}, After: &after})).must(t)
		want(t, len(ws) == 2 && ws[0].Err == nil && ws[1].Err == nil && ws[0].Issue.SortOrder < ws[1].Issue.SortOrder && ws[1].Issue.SortOrder < after &&
			f.issue(t, cfIssue1).SortOrder == ws[1].Issue.SortOrder, "renumbered %+v", ws)
		// A missing issue at one alias fails the document whole; the read
		// after it finds the other issue unchanged.
		ws = answered(f.c.RenumberIssues(f.ctx, RenumberInput{Field: OrderSort, IssueIDs: []string{cfIssue3, fid(99)}, Before: &after})).must(t)
		want(t, len(ws) == 2 && WriteNotApplied(ws[0].Err) && WriteNotApplied(ws[1].Err) && f.issue(t, cfIssue3).SortOrder == 300, "failed renumber %+v", ws)
	},
	"PollIssues": func(t *testing.T, f *conformanceEnv) {
		first := answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam})).must(t)
		for first.HasNextPage {
			first = answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam, From: first.Next})).must(t)
		}
		f.clock.advance(time.Minute)
		answered(f.c.UpdateIssue(f.ctx, cfIssue3, UpdateIssueInput{Title: Set("Login tests, all browsers")})).must(t)
		delta := answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam, From: first.Next.Resume()})).must(t)
		// Resumed exactly, the poll holds exactly the issue that changed,
		// and the walk's start is the fake's clock.
		want(t, slices.Equal(identifiers(delta.Items), []string{"WEB-3"}) && delta.Next.Walk.Start.Equal(cfStart.Add(time.Minute)),
			"delta %v walk %+v", identifiers(delta.Items), delta.Next.Walk)
		// An issue the cursor leaves out by ID comes back once updated
		// again, though no newer issue moved the floor.
		f.clock.advance(time.Minute)
		answered(f.c.UpdateIssue(f.ctx, cfIssue3, UpdateIssueInput{Title: Set("Login tests, every browser")})).must(t)
		again := answered(f.c.PollIssues(f.ctx, PollQuery{TeamID: cfTeam, From: delta.Next})).must(t)
		want(t, slices.Equal(identifiers(again.Items), []string{"WEB-3"}), "updated again %v", identifiers(again.Items))
	},
	"RecentIssues": func(t *testing.T, f *conformanceEnv) {
		q := RecentQuery{Filter: IssueFilter{Teams: []string{cfTeam}}, Limit: 2}
		first := answered(f.c.RecentIssues(f.ctx, q)).must(t)
		want(t, slices.Equal(identifiers(first.Items), []string{"WEB-1", "WEB-2"}) && first.HasNextPage &&
			first.Next.Bound.Equal(f.issue(t, cfIssue2).UpdatedAt) && slices.Equal(first.Next.At, []string{cfIssue2}),
			"first page %v next %+v", identifiers(first.Items), first.Next)
		// The page's last issue is updated between pages: it moves to the
		// front, behind the walk, and the issue after it is neither
		// skipped nor joined by one delivered already.
		f.clock.advance(time.Minute)
		answered(f.c.UpdateIssue(f.ctx, cfIssue2, UpdateIssueInput{Title: Set("Add site search")})).must(t)
		q.From = first.Next
		rest := answered(f.c.RecentIssues(f.ctx, q)).must(t)
		want(t, slices.Equal(identifiers(rest.Items), []string{"WEB-3"}) && !rest.HasNextPage, "rest %v", identifiers(rest.Items))
	},
	"Comment": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.Comment(f.ctx, cfComment)).must(t)
		want(t, c.IssueID == cfIssue1 && c.UserID == cfUser2 && c.Body == "Looks good", "comment %+v", c)
	},
	"CreateComment": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.CreateComment(f.ctx, CreateCommentInput{IssueID: "WEB-1", Body: "Thanks", ParentID: cfComment})).must(t)
		want(t, c.IssueID == cfIssue1 && c.ParentID == cfComment && c.UserID == cfUser1 && len(f.state().Comments) == 2, "comment %+v", c)
	},
	"UpdateComment": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.UpdateComment(f.ctx, cfComment, "Looks great")).must(t)
		want(t, c.Body == "Looks great" && c.EditedAt != nil && f.state().Comments[0].Body == "Looks great", "comment %+v", c)
	},
	"DeleteComment": func(t *testing.T, f *conformanceEnv) {
		if err := f.c.DeleteComment(f.ctx, cfComment); err != nil {
			t.Fatal(err)
		}
		want(t, len(f.state().Comments) == 0, "not deleted")
	},
	"Relation": func(t *testing.T, f *conformanceEnv) {
		r := answered(f.c.Relation(f.ctx, cfRel)).must(t)
		want(t, r.Type == RelationBlocks && r.Issue.Identifier == "WEB-1" && r.Related.Identifier == "WEB-2", "relation %+v", r)
	},
	"CreateRelation": func(t *testing.T, f *conformanceEnv) {
		r := answered(f.c.CreateRelation(f.ctx, CreateRelationInput{IssueID: "WEB-2", RelatedIssueID: "WEB-3", Type: RelationRelated})).must(t)
		want(t, r.Issue.ID == cfIssue2 && r.Related.ID == cfIssue3 && len(f.state().Relations) == 2, "relation %+v", r)
	},
	"DeleteRelation": func(t *testing.T, f *conformanceEnv) {
		if err := f.c.DeleteRelation(f.ctx, cfRel); err != nil {
			t.Fatal(err)
		}
		want(t, len(f.state().Relations) == 0, "not deleted")
	},
	"LinkURL": func(t *testing.T, f *conformanceEnv) {
		a := answered(f.c.LinkURL(f.ctx, LinkInput{IssueID: "WEB-2", URL: "https://example.com/pr/2", Title: "Pull request 2", Subtitle: "open"})).must(t)
		want(t, a.IssueID == cfIssue2 && a.Subtitle == "open" && len(f.state().Attachments) == 2, "attachment %+v", a)
		again := answered(f.c.LinkURL(f.ctx, LinkInput{IssueID: "WEB-2", URL: "https://example.com/pr/2", Title: "Pull request 2", Subtitle: "merged"})).must(t)
		want(t, again.ID == a.ID && again.Subtitle == "merged" && len(f.state().Attachments) == 2, "relinked %+v", again)
	},
	"StartUpcomingCycle": func(t *testing.T, f *conformanceEnv) {
		c := answered(f.c.StartUpcomingCycle(f.ctx, cfUpcoming)).must(t)
		want(t, c.IsActive && !c.StartsAt.After(cfStart), "started %+v", c)
		active := answered(f.c.Cycles(f.ctx, CycleQuery{TeamID: cfTeam, When: CyclesActive})).must(t)
		want(t, len(active) == 1 && active[0].ID == cfUpcoming, "active %+v", active)
		want(t, f.issue(t, cfIssue1).Cycle == cfUpcoming, "an unfinished issue did not roll over")
	},
}

func TestFakeConformance(t *testing.T) {
	var mu sync.Mutex
	sent := map[string]bool{}
	all := true
	for _, name := range slices.Sorted(maps.Keys(linearCases)) {
		all = t.Run(name, func(t *testing.T) {
			f := newConformanceEnv(t)
			linearCases[name](t, f)
			mu.Lock()
			defer mu.Unlock()
			for _, r := range f.e.Requests(0).Requests {
				sent[r.Operation] = true
			}
		}) && all
	}
	if !all {
		return
	}
	// Every operation the package registers must have run against the
	// fake, so that a new operation needs a case.
	for _, name := range slices.Sorted(maps.Keys(registeredOps())) {
		if !sent[name] {
			t.Errorf("operation %s never ran against the fake", name)
		}
	}
}

// TestFakeConformanceCoversClient fails for an exported Client method
// with no conformance case.
func TestFakeConformanceCoversClient(t *testing.T) {
	typ := reflect.TypeFor[*Client]()
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; linearCases[name] == nil {
			t.Errorf("Client.%s has no conformance case against the fake", name)
		}
	}
	for name := range linearCases {
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("conformance case %s names no Client method", name)
		}
	}
}
