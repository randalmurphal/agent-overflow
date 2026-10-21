package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

var (
	prListRepoA = gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets"}
	prListRepoB = gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "team", Repo: "app"}
)

// prListFake stands in for the forge behind the list pump: each
// repository's rows and failure, and the reads made with their admission
// (interactive). listGate, when set, runs before each list read, outside
// the fake's lock, so a test can hold a read.
type prListFake struct {
	mu          sync.Mutex
	projects    []ForgeProject
	rows        map[string][]gitops.PRListRow
	listErr     map[string]error
	lists       map[string]int
	interactive map[string][]bool
	listGate    func(repo gitops.RepoReference)
}

func newPRListFake(t *testing.T, app *App) *prListFake {
	f := &prListFake{
		projects: []ForgeProject{
			{ProjectID: "p-a", Name: "Widgets", Path: "/w", Repo: prListRepoA},
			{ProjectID: "p-b", Name: "App", Path: "/a", Repo: prListRepoB},
		},
		rows:        map[string][]gitops.PRListRow{},
		listErr:     map[string]error{},
		lists:       map[string]int{},
		interactive: map[string][]bool{},
	}
	// Registered before any subscription, so it runs after their release.
	t.Cleanup(app.prUpdates.wg.Wait)
	app.prUpdates.listProjectsFn = func(context.Context) ([]ForgeProject, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(f.projects), nil
	}
	app.prUpdates.listFetchFn = func(ctx context.Context, repo gitops.RepoReference) ([]gitops.PRListRow, error) {
		f.mu.Lock()
		gate := f.listGate
		f.mu.Unlock()
		if gate != nil {
			gate(repo)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lists[repo.Key()]++
		f.interactive[repo.Key()] = append(f.interactive[repo.Key()], forgeapi.IsInteractive(ctx))
		rows := slices.Clone(f.rows[repo.Key()])
		var detail *gitops.PRListDetailError
		if err := f.listErr[repo.Key()]; err != nil && !errors.As(err, &detail) {
			return nil, err
		} else if err != nil {
			return rows, err
		}
		return rows, nil
	}
	return f
}

func (f *prListFake) set(fn func(f *prListFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *prListFake) listCount(repo gitops.RepoReference) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists[repo.Key()]
}

func prListRow(repo gitops.RepoReference, number int, mutate func(*gitops.PRListRow)) gitops.PRListRow {
	row := gitops.PRListRow{
		Ref:       gitops.PRReference{Forge: repo.Forge, Host: repo.Host, Namespace: repo.Namespace, Repo: repo.Repo, Number: number},
		Title:     fmt.Sprintf("PR %d", number),
		UpdatedAt: "2026-01-01T00:00:00Z",
		HeadSHA:   fmt.Sprintf("%040x", number),
	}
	if mutate != nil {
		mutate(&row)
	}
	return row
}

// prListEvents are the list's frames a test captured: pulls:updated
// snapshots and pulls:badge counts.
type prListEvents struct {
	updates chan PRListSnapshot
	badges  chan int
}

func capturePRList(t *testing.T, app *App) prListEvents {
	t.Helper()
	events := prListEvents{updates: make(chan PRListSnapshot, 64), badges: make(chan int, 64)}
	app.testEmitHook = func(name string, data any) {
		switch name {
		case "pulls:updated":
			snapshot, ok := data.(PRListSnapshot)
			if !ok {
				t.Errorf("pulls:updated payload type = %T", data)
				return
			}
			events.updates <- snapshot
		case "pulls:badge":
			badge, ok := data.(PRListBadge)
			if !ok {
				t.Errorf("pulls:badge payload type = %T", data)
				return
			}
			events.badges <- badge.NeedsYou
		}
	}
	return events
}

// awaitPRList waits for a frame that satisfies ok, skipping the others.
func awaitPRList(t *testing.T, events prListEvents, why string, ok func(PRListSnapshot) bool) PRListSnapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case snapshot := <-events.updates:
			if ok(snapshot) {
				return snapshot
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", why)
			return PRListSnapshot{}
		}
	}
}

// awaitPRListRead waits for the frame that ends the first tick.
func awaitPRListRead(t *testing.T, events prListEvents) PRListSnapshot {
	t.Helper()
	return awaitPRList(t, events, "the first tick's last frame", func(s PRListSnapshot) bool { return !s.Stale })
}

