package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

func TestTrackerBoardMethodsCarryTheirScopeAndRoute(t *testing.T) {
	t.Parallel()
	type want struct {
		scope       transport.Scope
		route       transport.MethodRoute
		callerBound bool
	}
	read, operate := transport.ScopeTrackerRead, transport.ScopeTrackerOperate
	for name, w := range map[string]want{
		"TrackerProjects":               {read, transport.RouteHome, false},
		"TrackerBoards":                 {read, transport.RouteHome, false},
		"ProjectTrackerBoards":          {read, transport.RouteProject, false},
		"SetProjectTrackerBoards":       {operate, transport.RouteProject, false},
		"SubscribeTrackerBoard":         {read, transport.RouteProject, true},
		"TrackerBoard":                  {read, transport.RouteHome, false},
		"TrackerSectionRows":            {read, transport.RouteHome, false},
		"TrackerBoardPatch":             {read, transport.RouteHome, false},
		"SetTrackerSubscriptionVisible": {read, transport.RouteHome, false},
		"RefreshTrackerSubscription":    {read, transport.RouteHome, false},
		"ReleaseTrackerSubscription":    {read, transport.RouteHome, false},
	} {
		i := slices.IndexFunc(transport.GeneratedMethods, func(m transport.MethodMeta) bool { return m.Name == name })
		if i < 0 {
			t.Errorf("%s is not a generated method", name)
			continue
		}
		m := transport.GeneratedMethods[i]
		if m.Scope != w.scope || m.Route != w.route || m.CallerBound != w.callerBound || m.Budget != 0 {
			t.Errorf("%s = %+v, want scope %s, route %s, caller-bound %v, the default budget", name, m, w.scope, w.route, w.callerBound)
		}
	}
}

// Each of the picker's lists that takes the tracker more than one page
// ends at its method's own deadline, the caller setting none: the walk
// is cut while its second page is held, and the answer says the tracker
// was too slow and to try again, leaving the connection without a
// problem. The deadline is shortened here (trackerBudget); without
// one the call stays held and the test's own bound fails it.
func TestTrackerPickerListsEndAtTheirOwnDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method, route string
		call          func(a *App, ctx context.Context, conn string) error
	}{
		{"TrackerProjects", "GET rest/api/3/project/search", func(a *App, ctx context.Context, conn string) error {
			return ignoreResult(a.TrackerProjects(ctx, conn))
		}},
		{"TrackerBoards", "GET rest/agile/1.0/board", func(a *App, ctx context.Context, conn string) error {
			return ignoreResult(a.TrackerBoards(ctx, conn, "10001"))
		}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			// 121 projects and 60 boards of project BIG: two pages each.
			site := trackerBoardSite()
			site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10001", Key: "BIG", Name: "Big"})
			for i := range 119 {
				site.Projects = append(site.Projects, trackerfake.JiraProject{ID: fmt.Sprint(20000 + i), Key: fmt.Sprintf("K%03d", i), Name: fmt.Sprintf("Project %d", i)})
			}
			for i := range 60 {
				site.Boards = append(site.Boards, trackerfake.JiraBoard{ID: int64(100 + i), Name: fmt.Sprintf("Big board %d", i), Type: "kanban", Project: "BIG",
					Columns: site.Boards[0].Columns})
			}
			var e *trackerfake.Engine
			var armed, heldOnce sync.Once
			held := make(chan struct{})
			f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: site, budget: 2 * time.Second, engine: trackerfake.Options{OnRequest: func(r trackerfake.Request) {
				switch {
				case r.Route != tc.route:
				case r.Held:
					heldOnce.Do(func() { close(held) })
				case r.Status == 200:
					armed.Do(func() {
						if err := e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: tc.route, Kind: trackerfake.FaultHold, Times: 1}); err != nil {
							t.Error(err)
						}
					})
				}
			}}})
			e = f.e
			t.Cleanup(func() { e.ClearFaults(trackerBoardOrigin, tc.route) })
			reqs := e.Requests(0).Requests
			since := reqs[len(reqs)-1].Seq

			answer := make(chan error, 1)
			go func() { answer <- tc.call(f.a, context.Background(), f.conn.ID) }()
			var err error
			select {
			case err = <-answer:
			case <-time.After(20 * time.Second):
				t.Fatalf("%s did not end at its own deadline", tc.method)
			}
			select {
			case <-held:
			default:
				t.Fatalf("%s answered %v before its second page was held", tc.method, err)
			}
			if code, message, _ := errorsx.PublicDetails(err); code != methodBudgetExceededCode || !strings.Contains(message, "too slow, so try again") {
				t.Fatalf("%s cut at its deadline = %v, want %s saying to try again", tc.method, err, methodBudgetExceededCode)
			}
			pages := 0
			for _, r := range e.Requests(since).Requests {
				if r.Route == tc.route {
					pages++
				}
			}
			if pages != 2 {
				t.Fatalf("%s asked for %d pages before it was cut, want 2", tc.method, pages)
			}
			conns, err := f.a.TrackerConnections(context.Background())
			if err != nil || len(conns) != 1 || conns[0].Problem != nil {
				t.Fatalf("the connection after the cut list: %+v, %v", conns, err)
			}
		})
	}
}

