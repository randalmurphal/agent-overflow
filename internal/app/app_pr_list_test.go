package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

var (
	prListRepoA = gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets"}
	prListRepoB = gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "team", Repo: "app"}
)

// prListFake stands in for the forge behind the list pump: each
// repository's rows and failure, each PR's detail, and the reads made with
// their admission (interactive). With reserve set, a background detail
// read is refused as the transport refuses one near the rate limit
// reserve. listGate, when set, runs before each list read, outside the
// fake's lock, so a test can hold a tick inside its reads.
type prListFake struct {
	mu          sync.Mutex
	projects    []ForgeProject
	rows        map[string][]gitops.PRListRow
	listErr     map[string]error
	details     map[string]gitops.PRDetail
	lists       map[string]int
	reads       map[string]int
	interactive map[string][]bool
	reserve     bool
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
		details:     map[string]gitops.PRDetail{},
		lists:       map[string]int{},
		reads:       map[string]int{},
		interactive: map[string][]bool{},
	}
	// Registered before any subscription, so it runs after their release.
	t.Cleanup(app.prUpdates.wg.Wait)
	app.prUpdates.listProjectsFn = func(context.Context) ([]ForgeProject, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(f.projects), nil
	}
	app.prUpdates.listFetchFn = func(_ context.Context, repo gitops.RepoReference) ([]gitops.PRListRow, error) {
		f.mu.Lock()
		gate := f.listGate
		f.mu.Unlock()
		if gate != nil {
			gate(repo)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lists[repo.Key()]++
		if err := f.listErr[repo.Key()]; err != nil {
			return nil, err
		}
		return slices.Clone(f.rows[repo.Key()]), nil
	}
	app.prUpdates.listDetailFn = func(ctx context.Context, pr gitops.PRReference) (gitops.PRDetail, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reads[pr.Key()]++
		interactive := forgeapi.IsInteractive(ctx)
		f.interactive[pr.Key()] = append(f.interactive[pr.Key()], interactive)
		if f.reserve && !interactive {
			return gitops.PRDetail{}, &forgeapi.RateLimitedError{Host: pr.Host, Pool: "graphql", Until: time.Now().Add(time.Hour), Reserve: true}
		}
		return f.details[pr.Key()], nil
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

func (f *prListFake) readCount(pr gitops.PRReference) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[pr.Key()]
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

func capturePRList(t *testing.T, app *App) chan PRListSnapshot {
	t.Helper()
	events := make(chan PRListSnapshot, 64)
	app.testEmitHook = func(name string, data any) {
		if name != "pulls:updated" {
			return
		}
		snapshot, ok := data.(PRListSnapshot)
		if !ok {
			t.Errorf("pulls:updated payload type = %T", data)
			return
		}
		events <- snapshot
	}
	return events
}

// awaitPRList waits for a frame that satisfies ok, skipping the others.
func awaitPRList(t *testing.T, events <-chan PRListSnapshot, why string, ok func(PRListSnapshot) bool) PRListSnapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case snapshot := <-events:
			if ok(snapshot) {
				return snapshot
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", why)
			return PRListSnapshot{}
		}
	}
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

// The subscribe answers with every project's rows from one read per
// repository, attention applied and sorted; a joiner reads nothing.
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
			prListRow(prListRepoA, 2, func(r *gitops.PRListRow) { r.ReviewRequested = true }),
			prListRow(prListRepoA, 3, func(r *gitops.PRListRow) { r.ViewerIsAuthor, r.MergeStatus = true, "clean" }),
		}
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 7, func(r *gitops.PRListRow) { r.Draft = true })}
		// The user's own row is read for its detail at once; the fake's
		// default (a zero detail) would read as not ready.
		f.details[prListRow(prListRepoA, 3, nil).Ref.Key()] = gitops.PRDetail{Readiness: gitops.MergeReadiness{State: gitops.MergeStateReady}}
	})

	sub := subscribePRList(t, app)
	if sub.Snapshot.Seq == 0 || sub.Snapshot.FetchedAt == "" {
		t.Fatalf("snapshot was not stamped: %+v", sub.Snapshot)
	}
	if got := fake.listCount(prListRepoA); got != 1 {
		t.Fatalf("repository A read %d times, want 1 for its two projects", got)
	}
	if sub.Snapshot.NeedsYou != 2 {
		t.Fatalf("needs you = %d, want 2", sub.Snapshot.NeedsYou)
	}
	a := prListProject(t, sub.Snapshot, "p-a")
	var order []string
	for _, row := range a.Rows {
		order = append(order, fmt.Sprintf("%d:%s", row.Ref.Number, row.State))
	}
	if want := []string{"2:your_turn", "3:ready", "1:open"}; !slices.Equal(order, want) {
		t.Fatalf("rows = %v, want %v", order, want)
	}
	if clone := prListProject(t, sub.Snapshot, "p-a2"); len(clone.Rows) != 3 {
		t.Fatalf("the clone's rows = %d, want 3", len(clone.Rows))
	}
	if b := prListProject(t, sub.Snapshot, "p-b"); len(b.Rows) != 1 || b.Rows[0].State != PRStateDraft || b.Rows[0].Group != PRGroupWaiting {
		t.Fatalf("project B = %+v", b)
	}

	joiner := subscribePRList(t, app)
	if joiner.ID == sub.ID || joiner.Snapshot.Seq != sub.Snapshot.Seq {
		t.Fatalf("joiner = %s seq %d, want a new id on seq %d", joiner.ID, joiner.Snapshot.Seq, sub.Snapshot.Seq)
	}
	if got := fake.listCount(prListRepoA); got != 1 {
		t.Fatalf("the joiner read the forge: %d reads", got)
	}

	if err := app.UnsubscribePRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if !prListPumpLive(app) {
		t.Fatal("the pump stopped with a subscriber left")
	}
	if err := app.UnsubscribePRList(context.Background(), joiner.ID); err != nil {
		t.Fatal(err)
	}
	if prListPumpLive(app) {
		t.Fatal("the pump survived its last subscriber")
	}
	app.prUpdates.wg.Wait()
}