func prListProject(t *testing.T, snapshot PRListSnapshot, projectID string) ProjectPRList {
	t.Helper()
	for _, project := range snapshot.Projects {
		if project.ProjectID == projectID {
			return project
		}
	}
	t.Fatalf("snapshot has no project %s: %+v", projectID, snapshot.Projects)
	return ProjectPRList{}
}

func prListPumpLive(app *App) bool {
	app.prUpdates.mu.Lock()
	defer app.prUpdates.mu.Unlock()
	return app.prUpdates.list != nil && !app.prUpdates.list.dead
}

func subscribePRList(t *testing.T, app *App) PRListSubscriptionResult {
	t.Helper()
	sub, err := app.SubscribePRList(context.Background())
	if err != nil {
		t.Fatalf("SubscribePRList: %v", err)
	}
	t.Cleanup(func() {
		if err := app.UnsubscribePRList(context.Background(), sub.ID); err != nil {
			t.Errorf("UnsubscribePRList: %v", err)
		}
	})
	return sub
}

func prListSnapshotNow(app *App) PRListSnapshot {
	app.prUpdates.mu.Lock()
	defer app.prUpdates.mu.Unlock()
	return app.prUpdates.list.snapshot
}

// storePRList writes a repository's read into the store as a tick would,
// readAt ago.
func storePRList(t *testing.T, app *App, projectID string, repo gitops.RepoReference, rows []gitops.PRListRow, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago)
	encoded, err := json.Marshal(prListReadOf(projectID, "stored", repo, rows, at, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.PutPRListProject(store.PRListProject{RepoKey: repo.Key(), ProjectJSON: encoded, ReadAt: at.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
}

func storedPRListKeys(t *testing.T, app *App) []string {
	t.Helper()
	records, err := app.store.ListPRListProjects()
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// The subscribe answers from the store before any forge read: the stored
// rows, Stale while a project's stored read is old or missing, and the
// read the first tick then makes is published as it lands. A stored copy
// younger than the interval is not stale.
func TestPRListSubscribeAnswersFromTheStoreBeforeAnyForgeCall(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	storePRList(t, app, "p-a", prListRepoA, []gitops.PRListRow{prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.ViewerReviewState = "unreviewed" })}, 2*time.Hour)
	release := make(chan struct{})
	var entered sync.WaitGroup
	entered.Add(2)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 2, nil)}
		f.listGate = func(gitops.RepoReference) {
			entered.Done()
			<-release
		}
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	snapshot := sub.Snapshot
	a := prListProject(t, snapshot, "p-a")
	if !snapshot.Stale || snapshot.Seq == 0 || len(a.Rows) != 1 || a.Rows[0].Ref.Number != 1 || a.Rows[0].State != PRStateYourTurn || a.ReadAt == "" || a.Name != "Widgets" {
		t.Fatalf("stored answer = %+v", snapshot)
	}
	if snapshot.NeedsYou != 1 || len(prListProject(t, snapshot, "p-b").Rows) != 0 {
		t.Fatalf("stored answer needs-you %d, project B %+v", snapshot.NeedsYou, prListProject(t, snapshot, "p-b"))
	}
	// The pump's reads are under way and held: the answer did not wait.
	entered.Wait()
	if got := fake.listCount(prListRepoA); got != 0 {
		t.Fatalf("a read finished before the answer: %d", got)
	}
	close(release)
	read := awaitPRListRead(t, events)
	if rows := prListProject(t, read, "p-a").Rows; len(rows) != 1 || rows[0].Ref.Number != 2 || read.Seq <= snapshot.Seq {
		t.Fatalf("read = %+v (seq %d after %d)", rows, read.Seq, snapshot.Seq)
	}

	fresh := newTestAppWithStore(t)
	fresh.prUpdates.interval = time.Hour
	freshFake := newPRListFake(t, fresh)
	freshFake.set(func(f *prListFake) { f.projects = f.projects[:1] })
	storePRList(t, fresh, "p-a", prListRepoA, nil, time.Minute)
	if sub := subscribePRList(t, fresh); sub.Snapshot.Stale {
		t.Fatalf("a stored copy younger than the interval answered stale: %+v", sub.Snapshot)
	}
}