// The tracker's form reads and writes end at their methods' budgets
// unless a test shortens them.
func TestTrackerRunsEndAtTheirMethodsBudgets(t *testing.T) {
	t.Parallel()
	a := &App{}
	for _, method := range []string{"TrackerProjects", "TrackerBoards", "TrackerMoves", "AddTrackerComment"} {
		ctx, cancel := a.trackerRun(context.Background(), method, "Listing", "")
		deadline, ok := ctx.Deadline()
		cancel()
		if budget := transport.MethodBudget(method); !ok || time.Until(deadline) > budget || time.Until(deadline) < budget-time.Second {
			t.Errorf("%s's deadline is %v away, %v; want its budget %s", method, time.Until(deadline), ok, budget)
		}
	}
}

// trackerBoardOrigin is trackerBoardSite's origin in the fake.
var trackerBoardOrigin = trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}

// trackerBoardSite is the fake Jira site the board tests seed: one scrum
// board of project WEB with an active sprint and three issues.
func trackerBoardSite() trackerfake.JiraSite {
	day := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	return trackerfake.JiraSite{
		Site: "example.atlassian.net", Account: "acc-user1", Token: trackerTestToken,
		Users:    []trackerfake.JiraUser{{AccountID: "acc-user1", DisplayName: "User 1", Email: "user1@example.com", TimeZone: "UTC"}},
		Projects: []trackerfake.JiraProject{{ID: "10000", Key: "WEB", Name: "Web"}},
		Boards: []trackerfake.JiraBoard{{ID: 1, Name: "WEB board", Type: "scrum", Project: "WEB", Columns: []trackerfake.JiraColumn{
			{Name: "To Do", Statuses: []string{"10000"}}, {Name: "In Progress", Statuses: []string{"10001"}}, {Name: "Done", Statuses: []string{"10002"}},
		}}},
		Sprints: []trackerfake.JiraSprint{{ID: 1, Board: 1, Name: "Sprint 1", State: "active", Start: day, End: day.Add(14 * 24 * time.Hour)}},
		Issues: []trackerfake.JiraIssue{
			{ID: "10101", Key: "WEB-1", Project: "WEB", Type: "10001", Summary: "First story", Sprints: []int64{1}, Status: "10001", Created: day, Updated: day},
			{ID: "10102", Key: "WEB-2", Project: "WEB", Type: "10002", Summary: "Second task", Sprints: []int64{1}, Status: "10000", Created: day, Updated: day},
			{ID: "10103", Key: "WEB-3", Project: "WEB", Type: "10002", Summary: "Backlog task", Status: "10000", Created: day, Updated: day},
		},
	}
}

// trackerBoardFixture is an App whose tracker service reaches the fake
// Jira site of trackerBoardSite through trackerfake, with a verified
// connection to it and a project that lists its board.
type trackerBoardFixture struct {
	a       *App
	e       *trackerfake.Engine
	conn    TrackerConnection
	project string
	events  *transport.Subscriber
}

func newTrackerBoardFixture(t *testing.T) *trackerBoardFixture {
	t.Helper()
	return newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite()})
}

// trackerBoardSetup is what a board test changes of the fixture: the
// engine's options, the site, whose every board of project WEB the
// project lists, board 1 first and opening first, the service's sync
// intervals, and the deadline of the tracker's form reads and writes.
type trackerBoardSetup struct {
	engine  trackerfake.Options
	site    trackerfake.JiraSite
	cadence trackerapp.Cadence
	budget  time.Duration
	// arrive, when set, sees each request to the Jira site as it arrives,
	// before its body is read.
	arrive func(*http.Request)
}