func TestPRListPausesWhileHiddenAndCatchesUpOnResume(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = 5 * time.Millisecond
	fake := newPRListFake(t, app)
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)

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

func TestPRListFocusIsBoundedAndReportsUnknownKeys(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	row := prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.ViewerIsAuthor = true })
	unresolved := 2
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{row}
		f.details[row.Ref.Key()] = gitops.PRDetail{Readiness: gitops.MergeReadiness{
			State: gitops.MergeStateReady, Approvals: gitops.ApprovalCount{Given: 1, Required: 1}, Unresolved: &unresolved,
		}}
	})
	capturePRList(t, app)
	sub := subscribePRList(t, app)
	// The user's own row needs its detail for its state whether or not it
	// is on screen: the first tick reads it.
	if got := prListProject(t, sub.Snapshot, "p-a").Rows[0]; got.Detail == nil || got.State != PRStateReady {
		t.Fatalf("own row before focus = %+v", got)
	}

	tooMany := make([]string, maxPRListFocus+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("k%d", i)
	}
	if _, err := app.SetPRListFocus(context.Background(), sub.ID, tooMany); err == nil {
		t.Fatal("a focus over the bound was accepted")
	}
	result, err := app.SetPRListFocus(context.Background(), sub.ID, []string{row.Ref.Key(), "github:github.com:acme/widgets#99"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Unknown, []string{"github:github.com:acme/widgets#99"}) {
		t.Fatalf("unknown = %v", result.Unknown)
	}
	snapshot := sub.Snapshot
	got := prListProject(t, snapshot, "p-a").Rows[0]
	if got.State != PRStateReady || got.Detail.ApprovalsGiven != 1 || got.Detail.ApprovalsRequired != 1 || got.Detail.Unresolved != 2 {
		t.Fatalf("focused row = %+v detail %+v", got, got.Detail)
	}
	if snapshot.NeedsYou != 1 {
		t.Fatalf("needs you = %d, want 1", snapshot.NeedsYou)
	}
	if _, err := app.SetPRListFocus(context.Background(), "nope", nil); !errors.Is(err, errPRUpdatesUnknownSubscription) {
		t.Fatalf("unknown subscription: %v", err)
	}
}