// A tick publishes each project, and stores it, as its read lands: the
// project whose read is held does not hold back the other, and the frames
// stay Stale until every project of the first tick landed.
func TestPRListTickPublishesEachProjectAsItLands(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	release := make(chan struct{})
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.ViewerReviewState = "unreviewed" })}
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 7, nil)}
		f.listGate = func(repo gitops.RepoReference) {
			if repo == prListRepoB {
				<-release
			}
		}
	})
	events := capturePRList(t, app)
	subscribePRList(t, app)
	first := awaitPRList(t, events, "project A alone", func(s PRListSnapshot) bool {
		return len(prListProject(t, s, "p-a").Rows) == 1
	})
	if !first.Stale || len(prListProject(t, first, "p-b").Rows) != 0 || prListProject(t, first, "p-a").ReadAt == "" {
		t.Fatalf("frame before B landed = %+v", first)
	}
	select {
	case badge := <-events.badges:
		if badge != 1 {
			t.Fatalf("badge after A's write = %d, want 1", badge)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no pulls:badge after A's read was stored")
	}
	if keys := storedPRListKeys(t, app); !slices.Equal(keys, []string{prListRepoA.Key()}) {
		t.Fatalf("stored before B landed = %v", keys)
	}
	close(release)
	last := awaitPRListRead(t, events)
	if len(prListProject(t, last, "p-b").Rows) != 1 || last.Seq <= first.Seq {
		t.Fatalf("last frame = %+v", last)
	}
	if keys := storedPRListKeys(t, app); len(keys) != 2 {
		t.Fatalf("stored after both landed = %v", keys)
	}
}

// The subscribe's first tick reads each repository once however many
// projects clone it, as the user's action; a joiner is answered with the
// pump's last frame and reads nothing.
func TestPRListSubscribeReadsEachRepositoryOnceAndJoinersShareIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		// A second clone of the same repository is a second project.
		f.projects = append(f.projects, ForgeProject{ProjectID: "p-a2", Name: "Widgets clone", Path: "/w2", Repo: prListRepoA})
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{
			prListRow(prListRepoA, 1, nil),
			prListRow(prListRepoA, 2, func(r *gitops.PRListRow) { r.ReviewRequested, r.ViewerReviewState = true, "unreviewed" }),
			prListRow(prListRepoA, 3, func(r *gitops.PRListRow) { r.ViewerIsAuthor, r.MergeStatus = true, "clean" }),
		}
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 7, func(r *gitops.PRListRow) { r.Draft = true })}
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	snapshot := awaitPRListRead(t, events)
	if got := fake.listCount(prListRepoA); got != 1 {
		t.Fatalf("repository A read %d times, want 1 for its two projects", got)
	}
	fake.set(func(f *prListFake) {
		if !slices.Equal(f.interactive[prListRepoA.Key()], []bool{true}) {
			t.Errorf("the subscribe's read admission = %v, want interactive", f.interactive[prListRepoA.Key()])
		}
	})
	if snapshot.NeedsYou != 2 {
		t.Fatalf("needs you = %d, want 2", snapshot.NeedsYou)
	}
	var order []string
	for _, row := range prListProject(t, snapshot, "p-a").Rows {
		order = append(order, fmt.Sprintf("%d:%s", row.Ref.Number, row.State))
	}
	if want := []string{"2:your_turn", "3:ready", "1:open"}; !slices.Equal(order, want) {
		t.Fatalf("rows = %v, want %v", order, want)
	}
	if clone := prListProject(t, snapshot, "p-a2"); len(clone.Rows) != 3 || clone.Name != "Widgets clone" {
		t.Fatalf("the clone = %+v", clone)
	}
	if b := prListProject(t, snapshot, "p-b"); len(b.Rows) != 1 || b.Rows[0].State != PRStateDraft || b.Rows[0].Group != PRGroupWaiting {
		t.Fatalf("project B = %+v", b)
	}

	joiner := subscribePRList(t, app)
	if joiner.ID == sub.ID || joiner.Snapshot.Seq != snapshot.Seq || joiner.Snapshot.Stale {
		t.Fatalf("joiner = %s seq %d, want a new id on seq %d", joiner.ID, joiner.Snapshot.Seq, snapshot.Seq)
	}
	if got := fake.listCount(prListRepoA); got != 1 {
		t.Fatalf("the joiner read the forge: %d reads", got)
	}
}

