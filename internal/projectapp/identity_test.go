package projectapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

type identityAnswer struct {
	identity gitops.RepoIdentity
	err      error
}

type identityAsk struct{ path string }

// fakeIdentity answers from a path → answer table (a missing path is not a
// repository) and records every question, so a test can assert what each
// derivation was told about the stored row.
type fakeIdentity struct {
	answers map[string]identityAnswer
	asked   []identityAsk
	// during, when set, runs inside each derivation before it answers.
	during func()
}

func (f *fakeIdentity) derive(_ context.Context, path string) (gitops.RepoIdentity, error) {
	f.asked = append(f.asked, identityAsk{path})
	if f.during != nil {
		f.during()
	}
	answer := f.answers[path]
	return answer.identity, answer.err
}

func repo(id, source string) identityAnswer {
	return identityAnswer{identity: gitops.RepoIdentity{Repository: true, RepositoryID: id, IdentitySource: source}}
}

func newIdentityService(t *testing.T, fake *fakeIdentity) (*Service, *store.Store) {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return New(Deps{
		Store:    database,
		Now:      func() time.Time { return time.UnixMilli(1234) },
		Identity: fake.derive,
	}), database
}

func mustDir(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir %s: %v", path, err)
	}
	return path
}

func identityOf(row store.Project) store.ProjectIdentity {
	return store.ProjectIdentity{RepositoryID: row.RepositoryID, IdentitySource: row.IdentitySource, Error: row.IdentityError}
}

func mustGet(t *testing.T, database *store.Store, id string) store.Project {
	t.Helper()
	row, err := database.GetProject(id)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	return row
}

func TestCreateStampsRepositoryIdentity(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:3", "aaaa1111")}}
	service, database := newIdentityService(t, fake)

	created, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := store.ProjectIdentity{RepositoryID: "github:github.com:3", IdentitySource: "aaaa1111"}
	if identityOf(created) != want || identityOf(mustGet(t, database, created.ID)) != want {
		t.Fatalf("created identity = %+v, want %+v stored and returned", identityOf(created), want)
	}
}

// A read git refuses is recorded with its reason on the created row, not
// stored as the empty identity of a plain folder.
func TestCreateRecordsAFailedIdentityRead(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: {err: errors.New("detected dubious ownership")}}}
	service, database := newIdentityService(t, fake)

	created, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.IdentityError != "detected dubious ownership" || mustGet(t, database, created.ID).IdentityError != created.IdentityError {
		t.Fatalf("created row = %+v, want the read failure recorded", created)
	}
}

// A directory that is not a repository is the ordinary non-git project. It
// gets an empty identity and no error.
func TestCreateWithoutAnIdentityDeriverStoresEmpty(t *testing.T) {
	service, _ := newTestService(t)
	path := mustDir(t, t.TempDir(), "plain")

	created, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if identityOf(created) != (store.ProjectIdentity{}) {
		t.Fatalf("identity = %+v, want empty with no deriver wired", identityOf(created))
	}
}

func TestEnsureForWorkspaceStampsIdentityOnTheCreatedRow(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:5", "bbbb2222")}}
	service, database := newIdentityService(t, fake)

	write, err := service.EnsureForWorkspace(path)
	if err != nil {
		t.Fatalf("EnsureForWorkspace: %v", err)
	}
	if !write.Changed {
		t.Fatal("first EnsureForWorkspace reported no creation")
	}
	want := store.ProjectIdentity{RepositoryID: "github:github.com:5", IdentitySource: "bbbb2222"}
	if identityOf(write.Project) != want || identityOf(mustGet(t, database, write.Project.ID)) != want {
		t.Fatalf("identity = %+v, want %+v returned and stored", identityOf(write.Project), want)
	}

	// Resolving to the existing row changes nothing and must not spend a
	// second derivation.
	asked := len(fake.asked)
	again, err := service.EnsureForWorkspace(path)
	if err != nil {
		t.Fatalf("EnsureForWorkspace (repeat): %v", err)
	}
	if again.Changed {
		t.Fatal("resolving an existing project reported a creation")
	}
	if len(fake.asked) != asked {
		t.Fatalf("derivations after the repeat = %d, want the original %d", len(fake.asked), asked)
	}
}