func newTrackerBoardFixtureOn(t *testing.T, setup trackerBoardSetup) *trackerBoardFixture {
	t.Helper()
	opts, site := setup.engine, setup.site
	e := trackerfake.New(opts)
	t.Cleanup(e.Close)
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e, "example.atlassian.net")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	jiraURL := srv.JiraURL("example.atlassian.net")
	if setup.arrive != nil {
		h, err := e.Handler(trackerBoardOrigin)
		if err != nil {
			t.Fatal(err)
		}
		seen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			setup.arrive(r)
			h.ServeHTTP(w, r)
		}))
		t.Cleanup(seen.Close)
		jiraURL = seen.URL
	}
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s %s: %s", r.Method, r.Path, r.Detail)
		}
	})
	a := newTrackerTestApp(t, func(a *App) {
		ConfigureIsolation(a, IsolationConfig{TrackerOrigin: func(_, site string) (string, error) {
			if strings.TrimPrefix(site, "https://") == "example.atlassian.net" {
				return jiraURL, nil
			}
			return srv.JiraURL(strings.TrimPrefix(site, "https://")), nil
		}})
		a.trackerCadence = setup.cadence
		a.trackerBudget = setup.budget
	})
	bus := transport.NewEventBus(64)
	t.Cleanup(bus.Close)
	a.SetEventBus(bus)
	events := bus.Subscribe()
	t.Cleanup(events.Close)
	ctx := context.Background()
	conn, err := a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conn.KeyFilePath, []byte(trackerTestToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if conn, err = a.VerifyTrackerConnection(ctx, conn.ID); err != nil || conn.Problem != nil {
		t.Fatalf("VerifyTrackerConnection = %+v, %v", conn, err)
	}
	project := store.Project{ID: "project-web", Path: t.TempDir(), Name: "web", CreatedAt: 1, UpdatedAt: 1}
	if _, err := a.store.CreateProject(project); err != nil {
		t.Fatal(err)
	}
	var listed []trackerapp.ProjectBoard
	var want []trackerapp.BoardChoice
	for _, b := range site.Boards {
		if b.Project == "WEB" {
			board := trackerapp.ProjectBoard{Connection: conn.ID, Board: fmt.Sprint(b.ID), OpensFirst: len(listed) == 0, Name: b.Name, TrackerProject: "WEB"}
			listed = append(listed, board)
			want = append(want, trackerapp.BoardChoice{
				Project: project.ID, Connection: conn.ID, ConnectionKind: store.TrackerKindJira, ConnectionEnabled: true,
				VerifiedAccount: conn.VerifiedAccountID, Board: board.Board, Name: b.Name, TrackerProject: "WEB", OpensFirst: board.OpensFirst,
			})
		}
	}
	if got, err := a.SetProjectTrackerBoards(ctx, project.ID, nil, listed); err != nil || !slices.Equal(got, want) {
		t.Fatalf("SetProjectTrackerBoards = %+v, %v; want %+v", got, err, want)
	}
	if got, err := a.ProjectTrackerBoards(ctx, project.ID); err != nil || !slices.Equal(got, want) {
		t.Fatalf("ProjectTrackerBoards = %+v, %v; want %+v", got, err, want)
	}
	return &trackerBoardFixture{a: a, e: e, conn: conn, project: project.ID, events: events}
}

// held is the project's list as a page that read it holds it.
func (f *trackerBoardFixture) held(t *testing.T, ctx context.Context) []trackerapp.BoardRef {
	t.Helper()
	listed, err := f.a.ProjectTrackerBoards(ctx, f.project)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]trackerapp.BoardRef, len(listed))
	for i, b := range listed {
		out[i] = trackerapp.BoardRef{Connection: b.Connection, Board: b.Board, OpensFirst: b.OpensFirst}
	}
	return out
}

// subscribe subscribes ctx's connection to the board with its sprint.
func (f *trackerBoardFixture) subscribe(t *testing.T, ctx context.Context) trackerapp.BoardSubscription {
	t.Helper()
	sub, err := f.a.SubscribeTrackerBoard(ctx, f.project, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// boardEvent waits for the next tracker:board_updated event that ok
// accepts, skipping every other.
func (f *trackerBoardFixture) boardEvent(t *testing.T, what string, ok func(trackerapp.BoardEvent) bool) trackerapp.BoardEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-f.events.Events():
			if e.Channel != string(eventchan.TrackerBoardUpdated) {
				continue
			}
			var got trackerapp.BoardEvent
			if err := json.Unmarshal(e.Data, &got); err != nil {
				t.Fatal(err)
			}
			if ok(got) {
				return got
			}
		case <-deadline:
			t.Fatalf("no tracker:board_updated event %s", what)
		}
	}
}