// Someone else's row the user does not review is read only once a
// subscriber focuses it (it is on screen): its state at list stage needs
// no detail.
func TestPRListReadsAnotherPersonsRowOnlyOnFocus(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	row := prListRow(prListRepoA, 1, nil)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{row}
		f.details[row.Ref.Key()] = gitops.PRDetail{Readiness: gitops.MergeReadiness{State: gitops.MergeStateReady}}
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	if got := prListProject(t, sub.Snapshot, "p-a").Rows[0]; got.Detail != nil || fake.readCount(row.Ref) != 0 {
		t.Fatalf("row before focus = %+v (reads %d)", got, fake.readCount(row.Ref))
	}
	if _, err := app.SetPRListFocus(context.Background(), sub.ID, []string{row.Ref.Key()}); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the focused row's detail", func(s PRListSnapshot) bool {
		return prListProject(t, s, "p-a").Rows[0].Detail != nil
	})
}

// A focused detail is read again only when its list row changed or the
// user refreshed: the GitHub GraphQL budget cannot afford a read per row
// per tick.
func TestPRListDetailIsReadAgainOnlyWhenTheRowChangesOrOnRefresh(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = 5 * time.Millisecond
	fake := newPRListFake(t, app)
	row := prListRow(prListRepoA, 1, nil)
	fake.set(func(f *prListFake) { f.rows[prListRepoA.Key()] = []gitops.PRListRow{row} })
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)
	if _, err := app.SetPRListFocus(context.Background(), sub.ID, []string{row.Ref.Key()}); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the detail", func(s PRListSnapshot) bool { return prListProject(t, s, "p-a").Rows[0].Detail != nil })
	lists := fake.listCount(prListRepoA)
	for fake.listCount(prListRepoA) < lists+5 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fake.readCount(row.Ref); got != 1 {
		t.Fatalf("an unchanged row's detail was read %d times", got)
	}

	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.Comments = 3 })}
	})
	for fake.readCount(row.Ref) < 2 {
		time.Sleep(5 * time.Millisecond)
	}

	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := fake.readCount(row.Ref); got != 3 {
		t.Fatalf("detail reads after a refresh = %d, want 3", got)
	}
}

// A failing project keeps the rows last read beside its error and leaves
// the other projects alone; a transient failure is held back once.
func TestPRListProjectFailureKeepsRowsAndIsolatesProjects(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 1, nil)}
		f.rows[prListRepoB.Key()] = []gitops.PRListRow{prListRow(prListRepoB, 2, nil)}
	})
	sub := subscribePRList(t, app)

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

	// A project removed takes its rows along.
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
	sub := subscribePRList(t, app)

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
	if b.ErrorKind != forgeFailureForbidden || b.ErrorDetails == nil {
		t.Fatalf("failing project = %+v", b)
	}
	detail := *b.ErrorDetails
	if detail.Ref == "" || detail.Method == "" || detail.At == 0 {
		t.Fatalf("detail = %+v", detail)
	}
	if chain := strings.Join(detail.Chain, "\n"); !strings.Contains(chain, "insufficient_scope") || !strings.Contains(chain, "HTTP 403") {
		t.Fatalf("detail chain lacks the forge's text: %q", detail.Chain)
	}

	// The same failure again: the detail and the snapshot stay put.
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

// A project whose origin could not be read is listed with its error.
func TestPRListReportsAProjectWhoseOriginFailed(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	fake.set(func(f *prListFake) {
		f.projects = append(f.projects, ForgeProject{ProjectID: "p-c", Name: "Broken", Path: "/c", Error: "origin unreadable"})
	})
	sub := subscribePRList(t, app)
	c := prListProject(t, sub.Snapshot, "p-c")
	if c.Error != "origin unreadable" || c.ErrorKind != forgeFailureForge || len(c.Rows) != 0 {
		t.Fatalf("project = %+v", c)
	}
}

func prListSnapshotNow(app *App) PRListSnapshot {
	app.prUpdates.mu.Lock()
	defer app.prUpdates.mu.Unlock()
	return app.prUpdates.list.snapshot
}