// Every row is re-read, and only the
// rows whose identity moved are written and announced.
func TestRefreshIdentityRereadsEveryRowAndAnnouncesOnlyMoves(t *testing.T) {
	parent := t.TempDir()
	unchanged := mustDir(t, parent, "unchanged")
	moved := mustDir(t, parent, "moved")
	archived := mustDir(t, parent, "archived")
	plain := mustDir(t, parent, "plain")

	fake := &fakeIdentity{answers: map[string]identityAnswer{
		unchanged: repo("github:github.com:6", "cccc3333"),
		moved:     repo("", "dddd4444"),
		archived:  repo("", "eeee5555"),
	}}
	service, database := newIdentityService(t, fake)
	rows := map[string]store.Project{}
	for _, path := range []string{unchanged, moved, archived, plain} {
		row, err := service.Create(path)
		if err != nil {
			t.Fatalf("Create %s: %v", path, err)
		}
		rows[path] = row
	}
	if _, _, err := database.ArchiveProject(rows[archived].ID); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}
	// An origin added after creation, on a live and an archived row.
	fake.answers[moved] = repo("github:github.com:2", "dddd4444")
	fake.answers[archived] = repo("github:github.com:1", "eeee5555")

	fake.asked = nil
	var announced []string
	if _, err := service.RefreshIdentity(context.Background(), func(row store.Project) {
		announced = append(announced, row.ID)
	}); err != nil {
		t.Fatalf("RefreshIdentity: %v", err)
	}

	if len(fake.asked) != len(rows) {
		t.Fatalf("derivations = %+v, want one per row", fake.asked)
	}
	seen := map[string]bool{}
	for _, ask := range fake.asked {
		if _, ok := rows[ask.path]; !ok || seen[ask.path] {
			t.Fatalf("unexpected repeated read: %+v", fake.asked)
		}
		seen[ask.path] = true
	}
	if len(announced) != 2 || !(announced[0] == rows[moved].ID || announced[1] == rows[moved].ID) ||
		!(announced[0] == rows[archived].ID || announced[1] == rows[archived].ID) {
		t.Fatalf("announced %v, want exactly the moved and archived rows", announced)
	}
	if got := mustGet(t, database, rows[moved].ID).RepositoryID; got != "github:github.com:2" {
		t.Fatalf("moved row ID = %q, want the newly verified identity", got)
	}
	if got := mustGet(t, database, rows[archived].ID).RepositoryID; got != "github:github.com:1" {
		t.Fatalf("archived row ID = %q, want the newly verified identity", got)
	}
}

// A failed read keeps the last good identity beside its error, so the
// project stays merged and says why it may be stale; the next good read
// clears the error.
func TestRefreshIdentityKeepsTheLastGoodIdentityOnFailure(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:4", "ffff6666")}}
	service, database := newIdentityService(t, fake)
	row, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fake.answers[path] = identityAnswer{err: errors.New("git rev-parse failed: boom")}
	if _, err := service.RefreshIdentity(context.Background(), nil); err != nil {
		t.Fatalf("RefreshIdentity: %v", err)
	}
	want := store.ProjectIdentity{RepositoryID: "github:github.com:4", IdentitySource: "ffff6666", Error: "git rev-parse failed: boom"}
	if got := identityOf(mustGet(t, database, row.ID)); got != want {
		t.Fatalf("after a failed read = %+v, want %+v", got, want)
	}

	fake.answers[path] = repo("github:github.com:4", "ffff6666")
	if _, err := service.RefreshIdentity(context.Background(), nil); err != nil {
		t.Fatalf("RefreshIdentity: %v", err)
	}
	if got := mustGet(t, database, row.ID).IdentityError; got != "" {
		t.Fatalf("error after a good read = %q, want cleared", got)
	}
}

// A checkout that is missing or no longer a repository keeps its identity:
// an unmounted volume is the same repository when it returns.
func TestRefreshIdentityKeepsTheIdentityOfAMissingCheckout(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:4", "abab1212")}}
	service, database := newIdentityService(t, fake)
	row, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	delete(fake.answers, path)
	if _, err := service.RefreshIdentity(context.Background(), func(store.Project) {
		t.Fatal("a missing checkout announced a change")
	}); err != nil {
		t.Fatalf("RefreshIdentity: %v", err)
	}
	if got := identityOf(mustGet(t, database, row.ID)); got != identityOf(row) {
		t.Fatalf("identity = %+v, want %+v kept", got, identityOf(row))
	}
}