// boardHandles is how many board handles the service holds and how many
// board pumps run.
func (f *trackerBoardFixture) boardHandles() (handles, pumps int) {
	handles, _, pumps, _ = f.a.trackers.SubscriptionCountsForTesting()
	return handles, pumps
}

// A board is reached end to end from a client: the project's list, the
// subscribe, the event of its first read, a page, a change made in the
// tracker reaching the client as an event and a patch, and the release,
// through the bound methods with the real providers and
// clients against the fake tracker.
func TestATrackerBoardReachesItsClient(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})

	projects, err := f.a.TrackerProjects(ctx, f.conn.ID)
	if err != nil || len(projects) != 1 || projects[0].Key != "WEB" {
		t.Fatalf("TrackerProjects = %+v, %v", projects, err)
	}
	boards, err := f.a.TrackerBoards(ctx, f.conn.ID, projects[0].ID)
	if err != nil || len(boards) != 1 || boards[0].ID != "1" || !boards[0].PlansSprints {
		t.Fatalf("TrackerBoards = %+v, %v", boards, err)
	}
	listed, err := f.a.ProjectTrackerBoards(ctx, f.project)
	if err != nil || len(listed) != 1 || listed[0].Board != "1" || !listed[0].OpensFirst {
		t.Fatalf("ProjectTrackerBoards = %+v, %v", listed, err)
	}

	// The board's first read waits in the fake until the board is seen
	// unread.
	if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Kind: trackerfake.FaultHold}); err != nil {
		t.Fatal(err)
	}
	sub := f.subscribe(t, ctx)
	if sub.ID == "" || sub.Loaded {
		t.Fatalf("the first subscribe of a board never read = %+v", sub)
	}
	if _, err := f.a.TrackerBoard(ctx, sub.ID, nil); !hasCode(err, "tracker_board_not_read") {
		t.Fatalf("TrackerBoard before the first read = %v", err)
	}
	f.e.ClearFaults(trackerBoardOrigin, "")
	first := f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return e.ReadAgain && e.Seq > sub.Seq })
	if first.Scope.Board != "1" || first.Sync.ReadAt == 0 {
		t.Fatalf("the first read's event %+v", first)
	}
	sprint := []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}}
	view, err := f.a.TrackerBoard(ctx, sub.ID, sprint)
	if err != nil || view.Name != "WEB board" || len(view.Sections) != 1 || len(view.Sections[0].Rows) != 2 {
		t.Fatalf("TrackerBoard = %+v, %v", view, err)
	}
	backlog, err := f.a.TrackerSectionRows(ctx, sub.ID, trackerapp.Section{Kind: trackerapp.SectionBacklog}, nil)
	if err != nil || len(backlog.Rows) != 1 || backlog.Rows[0].Card.Key != "WEB-3" {
		t.Fatalf("TrackerSectionRows = %+v, %v", backlog, err)
	}

	// A person renames WEB-2 in the tracker; a refresh reads it.
	p, err := f.a.trackers.Provider(ctx, f.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Edit(ctx, tracker.EditInput{Issue: "10102", Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Second task, renamed")}}}); !r.AllApplied() {
		t.Fatalf("the edit in the tracker: %+v", r)
	}
	if err := f.a.RefreshTrackerSubscription(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	changed := f.boardEvent(t, "naming the renamed issue", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10102") })
	held := trackerapp.TotalsAt{Lifetime: view.Lifetime, Revision: view.TotalsRevision}
	patch, err := f.a.TrackerBoardPatch(ctx, sub.ID, sprint, idStringsOf(changed.Changed), held)
	if err != nil || len(patch.Sections) != 1 || len(patch.Sections[0].Rows) != 1 || patch.Sections[0].Rows[0].Card.Title != "Second task, renamed" {
		t.Fatalf("TrackerBoardPatch = %+v, %v", patch, err)
	}
	if patch.Totals != nil {
		t.Fatalf("a rename that moved no total sent the totals: %+v", patch.Totals)
	}

	if err := f.a.ReleaseTrackerSubscription(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if handles, pumps := f.boardHandles(); handles+pumps != 0 {
		t.Fatalf("after the release %d handles and %d pumps", handles, pumps)
	}
	for name, call := range map[string]func() error{
		"TrackerBoard":                  func() error { return ignoreResult(f.a.TrackerBoard(ctx, sub.ID, nil)) },
		"TrackerBoardPatch":             func() error { return ignoreResult(f.a.TrackerBoardPatch(ctx, sub.ID, nil, nil, held)) },
		"SetTrackerSubscriptionVisible": func() error { return f.a.SetTrackerSubscriptionVisible(ctx, sub.ID, true) },
		"RefreshTrackerSubscription":    func() error { return f.a.RefreshTrackerSubscription(ctx, sub.ID) },
	} {
		if err := call(); !hasCode(err, "tracker_subscription_unknown") || !errors.Is(err, trackerapp.ErrUnknownSubscription) {
			t.Errorf("%s of a released subscription = %v", name, err)
		}
	}
	if err := f.a.ReleaseTrackerSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("a second release = %v", err)
	}
}

// boardReads counts the delta reads the fake answers per board, for a
// test whose shown board is its clock.
type boardReads struct {
	mu   sync.Mutex
	n    map[string]int
	read chan struct{}
}

func newBoardReads() *boardReads {
	return &boardReads{n: map[string]int{}, read: make(chan struct{}, 1)}
}

// observe is the engine's OnRequest: it counts an answered read of a
// board's issues.
func (r *boardReads) observe(req trackerfake.Request) {
	if req.Route != "GET rest/software/1.0/board/{board}/issue" || req.Status == 0 {
		return
	}
	board := strings.Split(strings.TrimPrefix(req.Path, "/rest/software/1.0/board/"), "/")[0]
	r.mu.Lock()
	r.n[board]++
	r.mu.Unlock()
	select {
	case r.read <- struct{}{}:
	default:
	}
}

func (r *boardReads) of(board string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[board]
}

// waitFor waits until board has more than n reads.
func (r *boardReads) waitFor(t *testing.T, board string, n int, what string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for r.of(board) <= n {
		select {
		case <-r.read:
		case <-deadline:
			t.Fatalf("%s: board %s read %d times, want more than %d", what, board, r.of(board), n)
		}
	}
}

// A board a client hides is not read while another board keeps reading,
// and is read when the client shows it again.
func TestAHiddenTrackerBoardIsNotRead(t *testing.T) {
	t.Parallel()
	site := trackerBoardSite()
	clockBoard := site.Boards[0]
	clockBoard.ID, clockBoard.Name = 2, "WEB board 2"
	site.Boards = append(site.Boards, clockBoard)
	reads := newBoardReads()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{
		site: site, engine: trackerfake.Options{OnRequest: reads.observe},
		cadence: trackerapp.Cadence{Delta: 50 * time.Millisecond},
	})
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	sub := f.subscribe(t, ctx)
	if _, err := f.a.SubscribeTrackerBoard(ctx, f.project, f.conn.ID, "2", nil); err != nil {
		t.Fatal(err)
	}
	clock := func(n int) {
		t.Helper()
		for range n {
			reads.waitFor(t, "2", reads.of("2"), "the shown board's clock")
		}
	}
	reads.waitFor(t, "1", 1, "the board before it is hidden")
	if err := f.a.SetTrackerSubscriptionVisible(ctx, sub.ID, false); err != nil {
		t.Fatal(err)
	}
	clock(2) // a read of board 1 under way at the hide ends
	hidden := reads.of("1")
	clock(5)
	if got := reads.of("1"); got != hidden {
		t.Fatalf("the hidden board was read %d times over five reads of the shown one", got-hidden)
	}
	if err := f.a.SetTrackerSubscriptionVisible(ctx, sub.ID, true); err != nil {
		t.Fatal(err)
	}
	reads.waitFor(t, "1", hidden, "the board shown again")
}

// A refresh that waited its whole budget says whether it ran: one the
// board's pump never took, busy with its own read, did not run and is
// to be made again; one it took is still reading and its events follow.
func TestARefreshCutAtItsBudgetSaysWhetherItRan(t *testing.T) {
	t.Parallel()
	held := make(chan struct{}, 16)
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), engine: trackerfake.Options{OnRequest: func(r trackerfake.Request) {
		if r.Held {
			held <- struct{}{}
		}
	}}})
	t.Cleanup(func() { f.e.ClearFaults(trackerBoardOrigin, "") })
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	hold := func() {
		t.Helper()
		if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Kind: trackerfake.FaultHold}); err != nil {
			t.Fatal(err)
		}
	}
	await := func(what string, ready <-chan struct{}) {
		t.Helper()
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Fatalf("waited 10s for %s", what)
		}
	}
	refresh := func(id string, beforeCut func()) error {
		t.Helper()
		run, end := testutil.DeadlineContext(ctx)
		answer := make(chan error, 1)
		go func() { answer <- f.a.RefreshTrackerSubscription(run, id) }()
		beforeCut()
		end(errTrackerRefreshWait)
		select {
		case err := <-answer:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the refresh did not answer within 10s of its cut")
			return nil
		}
	}

	// The pump's first read is held, so the refresh waits to be taken.
	hold()
	sub := f.subscribe(t, ctx)
	await("the pump's first read to be held", held)
	err := refresh(sub.ID, func() {})
	if !hasCode(err, "tracker_refresh_not_run") {
		t.Fatalf("a refresh the busy pump never took = %v, want tracker_refresh_not_run", err)
	}
	f.e.ClearFaults(trackerBoardOrigin, "")
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return e.ReadAgain && e.Seq > sub.Seq })

	// The pump is idle: it takes the refresh, whose read is held.
	for len(held) > 0 {
		<-held
	}
	hold()
	err = refresh(sub.ID, func() { await("the refresh's read to be held", held) })
	if !hasCode(err, "tracker_refresh_waiting") {
		t.Fatalf("a refresh the pump took and is still reading = %v, want tracker_refresh_waiting", err)
	}
}

