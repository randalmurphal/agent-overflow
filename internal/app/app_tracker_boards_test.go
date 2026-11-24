package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
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
		"TrackerProjects":               {read, transport.RouteSelected, false},
		"TrackerBoards":                 {read, transport.RouteSelected, false},
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
	e := trackerfake.New(trackerfake.Options{})
	t.Cleanup(e.Close)
	if err := e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{trackerBoardSite()}}); err != nil {
		t.Fatal(err)
	}
	srv, err := trackerfake.Serve(e, "example.atlassian.net")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		for _, r := range e.Unhandled() {
			t.Errorf("unhandled request %s %s: %s", r.Method, r.Path, r.Detail)
		}
	})
	a := newTrackerTestApp(t, func(a *App) {
		ConfigureIsolation(a, IsolationConfig{TrackerOrigin: func(_, site string) (string, error) {
			return srv.JiraURL(strings.TrimPrefix(site, "https://")), nil
		}})
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
	listed := []trackerapp.ProjectBoard{{Connection: conn.ID, Board: "1", OpensFirst: true}}
	if got, err := a.SetProjectTrackerBoards(ctx, project.ID, listed); err != nil || !slices.Equal(got, listed) {
		t.Fatalf("SetProjectTrackerBoards = %+v, %v", got, err)
	}
	return &trackerBoardFixture{a: a, e: e, conn: conn, project: project.ID, events: events}
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
	handles, _, pumps = f.a.trackers.SubscriptionCountsForTesting()
	return handles, pumps
}

// A board is reached end to end from a client: the project's list, the
// subscribe, the event of its first read, a page, a change made in the
// tracker reaching the client as an event and a patch, visibility, and
// the release, through the bound methods with the real providers and
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

	sub := f.subscribe(t, ctx)
	if sub.ID == "" || sub.Loaded {
		t.Fatalf("the first subscribe of a board never read = %+v", sub)
	}
	if _, err := f.a.TrackerBoard(ctx, sub.ID, nil); !hasCode(err, "tracker_board_not_read") {
		t.Fatalf("TrackerBoard before the first read = %v", err)
	}
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

	if err := f.a.SetTrackerSubscriptionVisible(ctx, sub.ID, false); err != nil {
		t.Fatal(err)
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
			return ignoreResult(f.a.SetProjectTrackerBoards(ctx, "no-such-project", nil))
		}},
		{"a board list naming a Tempo connection", "tracker_boards_refused", trackerapp.ErrInvalidBoards, func() error {
			return ignoreResult(f.a.SetProjectTrackerBoards(ctx, f.project, []trackerapp.ProjectBoard{{Connection: tempo.ID, Board: "1"}}))
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
	} {
		err := tc.call()
		code, message, ok := errorsx.PublicDetails(err)
		if !ok || code != tc.code || !errors.Is(err, tc.cause) || message == "" {
			t.Errorf("%s = %v (code %q), want %s with cause %v", tc.name, err, code, tc.code, tc.cause)
		}
	}
	// Every sentence differs: each names its own remedy.
	sentences := map[string]string{}
	for _, err := range []error{
		trackerapp.ErrUnknownSubscription, trackerapp.ErrSubscriptionEnded, trackerapp.ErrTooManySubscriptions,
		trackerapp.ErrDisabled, trackerapp.ErrNotFound, trackerapp.ErrNoProvider, trackerapp.ErrNotListed,
		trackerapp.ErrNotMirrored, trackerapp.ErrStalePage, trackerapp.ErrProjectNotFound, trackerapp.ErrInvalidBoards,
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
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM tracker_cache_roots`).Scan(&n); err != nil {
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
