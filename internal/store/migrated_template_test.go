package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// databaseShape is everything about a database New decides other than its
// identity and the migration rows' applied times.
type databaseShape struct {
	Schema     [][4]string
	Migrations [][2]string
	Pragmas    map[string]string
}

func readDatabaseShape(t *testing.T, s *Store) databaseShape {
	t.Helper()
	shape := databaseShape{Pragmas: map[string]string{}}
	rows, err := s.db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var entry [4]string
		if err := rows.Scan(&entry[0], &entry[1], &entry[2], &entry[3]); err != nil {
			t.Fatal(err)
		}
		shape.Schema = append(shape.Schema, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	rows, err = s.db.Query(`SELECT version, name FROM migration_versions ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var entry [2]string
		if err := rows.Scan(&entry[0], &entry[1]); err != nil {
			t.Fatal(err)
		}
		shape.Migrations = append(shape.Migrations, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for _, pragma := range []string{
		"user_version", "schema_version", "application_id", "auto_vacuum", "journal_mode",
		"page_size", "encoding", "freelist_count", "foreign_keys", "synchronous", "busy_timeout",
	} {
		var value string
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&value); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		shape.Pragmas[pragma] = value
	}
	return shape
}

func closeTemplateStore(t *testing.T, s *Store) {
	t.Helper()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
}

func TestNewFromTemplateMatchesAFreshMigration(t *testing.T) {
	fresh, err := New(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeTemplateStore(t, fresh)
	dir := t.TempDir()
	first, err := NewFromTemplate(context.Background(), filepath.Join(dir, "first.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeTemplateStore(t, first)
	second, err := NewFromTemplate(context.Background(), filepath.Join(dir, "second.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeTemplateStore(t, second)

	want := readDatabaseShape(t, fresh)
	if len(want.Schema) == 0 || len(want.Migrations) == 0 {
		t.Fatalf("fresh database read as empty: %+v", want)
	}
	if got := readDatabaseShape(t, first); !reflect.DeepEqual(got, want) {
		t.Fatalf("template copy differs from a fresh migration:\n got %+v\nwant %+v", got, want)
	}
	if pending, err := first.DeferredMigrationsPending(); err != nil || pending {
		t.Fatalf("template copy has deferred phases pending: %v %v", pending, err)
	}

	identities := map[string]bool{}
	for _, s := range []*Store{fresh, first, second} {
		id, err := s.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if id.BackendID == "" || id.ReplicaGeneration == "" || identities[id.BackendID] || identities[id.ReplicaGeneration] {
			t.Fatalf("each database needs its own identity; got %+v among %v", id, identities)
		}
		identities[id.BackendID], identities[id.ReplicaGeneration] = true, true
	}

	// Copies share no state: a write to one is not in the other.
	if _, err := first.CreateProject(Project{ID: "p1", Path: "/tmp/p1", Name: "P1", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if projects, err := second.ListProjects(); err != nil || len(projects) != 0 {
		t.Fatalf("second copy sees %+v (%v)", projects, err)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{"first.db", "first.db-wal"} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Fatalf("%s has mode %o, want 600", name, perm)
			}
		}
	}
	// The build's directory is gone.
	if entries := dirNames(t, dir); !reflect.DeepEqual(entries, []string{"first.db", "first.db-shm", "first.db-wal", "second.db", "second.db-shm", "second.db-wal"}) {
		t.Fatalf("directory holds %v", entries)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// sharedImage builds through the process template, so a test instance
// does not replay the migration chain again.
func sharedImage(ctx context.Context, dir string) ([]byte, error) {
	return processTemplate.load(ctx, dir)
}

func TestNewFromTemplateBuildsOnceUnderConcurrentFirstUse(t *testing.T) {
	var builds atomic.Int32
	release := make(chan struct{})
	tmpl := newMigratedTemplate(func(ctx context.Context, dir string) ([]byte, error) {
		builds.Add(1)
		<-release
		return sharedImage(ctx, dir)
	})
	dir := t.TempDir()
	const callers = 8
	var entered, done sync.WaitGroup
	stores := make([]*Store, callers)
	errs := make([]error, callers)
	for i := range callers {
		entered.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			entered.Done()
			stores[i], errs[i] = tmpl.open(context.Background(), filepath.Join(dir, string(rune('a'+i))+".db"))
		}()
	}
	entered.Wait()
	close(release)
	done.Wait()
	for i, s := range stores {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		closeTemplateStore(t, s)
		if _, err := s.CreateProject(Project{ID: "p", Path: "/tmp/p", Name: "P", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("caller %d store is not usable: %v", i, err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("built %d times, want 1", got)
	}
}

func TestNewFromTemplateRetriesAFailedBuild(t *testing.T) {
	var builds atomic.Int32
	buildErr := errors.New("disk full")
	tmpl := newMigratedTemplate(func(ctx context.Context, dir string) ([]byte, error) {
		if builds.Add(1) == 1 {
			if err := os.WriteFile(filepath.Join(dir, "partial.db"), []byte("partial"), 0o600); err != nil {
				return nil, err
			}
			return nil, buildErr
		}
		return sharedImage(ctx, dir)
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")
	if _, err := tmpl.open(context.Background(), path); !errors.Is(err, buildErr) {
		t.Fatalf("first open: %v, want the build error", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("failed build left %v", names)
	}
	s, err := tmpl.open(context.Background(), path)
	if err != nil {
		t.Fatalf("second open did not rebuild: %v", err)
	}
	closeTemplateStore(t, s)
	other, err := tmpl.open(context.Background(), filepath.Join(dir, "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	closeTemplateStore(t, other)
	if got := builds.Load(); got != 2 {
		t.Fatalf("built %d times, want 2 (one failure, then one kept image)", got)
	}
}

func TestNewFromTemplateCancelledBuildIsNotKept(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	tmpl := newMigratedTemplate(func(_ context.Context, dir string) ([]byte, error) {
		return buildMigratedImage(ctx, dir)
	})
	if _, err := tmpl.open(context.Background(), filepath.Join(dir, "history.db")); !errors.Is(err, context.Canceled) {
		t.Fatalf("open with a cancelled build: %v", err)
	}
	if tmpl.image != nil {
		t.Fatal("a cancelled build was kept")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("cancelled build left %v", names)
	}
}

func TestNewFromTemplateWaiterHonorsItsContext(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	tmpl := newMigratedTemplate(func(ctx context.Context, dir string) ([]byte, error) {
		close(started)
		<-release
		return sharedImage(ctx, dir)
	})
	dir := t.TempDir()
	built := make(chan error, 1)
	go func() {
		s, err := tmpl.open(context.Background(), filepath.Join(dir, "builder.db"))
		if err == nil {
			err = s.Close()
		}
		built <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waited := make(chan error, 1)
	go func() {
		s, err := tmpl.open(ctx, filepath.Join(dir, "waiter.db"))
		if err == nil {
			err = errors.Join(errors.New("opened"), s.Close())
		}
		waited <- err
	}()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter: %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter kept waiting for the build after its context ended")
	}
	close(release)
	if err := <-built; err != nil {
		t.Fatalf("builder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "waiter.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled waiter created its database: %v", err)
	}
}

func TestNewFromTemplateRefusesExistingFiles(t *testing.T) {
	for _, existing := range []string{"", "-wal", "-journal"} {
		t.Run("history.db"+existing, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.db")
			if err := os.WriteFile(path+existing, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if s, err := NewFromTemplate(context.Background(), path); err == nil {
				s.Close()
				t.Fatal("opened over an existing file")
			}
			if data, err := os.ReadFile(path + existing); err != nil || string(data) != "keep" {
				t.Fatalf("existing file changed: %q %v", data, err)
			}
			if existing != "" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("created the database beside a stale %s: %v", existing, err)
				}
			}
		})
	}
}

func TestNewFromTemplateRemovesADatabaseThatFailsToOpen(t *testing.T) {
	tmpl := newMigratedTemplate(func(context.Context, string) ([]byte, error) {
		return []byte("not a database, but long enough to be read as a header......................................................................"), nil
	})
	dir := t.TempDir()
	if s, err := tmpl.open(context.Background(), filepath.Join(dir, "history.db")); err == nil {
		s.Close()
		t.Fatal("opened a corrupt image")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("failed open left %v", names)
	}
}
