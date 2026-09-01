package projectapp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// fakeIdentity answers from a path → identity table and records every path it
// was asked about, so a test can assert the pass costs one derivation per
// unidentified row and none for the rest.
type fakeIdentity struct {
	answers map[string][2]string
	asked   []string
}

func (f *fakeIdentity) derive(path string) (string, string) {
	f.asked = append(f.asked, path)
	answer := f.answers[path]
	return answer[0], answer[1]
}

func newIdentityService(t *testing.T, identity func(string) (string, string)) (*Service, *store.Store) {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return New(Deps{
		Store:    database,
		Now:      func() time.Time { return time.UnixMilli(1234) },
		Identity: identity,
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

func TestCreateStampsRepositoryIdentity(t *testing.T) {
	parent := t.TempDir()
	path := mustDir(t, parent, "workspace")
	fake := &fakeIdentity{answers: map[string][2]string{
		path: {"git@example.com:owner/repo.git", "aaaa1111"},
	}}
	service, database := newIdentityService(t, fake.derive)

	created, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.RemoteURL != "git@example.com:owner/repo.git" || created.RootCommit != "aaaa1111" {
		t.Fatalf("created identity = (%q, %q)", created.RemoteURL, created.RootCommit)
	}
	stored, err := database.GetProject(created.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.RemoteURL != created.RemoteURL || stored.RootCommit != created.RootCommit {
		t.Fatalf("stored identity = (%q, %q), want the created row's", stored.RemoteURL, stored.RootCommit)
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
	if created.RemoteURL != "" || created.RootCommit != "" {
		t.Fatalf("identity = (%q, %q), want both empty with no deriver wired",
			created.RemoteURL, created.RootCommit)
	}
}

func TestEnsureForWorkspaceStampsIdentityOnTheCreatedRow(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string][2]string{
		path: {"https://example.com/repo.git", "bbbb2222"},
	}}
	service, database := newIdentityService(t, fake.derive)

	write, err := service.EnsureForWorkspace(path)
	if err != nil {
		t.Fatalf("EnsureForWorkspace: %v", err)
	}
	if !write.Changed {
		t.Fatal("first EnsureForWorkspace reported no creation")
	}
	if write.Project.RemoteURL != "https://example.com/repo.git" || write.Project.RootCommit != "bbbb2222" {
		t.Fatalf("returned identity = (%q, %q), want the derived one",
			write.Project.RemoteURL, write.Project.RootCommit)
	}
	stored, err := database.GetProject(write.Project.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.RemoteURL != "https://example.com/repo.git" || stored.RootCommit != "bbbb2222" {
		t.Fatalf("stored identity = (%q, %q)", stored.RemoteURL, stored.RootCommit)
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

func TestBackfillIdentityWritesAndAnnouncesOnlyTheRowsItMoved(t *testing.T) {
	parent := t.TempDir()
	identified := mustDir(t, parent, "already")
	remoteless := mustDir(t, parent, "remoteless")
	plain := mustDir(t, parent, "plain")
	archived := mustDir(t, parent, "archived")

	fake := &fakeIdentity{answers: map[string][2]string{
		identified: {"https://example.com/already.git", "cccc3333"},
		remoteless: {"", "dddd4444"},
		archived:   {"https://example.com/archived.git", "eeee5555"},
		// `plain` answers ("", "") — not a repository.
	}}
	service, database := newIdentityService(t, fake.derive)

	for _, path := range []string{identified, remoteless, plain, archived} {
		if _, err := service.Create(path); err != nil {
			t.Fatalf("Create %s: %v", path, err)
		}
	}
	archivedRow, err := database.GetProjectByPath(archived)
	if err != nil {
		t.Fatalf("GetProjectByPath: %v", err)
	}
	if _, _, err := database.ArchiveProject(archivedRow.ID); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}

	// A row created before the deriver existed: clear its identity so the
	// pass has something to fill, which is exactly the pre-v79 shape.
	remotelessRow, err := database.GetProjectByPath(remoteless)
	if err != nil {
		t.Fatalf("GetProjectByPath: %v", err)
	}
	if _, _, err := database.UpdateProjectIdentity(remotelessRow.ID, "", ""); err != nil {
		t.Fatalf("clear identity: %v", err)
	}

	fake.asked = nil
	var announced []store.Project
	if err := service.BackfillIdentity(func(row store.Project) {
		announced = append(announced, row)
	}); err != nil {
		t.Fatalf("BackfillIdentity: %v", err)
	}

	// `identified` and `archived` were stamped at creation, so the pass
	// must not have asked about them at all.
	wantAsked := map[string]bool{remoteless: true, plain: true}
	for _, path := range fake.asked {
		if !wantAsked[path] {
			t.Errorf("backfill derived %s, which already had an identity", path)
		}
	}
	if len(fake.asked) != len(wantAsked) {
		t.Errorf("backfill derived %v, want exactly the unidentified rows", fake.asked)
	}

	if len(announced) != 1 || announced[0].ID != remotelessRow.ID {
		t.Fatalf("announced %+v, want only the remoteless row", announced)
	}
	if announced[0].RemoteURL != "" || announced[0].RootCommit != "dddd4444" {
		t.Fatalf("announced identity = (%q, %q), want the root-only answer",
			announced[0].RemoteURL, announced[0].RootCommit)
	}
	stored, err := database.GetProject(remotelessRow.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.RootCommit != "dddd4444" {
		t.Fatalf("stored root commit = %q, want dddd4444", stored.RootCommit)
	}
}

// The archived project is unarchived later; leaving it out of the pass would
// make it the one entry that never merges across machines.
func TestBackfillIdentityCoversArchivedRows(t *testing.T) {
	path := mustDir(t, t.TempDir(), "archived")
	fake := &fakeIdentity{answers: map[string][2]string{
		path: {"https://example.com/archived.git", "ffff6666"},
	}}
	service, database := newIdentityService(t, fake.derive)

	row, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := database.UpdateProjectIdentity(row.ID, "", ""); err != nil {
		t.Fatalf("clear identity: %v", err)
	}
	if _, _, err := database.ArchiveProject(row.ID); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}

	if err := service.BackfillIdentity(nil); err != nil {
		t.Fatalf("BackfillIdentity: %v", err)
	}
	stored, err := database.GetProject(row.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.RemoteURL != "https://example.com/archived.git" || stored.RootCommit != "ffff6666" {
		t.Fatalf("archived row identity = (%q, %q), want it backfilled",
			stored.RemoteURL, stored.RootCommit)
	}
}

// Identity is derived metadata, not user activity: the sidebar's "latest
// activity" ordering must be unmoved by a boot pass.
func TestBackfillIdentityLeavesTheActivityOrderAlone(t *testing.T) {
	path := mustDir(t, t.TempDir(), "workspace")
	fake := &fakeIdentity{answers: map[string][2]string{
		path: {"https://example.com/repo.git", "aaaa1111"},
	}}
	service, database := newIdentityService(t, fake.derive)

	row, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := database.UpdateProjectIdentity(row.ID, "", ""); err != nil {
		t.Fatalf("clear identity: %v", err)
	}

	if err := service.BackfillIdentity(nil); err != nil {
		t.Fatalf("BackfillIdentity: %v", err)
	}
	stored, err := database.GetProject(row.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.UpdatedAt != row.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want it untouched at %d", stored.UpdatedAt, row.UpdatedAt)
	}
}

func TestBackfillIdentityWithoutADeriverIsANoop(t *testing.T) {
	service, database := newTestService(t)
	path := mustDir(t, t.TempDir(), "workspace")
	row, err := service.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := service.BackfillIdentity(func(store.Project) {
		t.Fatal("backfill announced a row with no identity deriver wired")
	}); err != nil {
		t.Fatalf("BackfillIdentity: %v", err)
	}
	stored, err := database.GetProject(row.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.RemoteURL != "" || stored.RootCommit != "" {
		t.Fatalf("identity = (%q, %q), want both empty", stored.RemoteURL, stored.RootCommit)
	}
}

func TestBackfillIdentityWithoutAStoreErrors(t *testing.T) {
	service := New(Deps{})
	if err := service.BackfillIdentity(nil); err == nil {
		t.Fatal("BackfillIdentity with no store returned no error")
	}
}