// Opening a PR records it and the list shows what changed since.
func TestPRViewMarkedShowsSeenAndSinceInTheList(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	row := prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.Comments, r.Commits = 2, 3 })
	fake.set(func(f *prListFake) { f.rows[prListRepoA.Key()] = []gitops.PRListRow{row} })
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)

	if _, err := app.MarkPRViewed(context.Background(), row.Ref, "not a sha"); err == nil {
		t.Fatal("a head that is no commit id was recorded")
	}
	seen, err := app.MarkPRViewed(context.Background(), row.Ref, row.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if seen.HeadSHA != row.HeadSHA || seen.Comments == nil || *seen.Comments != 2 || seen.Commits == nil || *seen.Commits != 3 || seen.OpenedAt == 0 {
		t.Fatalf("seen = %+v", seen)
	}
	snapshot := awaitPRList(t, events, "the view in the list", func(s PRListSnapshot) bool {
		return prListProject(t, s, "p-a").Rows[0].Seen != nil
	})
	got := prListProject(t, snapshot, "p-a").Rows[0]
	if got.Since == nil || got.Since.HeadMoved || got.Since.NewComments != 0 || got.Since.NewCommits != 0 {
		t.Fatalf("since right after opening = %+v", got.Since)
	}

	fake.set(func(f *prListFake) {
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{prListRow(prListRepoA, 1, func(r *gitops.PRListRow) {
			r.Comments, r.Commits, r.HeadSHA = 5, 4, fmt.Sprintf("%040x", 99)
		})}
	})
	if err := app.RefreshPRList(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	got = prListProject(t, prListSnapshotNow(app), "p-a").Rows[0]
	if got.Since == nil || !got.Since.HeadMoved || got.Since.NewComments != 3 || got.Since.NewCommits != 1 {
		t.Fatalf("since after a push and comments = %+v", got.Since)
	}

	// A PR the list holds no row for records no counts.
	other := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 42}
	seen, err = app.MarkPRViewed(context.Background(), other, "abc123")
	if err != nil || seen.Comments != nil || seen.Commits != nil {
		t.Fatalf("seen without a row = %+v, %v", seen, err)
	}
}

// More own rows than the detail allowance across two repositories: every
// tick picks the same newest maxPRListFocus by update time (GitLab's
// times carry milliseconds), so an unchanged row's detail is read once
// and none is dropped and read again.
func TestPRListDetailSelectionIsStableAcrossTicks(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = 2 * time.Millisecond
	fake := newPRListFake(t, app)
	want := map[string]bool{}
	fake.set(func(f *prListFake) {
		for i := 1; i <= 40; i++ {
			row := prListRow(prListRepoA, i, func(r *gitops.PRListRow) {
				r.ViewerIsAuthor, r.UpdatedAt = true, fmt.Sprintf("2026-01-01T00:00:%02dZ", i)
			})
			f.rows[prListRepoA.Key()] = append(f.rows[prListRepoA.Key()], row)
			if i >= 12 {
				want[row.Ref.Key()] = true
			}
		}
		for i := 1; i <= 41; i++ {
			row := prListRow(prListRepoB, i, func(r *gitops.PRListRow) {
				r.ViewerIsReviewer, r.UpdatedAt = true, fmt.Sprintf("2026-01-01T00:00:%02d.500Z", i)
			})
			f.rows[prListRepoB.Key()] = append(f.rows[prListRepoB.Key()], row)
			if i >= 11 {
				want[row.Ref.Key()] = true
			}
		}
	})
	if len(want) != maxPRListFocus {
		t.Fatalf("the test expects %d rows, has %d", maxPRListFocus, len(want))
	}
	subscribePRList(t, app)
	for fake.listCount(prListRepoA) < 20 {
		time.Sleep(2 * time.Millisecond)
	}
	fake.mu.Lock()
	reads := maps.Clone(fake.reads)
	fake.mu.Unlock()
	for key, n := range reads {
		if !want[key] || n != 1 {
			t.Errorf("%s detail read %d times (in the newest set: %v)", key, n, want[key])
		}
	}
	if len(reads) != len(want) {
		t.Errorf("%d details read, want the %d newest", len(reads), len(want))
	}
}