// The last unsubscribe stops the pump and drops what it held; the next
// subscribe starts a new pump and answers from the store.
func TestPRListPumpHoldsNothingAfterTheLastUnsubscribe(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 1, nil)}
	})
	events := capturePRList(t, app)
	sub, err := app.SubscribePRList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	read := awaitPRListRead(t, events)
	app.prUpdates.mu.Lock()
	pump := app.prUpdates.list
	app.prUpdates.mu.Unlock()
	if err := app.UnsubscribePRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	app.prUpdates.wg.Wait()
	if prListPumpLive(app) {
		t.Fatal("the pump survived its last subscriber")
	}
	app.prUpdates.mu.Lock()
	held := pump.projects != nil || pump.encoded != nil || pump.snapshot.Seq != 0 || pump.forge != nil || app.prUpdates.list != nil
	app.prUpdates.mu.Unlock()
	if held {
		t.Fatalf("the stopped pump still holds state: projects %v encoded %d bytes snapshot seq %d", pump.projects, len(pump.encoded), pump.snapshot.Seq)
	}

	fake.set(func(f *prListFake) { f.listGate = func(gitops.RepoReference) { time.Sleep(50 * time.Millisecond) } })
	again := subscribePRList(t, app)
	if rows := prListProject(t, again.Snapshot, "p-a").Rows; len(rows) != 1 || again.Snapshot.Seq <= read.Seq {
		t.Fatalf("the next subscribe = %+v, want the stored rows", again.Snapshot)
	}
}

func TestPRListPausesWhileHiddenAndCatchesUpOnResume(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = 5 * time.Millisecond
	fake := newPRListFake(t, app)
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	awaitPRListRead(t, events)

	if err := app.SetPRListActive(context.Background(), sub.ID, false); err != nil {
		t.Fatal(err)
	}
	// A tick already past its pause check may finish; after it nothing.
	time.Sleep(20 * time.Millisecond)
	before := fake.listCount(prListRepoA)
	time.Sleep(60 * time.Millisecond)
	if got := fake.listCount(prListRepoA); got != before {
		t.Fatalf("hidden list read the forge %d times", got-before)
	}

	fake.set(func(f *prListFake) { f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 4, nil)} })
	if err := app.SetPRListActive(context.Background(), sub.ID, true); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the row read after resuming", func(s PRListSnapshot) bool {
		return len(prListProject(t, s, "p-a").Rows) == 1
	})
}

// A read whose detail request failed lists its rows with the failure on
// the project, stored like any read; the next clean read clears it.
func TestPRListDetailFailureListsTheRowsBesideIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 2, func(r *gitops.PRListRow) { r.ViewerIsReviewer = true })}
		f.listErr[prListRepoB.Key()] = &gitops.PRListDetailError{Errs: []error{&forgeapi.TransientError{Err: errors.New("events: connection reset")}}}
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	read := awaitPRListRead(t, events)
	b := prListProject(t, read, "p-b")
	if b.DetailError == "" || b.Error != "" || len(b.Rows) != 1 || b.Rows[0].State != PRStateYourTurn {
		t.Fatalf("project = %+v", b)
	}
	if strings.Contains(b.DetailError, "connection reset") {
		t.Fatalf("the detail failure leaked the raw error: %q", b.DetailError)
	}
	stored, err := app.store.ListPRListProjects()
	if err != nil || !strings.Contains(string(stored[prListRepoB.Key()].ProjectJSON), `"detailError"`) {
		t.Fatalf("stored read = %s, %v", stored[prListRepoB.Key()].ProjectJSON, err)
	}
	fake.set(func(f *prListFake) { delete(f.listErr, prListRepoB.Key()) })
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if b := prListProject(t, prListSnapshotNow(app), "p-b"); b.DetailError != "" {
		t.Fatalf("a clean read kept the detail failure: %+v", b)
	}
}