// Identity is derived metadata, not user activity: the sidebar's "latest
// activity" ordering must be unmoved by a boot pass.
func TestRefreshIdentityLeavesTheActivityOrderAlone(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("", "aaaa1111")}}
	service, database := newIdentityService(t, fake)
	row, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fake.answers[path] = repo("github:github.com:5", "aaaa1111")
	if _, err := service.RefreshIdentity(context.Background(), nil); err != nil {
		t.Fatalf("RefreshIdentity: %v", err)
	}
	stored := mustGet(t, database, row.ID)
	if stored.RepositoryID == "" || stored.UpdatedAt != row.UpdatedAt {
		t.Fatalf("stored = %+v, want the origin written and UpdatedAt untouched at %d", stored, row.UpdatedAt)
	}
}

func TestRefreshIdentityStopsWhenItsContextEnds(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{}}
	service, _ := newIdentityService(t, fake)
	if _, err := service.Create(path); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fake.asked = nil

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.RefreshIdentity(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshIdentity = %v, want context.Canceled", err)
	}
	if len(fake.asked) != 0 {
		t.Fatalf("derivations after cancellation = %+v, want none", fake.asked)
	}
}

// A read the pass's ctx cut short fails because of the ctx, not the
// checkout, so it is not recorded as the row's identity error.
func TestRefreshIdentityDoesNotRecordAReadItsContextEnded(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string]identityAnswer{path: repo("github:github.com:3", "aaaa1111")}}
	service, database := newIdentityService(t, fake)
	row, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fake.answers[path] = identityAnswer{err: errors.New("signal: killed")}
	fake.during = cancel
	var persisted []store.Project
	if _, err := service.RefreshIdentity(ctx, func(row store.Project) { persisted = append(persisted, row) }); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshIdentity = %v, want context.Canceled", err)
	}
	if len(persisted) != 0 {
		t.Fatalf("persisted = %+v, want nothing", persisted)
	}
	if got := identityOf(mustGet(t, database, row.ID)); got != identityOf(row) {
		t.Fatalf("stored identity = %+v, want it unchanged at %+v", got, identityOf(row))
	}
}

func TestRefreshIdentityWithoutADeriverIsANoop(t *testing.T) {
	service, _ := newTestService(t)
	if _, err := service.Create(mustDir(t, t.TempDir(), "workspace")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.RefreshIdentity(context.Background(), func(store.Project) {
		t.Fatal("refresh announced a row with no identity deriver wired")
	}); err != nil {
		t.Fatalf("RefreshIdentity: %v", err)
	}
}

func TestRefreshIdentityWithoutAStoreErrors(t *testing.T) {
	service := New(Deps{})
	if _, err := service.RefreshIdentity(context.Background(), nil); err == nil {
		t.Fatal("RefreshIdentity with no store returned no error")
	}
}

func TestInspectFolderAnswersTheFolderIdentityWithoutCreatingAProject(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	checkout := mustDir(t, parent, "checkout")
	plain := mustDir(t, parent, "plain")
	broken := mustDir(t, parent, "broken")
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fake := &fakeIdentity{answers: map[string]identityAnswer{
		checkout: repo("github:github.com:3", "aaaa1111"),
		broken:   {err: errors.New("detected dubious ownership")},
	}}
	service, database := newIdentityService(t, fake)

	got, err := service.InspectFolder(context.Background(), checkout)
	if err != nil {
		t.Fatalf("InspectFolder(checkout): %v", err)
	}
	if want := (FolderIdentity{Repository: true, RepositoryID: "github:github.com:3", IdentitySource: "aaaa1111"}); got != want {
		t.Fatalf("InspectFolder(checkout) = %+v, want %+v", got, want)
	}
	if got, err := service.InspectFolder(context.Background(), plain); err != nil || got != (FolderIdentity{}) {
		t.Fatalf("InspectFolder(plain) = %+v, %v; want not a repository", got, err)
	}
	if _, err := service.InspectFolder(context.Background(), broken); err == nil || !strings.Contains(err.Error(), "dubious ownership") {
		t.Fatalf("InspectFolder(broken) error = %v, want the git failure", err)
	}
	for _, path := range []string{"", "  ", file, filepath.Join(parent, "missing")} {
		if _, err := service.InspectFolder(context.Background(), path); err == nil {
			t.Errorf("InspectFolder(%q) succeeded, want an error", path)
		}
	}
	rows, err := database.ListAllProjects()
	if err != nil {
		t.Fatalf("ListAllProjects: %v", err)
	}
	for _, row := range rows {
		if row.Path == checkout {
			t.Fatalf("InspectFolder created a project at %s", checkout)
		}
	}
	if _, err := New(Deps{}).InspectFolder(context.Background(), checkout); err == nil {
		t.Fatal("InspectFolder without a deriver succeeded, want an error")
	}
}

func TestInspectFolderCancelsItsIdentityRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := New(Deps{Identity: func(readCtx context.Context, _ string) (gitops.RepoIdentity, error) {
		cancel()
		select {
		case <-readCtx.Done():
			return gitops.RepoIdentity{}, readCtx.Err()
		default:
			t.Error("folder inspection did not pass cancellation to its identity read")
			return gitops.RepoIdentity{}, nil
		}
	}})
	if _, err := service.InspectFolder(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("inspection returned %v after cancellation", err)
	}
}