// Only a visible subscriber's focus selects detail reads: hiding one
// gives the other's rows the allowance at once, and showing it again
// reads its rows back.
func TestPRListHiddenSubscriberFocusYieldsTheAllowance(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	var older, newer []string
	fake.set(func(f *prListFake) {
		for i := 1; i <= 2*maxPRListFocus; i++ {
			row := prListRow(prListRepoA, i, func(r *gitops.PRListRow) {
				r.UpdatedAt = time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)
			})
			f.rows[prListRepoA.Key()] = append(f.rows[prListRepoA.Key()], row)
			if i <= maxPRListFocus {
				older = append(older, row.Ref.Key())
			} else {
				newer = append(newer, row.Ref.Key())
			}
		}
	})
	events := capturePRList(t, app)
	a := subscribePRList(t, app)
	b := subscribePRList(t, app)
	detailed := func(s PRListSnapshot, keys []string) int {
		n := 0
		for _, row := range prListProject(t, s, "p-a").Rows {
			if slices.Contains(keys, row.Ref.Key()) && row.Detail != nil {
				n++
			}
		}
		return n
	}
	if _, err := app.SetPRListFocus(context.Background(), b.ID, newer); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the newer rows' detail", func(s PRListSnapshot) bool { return detailed(s, newer) == maxPRListFocus })
	// Both visible: the newer rows hold the allowance.
	if _, err := app.SetPRListFocus(context.Background(), a.ID, older); err != nil {
		t.Fatal(err)
	}

	if err := app.SetPRListActive(context.Background(), b.ID, false); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the visible subscriber's rows read once the other hid", func(s PRListSnapshot) bool {
		return detailed(s, older) == maxPRListFocus && detailed(s, newer) == 0
	})

	if err := app.SetPRListActive(context.Background(), b.ID, true); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the shown subscriber's rows read again", func(s PRListSnapshot) bool {
		return detailed(s, newer) == maxPRListFocus && detailed(s, older) == 0
	})
	fake.mu.Lock()
	for _, key := range newer {
		if n := fake.reads[key]; n != 2 {
			t.Errorf("%s detail read %d times, want 2", key, n)
		}
	}
	fake.mu.Unlock()

	// A visible subscriber that leaves gives its share back at once.
	if err := app.UnsubscribePRList(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the remaining subscriber's rows read once the other left", func(s PRListSnapshot) bool {
		return detailed(s, older) == maxPRListFocus && detailed(s, newer) == 0
	})

	// Nothing left to read: hiding still drops the hidden rows' detail.
	c := subscribePRList(t, app)
	if _, err := app.SetPRListFocus(context.Background(), c.ID, newer); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the newer rows read for the new subscriber", func(s PRListSnapshot) bool {
		return detailed(s, newer) == maxPRListFocus
	})
	if _, err := app.SetPRListFocus(context.Background(), a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := app.SetPRListActive(context.Background(), c.ID, false); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the hidden rows' detail dropped", func(s PRListSnapshot) bool {
		return detailed(s, newer) == 0 && detailed(s, older) == 0
	})
}