// A failing project keeps the rows last read beside its error and leaves
// the other projects alone; a transient failure is held back once. A
// removed project takes its rows along, in the pump and in the store.
func TestPRListProjectFailureKeepsRowsAndIsolatesProjects(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 1, nil)}
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 2, nil)}
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	awaitPRListRead(t, events)

	fake.set(func(f *prListFake) {
		f.listErr[prListRepoB.Key()] = &forgeapi.TransientError{Err: errors.New("dial refused")}
		f.rows[prListRepoA.Key()] = append(f.rows[prListRepoA.Key()], prListRow(prListRepoA, 3, nil))
	})
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := prListSnapshotNow(app)
	if b := prListProject(t, snapshot, "p-b"); b.Error != "" || len(b.Rows) != 1 {
		t.Fatalf("a first transient failure was reported: %+v", b)
	}
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	snapshot = prListSnapshotNow(app)
	b := prListProject(t, snapshot, "p-b")
	if b.ErrorKind != forgeFailureTransient || b.Error == "" || len(b.Rows) != 1 || b.Rows[0].Ref.Number != 2 {
		t.Fatalf("failing project = %+v", b)
	}
	if a := prListProject(t, snapshot, "p-a"); a.Error != "" || len(a.Rows) != 2 {
		t.Fatalf("healthy project = %+v", a)
	}

	fake.set(func(f *prListFake) { delete(f.listErr, prListRepoB.Key()) })
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if b := prListProject(t, prListSnapshotNow(app), "p-b"); b.Error != "" || b.ErrorKind != "" {
		t.Fatalf("recovered project still fails: %+v", b)
	}

	fake.set(func(f *prListFake) { f.projects = f.projects[:1] })
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	snapshot = prListSnapshotNow(app)
	if len(snapshot.Projects) != 1 {
		t.Fatalf("projects after removal = %+v", snapshot.Projects)
	}
	app.prUpdates.mu.Lock()
	_, kept := app.prUpdates.list.projects[prListRepoB.Key()]
	app.prUpdates.mu.Unlock()
	if kept {
		t.Fatal("the removed project's rows were kept")
	}
	if keys := storedPRListKeys(t, app); !slices.Equal(keys, []string{prListRepoA.Key()}) {
		t.Fatalf("stored after removal = %v, want the removed repository's row deleted", keys)
	}
}

// A failed project carries the correlation id and the forge's raw text
// behind its sentence, fixed while the same failure repeats, so a repeat
// publishes nothing new; recovery clears it.
func TestPRListProjectFailureCarriesItsDetails(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 2, nil)}
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	awaitPRListRead(t, events)

	refused := &forgeapi.ForbiddenError{
		Status:  &forgeapi.StatusError{Method: "GET", URL: "https://gitlab.com/api/v4/projects/team%2Fapp/merge_requests", Status: 403, Body: `{"error":"insufficient_scope"}`},
		Scope:   "api",
		Message: "insufficient_scope",
	}
	fake.set(func(f *prListFake) { f.listErr[prListRepoB.Key()] = refused })
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := prListSnapshotNow(app)
	b := prListProject(t, snapshot, "p-b")
	if b.ErrorKind != forgeFailureForbidden || b.ErrorDetails == nil || len(b.Rows) != 1 {
		t.Fatalf("failing project = %+v", b)
	}
	detail := *b.ErrorDetails
	if detail.Ref == "" || detail.Method == "" || detail.At == 0 {
		t.Fatalf("detail = %+v", detail)
	}
	if chain := strings.Join(detail.Chain, "\n"); !strings.Contains(chain, "insufficient_scope") || !strings.Contains(chain, "HTTP 403") {
		t.Fatalf("detail chain lacks the forge's text: %q", detail.Chain)
	}

	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	again := prListSnapshotNow(app)
	if got := prListProject(t, again, "p-b").ErrorDetails; got == nil || got.Ref != detail.Ref || got.At != detail.At {
		t.Fatalf("a repeated failure moved its detail: %+v, was %+v", got, detail)
	}
	if again.Seq != snapshot.Seq {
		t.Fatalf("a repeated failure published a new snapshot: seq %d, was %d", again.Seq, snapshot.Seq)
	}

	fake.set(func(f *prListFake) { delete(f.listErr, prListRepoB.Key()) })
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if b := prListProject(t, prListSnapshotNow(app), "p-b"); b.ErrorDetails != nil {
		t.Fatalf("recovered project keeps its details: %+v", b.ErrorDetails)
	}
}