// The pass returns only the rows whose forge was unavailable, and a retry
// re-reads only those rows, skipping one deleted since.
func TestRefreshIdentityReturnsUnavailableForgesAndRetryRereadsOnlyThose(t *testing.T) {
	parent := t.TempDir()
	verified := mustDir(t, parent, "verified")
	unavailable := mustDir(t, parent, "unavailable")
	invalid := mustDir(t, parent, "invalid")
	unreadable := mustDir(t, parent, "unreadable")
	deleted := mustDir(t, parent, "deleted")
	down := identityAnswer{identity: gitops.RepoIdentity{Repository: true, IdentitySource: "aaaa", LookupError: "forge down", LookupRetryable: true}}
	fake := &fakeIdentity{answers: map[string]identityAnswer{
		verified:    repo("github:github.com:1", "bbbb"),
		unavailable: down,
		invalid:     {identity: gitops.RepoIdentity{Repository: true, IdentitySource: "cccc", LookupError: "no repository ID"}},
		unreadable:  {err: errors.New("detected dubious ownership")},
		deleted:     down,
	}}
	service, database := newIdentityService(t, fake)
	rows := map[string]store.Project{}
	for _, path := range []string{verified, unavailable, invalid, unreadable, deleted} {
		row, err := service.Create(path)
		if err != nil {
			t.Fatalf("Create %s: %v", path, err)
		}
		rows[path] = row
	}

	retry, err := service.RefreshIdentity(context.Background(), nil)
	if err != nil {
		t.Fatalf("RefreshIdentity: %v", err)
	}
	if len(retry) != 2 || !(retry[0] == rows[unavailable].ID && retry[1] == rows[deleted].ID || retry[1] == rows[unavailable].ID && retry[0] == rows[deleted].ID) {
		t.Fatalf("retry = %v, want exactly the rows whose forge was unavailable", retry)
	}

	if err := database.DeleteProject(rows[deleted].ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	fake.answers[unavailable] = repo("github:github.com:2", "aaaa")
	fake.asked = nil
	var announced []string
	retry, err = service.RetryIdentity(context.Background(), retry, func(row store.Project) { announced = append(announced, row.ID) })
	if err != nil {
		t.Fatalf("RetryIdentity: %v", err)
	}
	if len(retry) != 0 || len(fake.asked) != 1 || fake.asked[0].path != unavailable {
		t.Fatalf("retry = %v after reading %+v, want only the unavailable row read and resolved", retry, fake.asked)
	}
	if len(announced) != 1 || announced[0] != rows[unavailable].ID {
		t.Fatalf("announced %v, want the resolved row", announced)
	}
	if got := identityOf(mustGet(t, database, rows[unavailable].ID)); got != (store.ProjectIdentity{RepositoryID: "github:github.com:2", IdentitySource: "aaaa"}) {
		t.Fatalf("resolved identity = %+v", got)
	}
}