// The allowance a subscriber frees by hiding or leaving is read in the
// background, so near the rate limit reserve those reads are refused;
// the user's own focus change and showing the page read as interactive
// and pass the reserve.
func TestPRListFreedAllowanceReadsInTheBackground(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	var newest, older, oldest []string
	fake.set(func(f *prListFake) {
		for i := 1; i <= 3*maxPRListFocus; i++ {
			row := prListRow(prListRepoA, i, func(r *gitops.PRListRow) {
				r.UpdatedAt = time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)
			})
			f.rows[prListRepoA.Key()] = append(f.rows[prListRepoA.Key()], row)
			switch {
			case i <= maxPRListFocus:
				oldest = append(oldest, row.Ref.Key())
			case i <= 2*maxPRListFocus:
				older = append(older, row.Ref.Key())
			default:
				newest = append(newest, row.Ref.Key())
			}
		}
	})
	events := capturePRList(t, app)
	a := subscribePRList(t, app)
	b := subscribePRList(t, app)
	count := func(s PRListSnapshot, keys []string, has func(PRListItem) bool) int {
		n := 0
		for _, row := range prListProject(t, s, "p-a").Rows {
			if slices.Contains(keys, row.Ref.Key()) && has(row) {
				n++
			}
		}
		return n
	}
	detailed := func(row PRListItem) bool { return row.Detail != nil }
	refused := func(row PRListItem) bool { return row.Detail == nil && row.DetailError != "" }
	admissions := func(keys []string) (interactive, background int) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, key := range keys {
			for _, user := range fake.interactive[key] {
				if user {
					interactive++
				} else {
					background++
				}
			}
		}
		return interactive, background
	}

	// The user's focus reads as interactive. Once b's newer rows hold the
	// allowance (both visible) no wake is pending: the pass that read them
	// saw both focuses.
	if _, err := app.SetPRListFocus(context.Background(), a.ID, older); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SetPRListFocus(context.Background(), b.ID, newest); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the newest rows hold the allowance", func(s PRListSnapshot) bool {
		return count(s, newest, detailed) == maxPRListFocus && count(s, older, detailed) == 0
	})
	if user, background := admissions(newest); user != maxPRListFocus || background != 0 {
		t.Fatalf("focus reads: %d interactive, %d background", user, background)
	}
	fake.set(func(f *prListFake) { f.reserve = true })
	userBefore, backgroundBefore := admissions(older)
	if backgroundBefore != 0 {
		t.Fatalf("%d background reads before the reserve", backgroundBefore)
	}

	if err := app.SetPRListActive(context.Background(), b.ID, false); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the freed rows refused at the reserve once the other hid", func(s PRListSnapshot) bool {
		return count(s, older, refused) == maxPRListFocus
	})
	if user, background := admissions(older); user != userBefore || background != maxPRListFocus {
		t.Fatalf("reads after a hide: %d interactive, %d background", user-userBefore, background)
	}

	if err := app.SetPRListActive(context.Background(), b.ID, true); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the shown subscriber's rows read past the reserve", func(s PRListSnapshot) bool {
		return count(s, newest, detailed) == maxPRListFocus && count(s, older, refused) == 0
	})
	if user, background := admissions(newest); user != 2*maxPRListFocus || background != 0 {
		t.Fatalf("reads after a show: %d interactive, %d background", user, background)
	}

	if err := app.UnsubscribePRList(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the freed rows refused at the reserve once the other left", func(s PRListSnapshot) bool {
		return count(s, older, refused) == maxPRListFocus && count(s, newest, detailed) == 0
	})
	if user, background := admissions(older); user != userBefore || background != 2*maxPRListFocus {
		t.Fatalf("reads after a leave: %d interactive, %d background", user-userBefore, background)
	}

	// The user's own focus change still reads past the reserve.
	if _, err := app.SetPRListFocus(context.Background(), a.ID, oldest); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the user's focus read past the reserve", func(s PRListSnapshot) bool {
		return count(s, oldest, detailed) == maxPRListFocus
	})
	if user, background := admissions(oldest); user != maxPRListFocus || background != 0 {
		t.Fatalf("reads after a focus change: %d interactive, %d background", user, background)
	}
}