// A project whose origin could not be read is listed with its error,
// beside the rows its repository's stored read holds.
func TestPRListReportsAProjectWhoseOriginFailed(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	repoC := gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "broken"}
	storePRList(t, app, "p-c", repoC, []gitops.PRListRow{prListRow(repoC, 5, nil)}, time.Hour)
	fake.set(func(f *prListFake) {
		f.projects = append(f.projects, ForgeProject{ProjectID: "p-c", Name: "Broken", Path: "/c", Error: "origin unreadable"})
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	for _, snapshot := range []PRListSnapshot{sub.Snapshot, awaitPRListRead(t, events)} {
		c := prListProject(t, snapshot, "p-c")
		if c.Error != "origin unreadable" || c.ErrorKind != forgeFailureForge || len(c.Rows) != 1 || c.Repo != repoC {
			t.Fatalf("project = %+v", c)
		}
	}
	if fake.listCount(repoC) != 0 {
		t.Fatal("the unreadable origin's repository was read")
	}
	if keys := storedPRListKeys(t, app); !slices.Contains(keys, repoC.Key()) {
		t.Fatalf("stored = %v, want the unreadable project's rows kept", keys)
	}
}

// The badge before the page opens counts the stored copy's needs-you
// rows for the current projects, each PR once, with no forge call; a
// repository no project names does not count.
func TestGetPRListNeedsYouAnswersFromTheStore(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.projects = append(f.projects, ForgeProject{ProjectID: "p-a2", Name: "Clone", Path: "/w2", Repo: prListRepoA})
		f.listGate = func(gitops.RepoReference) { t.Error("GetPRListNeedsYou read the forge") }
	})
	mine := func(r *gitops.PRListRow) { r.ViewerIsAuthor, r.MergeStatus = true, "clean" }
	storePRList(t, app, "p-a", prListRepoA, []gitops.PRListRow{prListRow(prListRepoA, 1, mine), prListRow(prListRepoA, 2, nil)}, time.Hour)
	gone := gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "gone"}
	storePRList(t, app, "p-gone", gone, []gitops.PRListRow{prListRow(gone, 3, mine)}, time.Hour)
	got, err := app.GetPRListNeedsYou(context.Background())
	if err != nil || got != 1 {
		t.Fatalf("GetPRListNeedsYou = %d, %v; want 1", got, err)
	}
	if prListPumpLive(app) {
		t.Fatal("GetPRListNeedsYou started a pump")
	}
}

// The boot prefetch reads every project with background admission,
// stores each read, emits pulls:badge, prunes repositories no project
// names and holds nothing; it reads nothing while the stored copy is
// younger than the interval or a pump runs.
func TestPRListPrefetchWritesTheStoreAndSkipsWhenFresh(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.ViewerReviewState = "unreviewed" })}
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 2, nil)}
	})
	gone := gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "gone"}
	storePRList(t, app, "p-gone", gone, nil, 2*time.Hour)
	events := capturePRList(t, app)
	stored, err := app.prefetchPRList(context.Background())
	if err != nil || stored != 2 {
		t.Fatalf("prefetch = %d, %v", stored, err)
	}
	if keys := storedPRListKeys(t, app); !slices.Equal(keys, []string{prListRepoA.Key(), prListRepoB.Key()}) {
		t.Fatalf("stored = %v", keys)
	}
	fake.set(func(f *prListFake) {
		if !slices.Equal(f.interactive[prListRepoA.Key()], []bool{false}) {
			t.Errorf("prefetch admission = %v, want background", f.interactive[prListRepoA.Key()])
		}
	})
	var badges []int
	for len(badges) < 2 {
		select {
		case badge := <-events.badges:
			badges = append(badges, badge)
		case <-time.After(3 * time.Second):
			t.Fatalf("badges = %v, want one per stored read", badges)
		}
	}
	if badges[1] != 1 {
		t.Fatalf("badges = %v, want the last to count the review asked of the user", badges)
	}
	if prListPumpLive(app) || app.prUpdates.list != nil {
		t.Fatal("the prefetch left a pump")
	}
	if got, err := app.prefetchPRList(context.Background()); err != nil || got != 0 || fake.listCount(prListRepoA) != 1 {
		t.Fatalf("a prefetch over a fresh copy = %d, %v, reads %d", got, err, fake.listCount(prListRepoA))
	}

	live := newTestAppWithStore(t)
	live.prUpdates.interval = time.Hour
	liveFake := newPRListFake(t, live)
	hold := make(chan struct{})
	liveFake.set(func(f *prListFake) { f.listGate = func(gitops.RepoReference) { <-hold } })
	subscribePRList(t, live)
	t.Cleanup(func() { close(hold) })
	if got, err := live.prefetchPRList(context.Background()); err != nil || got != 0 {
		t.Fatalf("a prefetch beside a live pump = %d, %v", got, err)
	}
}

func TestUnsubscribePRListUnbindsItsConnectionTie(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	newPRListFake(t, app)
	checkByIDTie(t, byIDTie{
		subscribe: func(ctx context.Context) (string, error) {
			sub, err := app.SubscribePRList(ctx)
			return sub.ID, err
		},
		unsubscribe: app.UnsubscribePRList,
		key:         prListCleanupKey,
		live: func(id string) bool {
			app.prUpdates.mu.Lock()
			defer app.prUpdates.mu.Unlock()
			_, ok := app.prUpdates.listHandles[id]
			return ok
		},
	})
	app.prUpdates.wg.Wait()
}