func idStringsOf(ids []tracker.IssueID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// hasCode reports whether err carries the public code.
func hasCode(err error, code string) bool {
	got, _, ok := errorsx.PublicDetails(err)
	return ok && got == code
}

// A client's subscriptions end with its connection: subscribing and
// releasing again and again, and a client that drops and comes back,
// leave no handle and no pump behind, and another client's subscription
// to the same board stands through it.
func TestTrackerBoardSubscriptionsFollowTheirClient(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	other, _ := newTestConnection(t, transport.ConnPrincipal{})
	kept := f.subscribe(t, other)
	for range 5 {
		ctx, conn := newTestConnection(t, transport.ConnPrincipal{})
		sub := f.subscribe(t, ctx)
		if !conn.Bound(trackerSubscriptionCleanupKey(sub.ID)) {
			t.Fatal("the subscription is not tied to its connection")
		}
		if err := f.a.ReleaseTrackerSubscription(ctx, sub.ID); err != nil {
			t.Fatal(err)
		}
		if conn.Bound(trackerSubscriptionCleanupKey(sub.ID)) {
			t.Fatal("a released subscription is still tied to its connection")
		}
		// The client drops with a subscription open, and comes back.
		f.subscribe(t, ctx)
		f.subscribe(t, ctx)
		conn.RunCleanups()
	}
	if handles, pumps := f.boardHandles(); handles != 1 || pumps != 1 {
		t.Fatalf("after five clients came and went: %d handles and %d pumps, want the other client's 1 and 1", handles, pumps)
	}
	if _, err := f.a.TrackerBoard(other, kept.ID, nil); err != nil && !hasCode(err, "tracker_board_not_read") {
		t.Fatalf("the other client's subscription: %v", err)
	}

	// A subscription that ends on its own, its connection turned off,
	// unties its client.
	ctx, conn := newTestConnection(t, transport.ConnPrincipal{})
	sub := f.subscribe(t, ctx)
	if _, err := f.a.SetTrackerConnectionEnabled(context.Background(), f.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if conn.Bound(trackerSubscriptionCleanupKey(sub.ID)) {
		t.Fatal("a subscription its connection's turn off ended is still tied to its client")
	}
	if handles, pumps := f.boardHandles(); handles+pumps != 0 {
		t.Fatalf("after the turn off: %d handles and %d pumps", handles, pumps)
	}
	if _, err := f.a.SubscribeTrackerBoard(ctx, f.project, f.conn.ID, "1", nil); !hasCode(err, "tracker_connection_off") {
		t.Fatalf("a subscribe to a board of a connection turned off = %v", err)
	}
}

// trackerBoardConnectionTie is SubscribeTrackerBoard's row of
// connectionTieChecks: a handle on the board's pump, released by any
// caller.
func trackerBoardConnectionTie(t *testing.T) connectionTie {
	f := newTrackerBoardFixture(t)
	return connectionTie{
		open: func(ctx context.Context) (string, error) {
			sub, err := f.a.SubscribeTrackerBoard(ctx, f.project, f.conn.ID, "1", nil)
			return sub.ID, err
		},
		// The handle and the pump it alone keeps.
		held: func() int {
			handles, pumps := f.boardHandles()
			return handles + pumps
		},
		unit:      2,
		release:   f.a.ReleaseTrackerSubscription,
		anyCaller: true,
		key:       trackerSubscriptionCleanupKey,
	}
}

// Each refusal a person can cause or see carries its code, a sentence of
// what to do next, and the service's error as its cause.
func TestTrackerBoardRefusalsSayWhatToDo(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	subscribe := func(project, conn, board string) error {
		return ignoreResult(f.a.SubscribeTrackerBoard(ctx, project, conn, board, nil))
	}
	tempo, err := f.a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindTempo, Site: "https://api.tempo.io", ParentID: f.conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code string
		cause      error
		call       func() error
	}{
		{"a board the project does not list", "tracker_board_not_listed", trackerapp.ErrNotListed, func() error { return subscribe(f.project, f.conn.ID, "2") }},
		{"a project that does not exist", "tracker_project_missing", trackerapp.ErrProjectNotFound, func() error {
			return ignoreResult(f.a.SetProjectTrackerBoards(ctx, "no-such-project", nil, nil))
		}},
		{"a board list naming a Tempo connection", "tracker_boards_refused", trackerapp.ErrInvalidBoards, func() error {
			return ignoreResult(f.a.SetProjectTrackerBoards(ctx, f.project, f.held(t, ctx), []trackerapp.ProjectBoard{{Connection: tempo.ID, Board: "1"}}))
		}},
		{"a board list built from a list that is no longer stored", "tracker_boards_changed", trackerapp.ErrBoardsChanged, func() error {
			return ignoreResult(f.a.SetProjectTrackerBoards(ctx, f.project, nil, nil))
		}},
		{"the projects of a Tempo connection", "tracker_connection_tempo", trackerapp.ErrNoProvider, func() error {
			return ignoreResult(f.a.TrackerProjects(ctx, tempo.ID))
		}},
		{"the projects of a removed connection", "tracker_connection_missing", trackerapp.ErrNotFound, func() error {
			return ignoreResult(f.a.TrackerProjects(ctx, "no-such-connection"))
		}},
		{"an unknown subscription", "tracker_subscription_unknown", trackerapp.ErrUnknownSubscription, func() error {
			return ignoreResult(f.a.TrackerSectionRows(ctx, "no-such-subscription", trackerapp.Section{Kind: trackerapp.SectionBacklog}, nil))
		}},
		{"a Jira site with a path", "tracker_connection_invalid", trackerapp.ErrInvalid, func() error {
			return ignoreResult(f.a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net/jira", Account: "user1@example.com"}))
		}},
		{"a connection this computer has", "tracker_connection_exists", store.ErrTrackerConnectionExists, func() error {
			return ignoreResult(f.a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com"}))
		}},
		{"a rename of a removed connection", "tracker_connection_missing", trackerapp.ErrNotFound, func() error {
			return ignoreResult(f.a.RenameTrackerConnection(ctx, "no-such-connection", "Work"))
		}},
		{"the key file of a removed connection", "tracker_connection_missing", trackerapp.ErrNotFound, func() error {
			return ignoreResult(f.a.EditTrackerKeyFile(ctx, "no-such-connection"))
		}},
		{"the token page of a removed connection", "tracker_connection_missing", trackerapp.ErrNotFound, func() error {
			return f.a.OpenTrackerTokenPage(ctx, "no-such-connection")
		}},
		{"the removal of a removed connection", "tracker_connection_missing", trackerapp.ErrNotFound, func() error {
			return f.a.RemoveTrackerConnection(ctx, "no-such-connection")
		}},
	} {
		err := tc.call()
		code, message, ok := errorsx.PublicDetails(err)
		if !ok || code != tc.code || !errors.Is(err, tc.cause) || message == "" {
			t.Errorf("%s = %v (code %q), want %s with cause %v", tc.name, err, code, tc.code, tc.cause)
		}
	}
	// An input the service refuses is told in the service's own words.
	_, message, _ := errorsx.PublicDetails(ignoreResult(f.a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net/jira", Account: "user1@example.com"})))
	if want := "the Jira site must be an address such as https://example.atlassian.net: it has a path"; message != want {
		t.Errorf("a Jira site with a path is told %q, want %q", message, want)
	}
	// True whichever screen changed the list, the one that saves included
	// (a connection removed there deletes its listings).
	_, message, _ = errorsx.PublicDetails(ignoreResult(f.a.SetProjectTrackerBoards(ctx, f.project, nil, nil)))
	if want := "this change was not saved: the project's boards changed after this list was read"; message != want {
		t.Errorf("a save from an old list is told %q, want %q", message, want)
	}
	// Every sentence differs: each names its own remedy.
	sentences := map[string]string{}
	for _, err := range []error{
		trackerapp.ErrUnknownSubscription, trackerapp.ErrSubscriptionEnded, trackerapp.ErrTooManySubscriptions,
		trackerapp.ErrDisabled, trackerapp.ErrNotFound, trackerapp.ErrNoProvider, trackerapp.ErrNotListed,
		trackerapp.ErrNotMirrored, trackerapp.ErrStalePage, trackerapp.ErrProjectNotFound, trackerapp.ErrInvalidBoards,
		store.ErrTrackerConnectionExists,
	} {
		code, message := trackerRefusal(err)
		if code == "" {
			t.Errorf("%v has no code", err)
		}
		if prior, seen := sentences[message]; seen {
			t.Errorf("%v and %s share the sentence %q", err, prior, message)
		}
		sentences[message] = code
	}
}

// A boot runs the trackers' step with the unattended work and, once the
// first client's reads settle, finishes a mirror cleanup the last run left
// with no connection on, which the linked-issues loop alone would never
// run.
func TestABootFinishesTheLastRunsTrackerCleanup(t *testing.T) {
	a := newBootTestApp(t)
	dbPath := filepath.Join(a.dataDirOverride, "agent-overflow", databaseFileName)
	migratedDatabaseAt(t, dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// A removal the last run did not drain: its root detached, no
	// connection on.
	conn, err := st.CreateTrackerConnection(store.NewTrackerConnection{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net", Account: "user1@example.com", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.GetOrCreateMirrorRoot(conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTrackerConnection(conn.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	roots := func() int {
		db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = db.QueryRow(`SELECT count(*) FROM tracker_cache_roots`).Scan(&n)
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if roots() != 1 {
		t.Fatal("the fixture left no detached root")
	}

	progress := newRecordingBootProgress("")
	SetBootProgress(a, progress)
	a.maintenance.firstReadsFallback = time.Hour
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if begun, ended, _ := progress.snapshot(); !slices.Contains(begun, "tracker.start") || !slices.Contains(ended, "tracker.start") {
		t.Fatalf("the boot ran no tracker.start phase: begun %v", begun)
	}
	if failures := progress.failures(); len(failures) != 0 {
		t.Fatalf("boot failures = %v", failures)
	}
	for until := time.Now().Add(200 * time.Millisecond); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		if roots() != 1 {
			t.Fatal("the cleanup ran before the first client's reads settled")
		}
	}
	a.firstReads.expire()
	for deadline := time.Now().Add(10 * time.Second); roots() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the detached root was not drained once the first reads settled")
		}
	}
}

// A trackers' step that cannot arm the linked-issues loop fails, which its
// boot step reports to every client (reportedBootStep).
func TestTheTrackersBootStepFailsWhenTheLoopCannotStart(t *testing.T) {
	t.Parallel()
	a := newTrackerTestApp(t, nil)
	a.trackers.Stop()
	if err := a.startTrackers(); !errors.Is(err, trackerapp.ErrStopped) {
		t.Fatalf("startTrackers on a stopped service = %v, want ErrStopped", err)
	}
}

// A harness reset removes every tracker connection, which ends the board
// subscriptions on them.
func TestHarnessResetRemovesTheTrackerConnections(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	f.subscribe(t, ctx)
	if err := (&harnessHost{app: f.a}).ResetTrackerConnections(); err != nil {
		t.Fatal(err)
	}
	if list, err := f.a.TrackerConnections(ctx); err != nil || len(list) != 0 {
		t.Fatalf("connections after the reset = %+v, %v", list, err)
	}
	if handles, pumps := f.boardHandles(); handles+pumps != 0 {
		t.Fatalf("after the reset %d handles and %d pumps", handles, pumps)
	}
}

// A cleanup that fails once the first reads settle is reported to every
// client as a failure of the boot (bootPhaseFailed).
func TestTheTrackersBootStepReportsAFailedCleanup(t *testing.T) {
	t.Parallel()
	path := storetest.ClonePath(t)
	a := newTestAppAtStorePath(t, path)
	a.configDir, a.version = t.TempDir(), "1.2.3"
	a.maintenance.firstReadsFallback = time.Hour
	progress := newRecordingBootProgress("")
	SetBootProgress(a, progress)
	if err := a.initTrackers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.trackers.Stop)
	// The cleanup's first statement reads a table that is gone.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE tracker_cache_roots`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.startTrackers(); err != nil {
		t.Fatal(err)
	}
	a.firstReads.expire()
	for deadline := time.Now().Add(10 * time.Second); len(progress.failures()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the failed cleanup was not reported")
		}
	}
	if got := progress.failures(); len(got) != 1 || !strings.Contains(got[0], "finish the issue tracker cleanup the last run left") {
		t.Fatalf("boot failures = %v", got)
	}
}