// A user's focus that lands while a background tick is reading the lists,
// after the tick took its admission and before it selects the rows, is
// selected by that tick and refused at the reserve. The refusal is not a
// read: the user's focus wake that follows reads those rows past it.
func TestPRListFocusRefusedByABackgroundTickIsReadByTheUsersWake(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = 20 * time.Millisecond
	fake := newPRListFake(t, app)
	var keys []string
	fake.set(func(f *prListFake) {
		for i := 1; i <= 5; i++ {
			row := prListRow(prListRepoA, i, nil)
			f.rows[prListRepoA.Key()] = append(f.rows[prListRepoA.Key()], row)
			keys = append(keys, row.Ref.Key())
		}
	})
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fake.set(func(f *prListFake) {
		f.reserve = true
		f.listGate = func(repo gitops.RepoReference) {
			if repo.Key() == prListRepoA.Key() {
				once.Do(func() {
					close(entered)
					<-release
				})
			}
		}
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no background tick read the lists")
	}
	if _, err := app.SetPRListFocus(context.Background(), sub.ID, keys); err != nil {
		t.Fatal(err)
	}
	close(release)

	awaitPRList(t, events, "the focused rows read past the reserve", func(s PRListSnapshot) bool {
		n := 0
		for _, row := range prListProject(t, s, "p-a").Rows {
			if slices.Contains(keys, row.Ref.Key()) && row.Detail != nil {
				n++
			}
		}
		return n == len(keys)
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, key := range keys {
		if got := fake.interactive[key]; !slices.Equal(got, []bool{false, true}) {
			t.Errorf("%s reads (interactive) = %v, want the tick's refused one, then the user's", key, got)
		}
	}
}

// A row a background tick refused at the reserve is unread: the user
// focusing it reads it past the reserve.
func TestPRListFocusOfARowRefusedAtTheReserveReadsIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = 20 * time.Millisecond
	fake := newPRListFake(t, app)
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)

	row := prListRow(prListRepoA, 1, func(r *gitops.PRListRow) { r.ViewerIsAuthor = true })
	key := row.Ref.Key()
	fake.set(func(f *prListFake) {
		f.reserve = true
		f.rows[prListRepoA.Key()] = []gitops.PRListRow{row}
	})
	awaitPRList(t, events, "the background tick's refusal", func(s PRListSnapshot) bool {
		rows := prListProject(t, s, "p-a").Rows
		return len(rows) == 1 && rows[0].Detail == nil && rows[0].DetailError != ""
	})
	if _, err := app.SetPRListFocus(context.Background(), sub.ID, []string{key}); err != nil {
		t.Fatal(err)
	}
	awaitPRList(t, events, "the focused row read past the reserve", func(s PRListSnapshot) bool {
		rows := prListProject(t, s, "p-a").Rows
		return len(rows) == 1 && rows[0].Detail != nil
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.interactive[key]; !slices.Equal(got, []bool{false, true}) {
		t.Errorf("%s reads (interactive) = %v, want the tick's refused one, then the user's", key, got)
	}
}

// A show that lands after the loop read the pause for a focus wake taken
// during it, and before the loop took that wake's admission, is the
// user's: the loop reads both under the lock the show changes them under,
// so the show waits for the paused wake to be dropped, then wakes the
// loop with the user's admission, and the focused rows read past the
// reserve. The hook starts the show at that boundary and gives it a
// moment to land, which only a split of the two reads lets it do.
func TestPRListShowDuringAPausedFocusWakeReadsAsTheUsers(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.prUpdates.interval = time.Hour
	fake := newPRListFake(t, app)
	var keys []string
	fake.set(func(f *prListFake) {
		for i := 1; i <= 3; i++ {
			row := prListRow(prListRepoA, i, nil)
			f.rows[prListRepoA.Key()] = append(f.rows[prListRepoA.Key()], row)
			keys = append(keys, row.Ref.Key())
		}
	})
	shown := make(chan error, 1)
	var hold sync.Mutex
	armed := false
	var subID string
	app.prUpdates.listFocusPauseHook = func() {
		hold.Lock()
		run, id := armed, subID
		armed = false
		hold.Unlock()
		if !run {
			return
		}
		done := make(chan struct{})
		go func() {
			shown <- app.SetPRListActive(context.Background(), id, true)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
		}
	}
	events := capturePRList(t, app)
	sub := subscribePRList(t, app)

	fake.set(func(f *prListFake) { f.reserve = true })
	if err := app.SetPRListActive(context.Background(), sub.ID, false); err != nil {
		t.Fatal(err)
	}
	hold.Lock()
	armed, subID = true, sub.ID
	hold.Unlock()
	// A focus while hidden wakes the loop, and the hook shows the page.
	if _, err := app.SetPRListFocus(context.Background(), sub.ID, keys); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-shown:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the focus wake never reached the pause read")
	}

	awaitPRList(t, events, "the focused rows read past the reserve", func(s PRListSnapshot) bool {
		n := 0
		for _, row := range prListProject(t, s, "p-a").Rows {
			if row.Detail != nil {
				n++
			}
		}
		return n == len(keys)
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, key := range keys {
		if got := fake.interactive[key]; !slices.Equal(got, []bool{true}) {
			t.Errorf("%s reads (interactive) = %v, want the user's alone", key, got)
		}
	}
}

// A user wake that arrives while a background one is pending is not
// downgraded: the merged wake reads as interactive.
func TestPRListFocusWakeKeepsTheUsersAdmission(t *testing.T) {
	t.Parallel()
	for _, order := range [][]bool{{false, true}, {true, false}, {true, true}} {
		pump := &prListPump{pumpCore: pumpCore{ctx: context.Background()}, focusWake: make(chan struct{}, 1)}
		for _, interactive := range order {
			pump.wakeFocusLocked(interactive)
		}
		<-pump.focusWake
		if !forgeapi.IsInteractive(pump.focusAdmissionLocked()) {
			t.Errorf("wakes %v read as background", order)
		}
		if forgeapi.IsInteractive(pump.focusAdmissionLocked()) {
			t.Errorf("wakes %v: the next wake inherited the user's admission", order)
		}
	}
	pump := &prListPump{pumpCore: pumpCore{ctx: context.Background()}, focusWake: make(chan struct{}, 1)}
	pump.wakeFocusLocked(false)
	<-pump.focusWake
	if forgeapi.IsInteractive(pump.focusAdmissionLocked()) {
		t.Error("a background wake read as interactive")
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
