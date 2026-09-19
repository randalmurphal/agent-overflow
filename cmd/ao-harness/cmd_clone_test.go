package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// cloneSourceOptions shapes the synthetic source tree. No test here ever
// touches real app data: the source is built in a temp dir with a real
// SQLite file carrying the real column names the scrub names.
type cloneSourceOptions struct {
	// skipImportState builds a database with no thread_import_state table,
	// which is what a store predating migration v50 looks like.
	skipImportState bool
	attachments     map[string]string
	// leaveOpen keeps a writer connection on the source for the test's
	// lifetime, which is the state that actually matters: the real app is
	// RUNNING while a clone is taken.
	leaveOpen bool
	// skipWorkItems builds a database with no workflow tables, which is
	// what a store predating them looks like.
	skipWorkItems bool
}

// The real paths the fixture carries. Every one of them must be gone from
// the copy: these are the developer's checkouts a booted clone would
// otherwise spawn a mock provider inside.
const (
	realProjectApp   = "/home/real/repos/app"
	realProjectWork  = "/home/real/work/app"
	realProjectTools = "/home/real/repos/tools"
	realGhost        = "/home/real/repos/ghost"
	realWorktreeX    = "/home/real/.config/agent-overflow/worktrees/app/feature-x"
	realWorktreeY    = "/home/real/.config/agent-overflow/worktrees/app/ao-workflow-y"
)

// realClonePaths is every path spelled above, for the assertion that no
// column in the copy still names one.
var realClonePaths = []string{
	realProjectApp, realProjectWork, realProjectTools, realGhost,
	realWorktreeX, realWorktreeY,
}

// newCloneSource builds <root>/agent-overflow/agent-overflow.db plus the
// attachments beside it, and returns the DATA DIR.
func newCloneSource(t *testing.T, opts cloneSourceOptions) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), appDataDirName)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, storeFileName)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if opts.leaveOpen {
		t.Cleanup(func() { db.Close() })
	} else {
		defer db.Close()
	}

	// The columns are the real ones, spelled as the schema spells them:
	// the whole point of the scrub is that it names columns correctly, and
	// a fixture with invented names would assert nothing.
	statements := []string{
		// projects.slug carries the real UNIQUE index: the relocation reads
		// slug first precisely because the store already keeps it unique.
		// The two slugs below are distinct there and reduce to the same
		// filesystem-safe name, which is the collision the dedupe answers.
		`CREATE TABLE projects (
		    id            TEXT    PRIMARY KEY,
		    path          TEXT    NOT NULL UNIQUE,
		    name          TEXT    NOT NULL,
		    color         TEXT    NOT NULL DEFAULT '',
		    sort_position INTEGER NOT NULL DEFAULT 0,
		    created_at    INTEGER NOT NULL,
		    updated_at    INTEGER NOT NULL,
		    archived      INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1)),
		    slug          TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX idx_projects_slug ON projects(slug)`,
		`INSERT INTO projects (id, path, name, created_at, updated_at, slug) VALUES
		    ('p1','` + realProjectApp + `','My App',1,1,'my app'),
		    ('p2','` + realProjectWork + `','My App',1,1,'my/app'),
		    ('p3','` + realProjectTools + `','',1,1,'')`,
		`CREATE TABLE threads (
		    id                       TEXT PRIMARY KEY,
		    project_id               TEXT REFERENCES projects(id) ON DELETE CASCADE,
		    title                    TEXT NOT NULL DEFAULT '',
		    provider                 TEXT NOT NULL,
		    workspace_path           TEXT NOT NULL,
		    worktree_path            TEXT,
		    branch                   TEXT,
		    session_ref              TEXT,
		    pending_fork_session_ref TEXT,
		    pending_fork_resume_at   TEXT NOT NULL DEFAULT ''
		)`,
		// t2 and t3 share one REAL worktree, so they must share one fixture
		// worktree. t5 has no project row at all: threads.project_id is
		// nullable, so an orphan is a shape the relocation has to answer.
		`INSERT INTO threads VALUES
		    ('t1','p1','one','claude','` + realProjectApp + `',NULL,NULL,'claude-session-aaaa','pending-bbbb','leaf-cccc'),
		    ('t2','p1','two','codex','` + realWorktreeX + `','` + realWorktreeX + `','feature-x','codex-thread-dddd',NULL,''),
		    ('t3','p1','three','claude','` + realWorktreeX + `','` + realWorktreeX + `','feature-x','claude-session-eeee',NULL,''),
		    ('t4','p2','four','codex','` + realProjectWork + `','',NULL,NULL,NULL,''),
		    ('t5',NULL,'five','codex','` + realGhost + `',NULL,NULL,NULL,NULL,'')`,
		`CREATE TABLE ui_state (
		    scope      TEXT NOT NULL,
		    key        TEXT NOT NULL,
		    value      TEXT NOT NULL,
		    updated_at INTEGER NOT NULL,
		    PRIMARY KEY (scope, key)
		)`,
		`INSERT INTO ui_state VALUES ('client:abc','panes','{"open":["t1"]}',1), ('client:abc','sidebar','w',1)`,
	}
	if !opts.skipWorkItems {
		statements = append(statements,
			`CREATE TABLE work_items (
			    id            TEXT PRIMARY KEY,
			    project_id    TEXT NOT NULL,
			    worktree_path TEXT NOT NULL DEFAULT ''
			)`,
			// w1 names a worktree the thread pass rebuilds; w2 names one no
			// thread does, which has no fixture to point at.
			`INSERT INTO work_items VALUES
			    ('w1','p1','`+realWorktreeX+`'),
			    ('w2','p1','`+realWorktreeY+`'),
			    ('w3','p2','')`,
			`CREATE TABLE work_item_units (
			    item_id       TEXT NOT NULL,
			    unit_id       TEXT NOT NULL,
			    worktree_path TEXT NOT NULL DEFAULT '',
			    PRIMARY KEY (item_id, unit_id)
			)`,
			`INSERT INTO work_item_units VALUES
			    ('w1','u1','`+realWorktreeX+`'),
			    ('w2','u2','`+realWorktreeY+`')`,
		)
	}
	if !opts.skipImportState {
		statements = append(statements,
			`CREATE TABLE thread_import_state (
			    thread_id                TEXT PRIMARY KEY,
			    provider                 TEXT NOT NULL,
			    source_path              TEXT NOT NULL,
			    source_session_id        TEXT NOT NULL,
			    leaf_uuid                TEXT NOT NULL DEFAULT '',
			    source_parent_session_id TEXT NOT NULL DEFAULT '',
			    imported_at              INTEGER NOT NULL
			)`,
			// TWO rows on the SAME provider: the shape that fired migration
			// v63's uniqueness trigger when the scrub blanked both to ''
			// (found live 2026-08-26 against a real store, 1811 rows).
			`INSERT INTO thread_import_state VALUES
			    ('t1','claude','/home/real/.claude/x.jsonl','sess-1111','leaf-2222','parent-3333',1),
			    ('t3','claude','/home/real/.claude/y.jsonl','sess-4444','leaf-5555','',1)`,
			// The v63 uniqueness triggers, verbatim from
			// internal/store/migrate.go (migration 63). A fixture without
			// them passed a scrub the real store aborts.
			`CREATE TRIGGER thread_import_state_unique_source_insert
			BEFORE INSERT ON thread_import_state
			WHEN NOT EXISTS (
			    SELECT 1 FROM thread_import_state AS own
			     WHERE own.thread_id = NEW.thread_id
			       AND own.provider = NEW.provider
			       AND own.source_session_id = NEW.source_session_id
			)
			 AND (EXISTS (
			    SELECT 1 FROM thread_import_state AS existing
			     WHERE existing.provider = NEW.provider
			       AND existing.source_session_id = NEW.source_session_id
			       AND existing.thread_id <> NEW.thread_id
			) OR EXISTS (
			    SELECT 1 FROM threads AS existing
			     WHERE existing.id <> NEW.thread_id
			       AND CASE existing.provider
			             WHEN 'claude-tui' THEN 'claude'
			             ELSE existing.provider
			           END = NEW.provider
			       AND (existing.session_ref = NEW.source_session_id
			            OR existing.pending_fork_session_ref = NEW.source_session_id)
			))
			BEGIN
			    SELECT RAISE(ABORT, 'provider session is already claimed by another thread');
			END`,
			`CREATE TRIGGER thread_import_state_unique_source_update
			BEFORE UPDATE OF provider, source_session_id ON thread_import_state
			WHEN (OLD.provider <> NEW.provider OR OLD.source_session_id <> NEW.source_session_id)
			 AND (EXISTS (
			    SELECT 1 FROM thread_import_state AS existing
			     WHERE existing.provider = NEW.provider
			       AND existing.source_session_id = NEW.source_session_id
			       AND existing.thread_id <> NEW.thread_id
			) OR EXISTS (
			    SELECT 1 FROM threads AS existing
			     WHERE existing.id <> NEW.thread_id
			       AND CASE existing.provider
			             WHEN 'claude-tui' THEN 'claude'
			             ELSE existing.provider
			           END = NEW.provider
			       AND (existing.session_ref = NEW.source_session_id
			            OR existing.pending_fork_session_ref = NEW.source_session_id)
			))
			BEGIN
			    SELECT RAISE(ABORT, 'provider session is already claimed by another thread');
			END`,
		)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed source: %v", err)
		}
	}

	for name, content := range opts.attachments {
		full := filepath.Join(dataDir, attachmentsDirName, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir
}

// runCloneInto drives the verb the way an operator does, with the OS
// config lookup pointed at a temp tree so refuseUnsafeDataRoot has
// something real to compare against.
func runCloneInto(t *testing.T, sourceDir, targetRoot string, extra ...string) (*env, string) {
	t.Helper()
	e, _, _ := testEnv(t.TempDir())
	args := append([]string{"--from", sourceDir, "--data-dir", targetRoot}, extra...)
	if err := runClone(e, args); err != nil {
		t.Fatalf("clone: %v", err)
	}
	return e, filepath.Join(targetRoot, appDataDirName, storeFileName)
}

func openClone(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The triple store.UpdateSessionRef always writes together. A clone that
// left any one of them behind would hand a harness thread a handle onto a
// real provider session.
func TestCloneClearsEverySessionRefColumn(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{leaveOpen: true})
	_, targetDB := runCloneInto(t, source, filepath.Join(t.TempDir(), "root"))

	db := openClone(t, targetDB)
	var stragglers int
	if err := db.QueryRow(
		`SELECT count(*) FROM threads
		  WHERE session_ref IS NOT NULL
		     OR pending_fork_session_ref IS NOT NULL
		     OR pending_fork_resume_at <> ''`,
	).Scan(&stragglers); err != nil {
		t.Fatal(err)
	}
	if stragglers != 0 {
		t.Fatalf("%d thread row(s) kept a session handle", stragglers)
	}
	// The rows themselves must survive: the threads ARE the repro.
	var threads int
	if err := db.QueryRow(`SELECT count(*) FROM threads`).Scan(&threads); err != nil {
		t.Fatal(err)
	}
	if threads != 5 {
		t.Fatalf("threads = %d, want 5 (the clone dropped rows)", threads)
	}
}

// Import state is NEUTRALIZED, not deleted: the row carries the cursor an
// imported thread is read through, and dropping it would change the
// thread's shape rather than only its provider identity.
func TestCloneNeutralizesImportIdentityWithoutDroppingTheRow(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	_, targetDB := runCloneInto(t, source, filepath.Join(t.TempDir(), "root"))

	db := openClone(t, targetDB)
	// BOTH same-provider rows: the second is the one migration v63's
	// uniqueness trigger used to abort on when it reached ''.
	for _, threadID := range []string{"t1", "t3"} {
		var sessionID, leaf, parent, path string
		if err := db.QueryRow(
			`SELECT source_session_id, leaf_uuid, source_parent_session_id, source_path
			   FROM thread_import_state WHERE thread_id = ?`, threadID,
		).Scan(&sessionID, &leaf, &parent, &path); err != nil {
			t.Fatalf("%s: %v", threadID, err)
		}
		if sessionID != "" || leaf != "" || parent != "" {
			t.Fatalf("%s import identity survived: session=%q leaf=%q parent=%q", threadID, sessionID, leaf, parent)
		}
		if path == "" {
			t.Fatalf("%s import row was rewritten past its identity columns", threadID)
		}
	}
}

// The scrub drops each table's triggers to run and restores them from
// sqlite_master afterwards. Both halves are asserted: the triggers exist
// in the copy, and the restored copy still ENFORCES — a restore that
// produced an inert row would weaken the clone's schema in silence.
func TestCloneRestoresTheTriggersItDroppedForTheScrub(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	_, targetDB := runCloneInto(t, source, filepath.Join(t.TempDir(), "root"))

	db := openClone(t, targetDB)
	var triggers int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'trigger'
		  AND name IN ('thread_import_state_unique_source_insert',
		               'thread_import_state_unique_source_update')`,
	).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if triggers != 2 {
		t.Fatalf("copy holds %d of the 2 uniqueness triggers after the scrub", triggers)
	}

	rw, err := sql.Open("sqlite", "file:"+targetDB+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	// Claim an id from one row, then claim the same id from the other:
	// the second write is exactly what the update trigger exists to abort.
	if _, err := rw.Exec(`UPDATE thread_import_state SET source_session_id = 'claimed' WHERE thread_id = 't1'`); err != nil {
		t.Fatalf("first re-claim should pass: %v", err)
	}
	_, err = rw.Exec(`UPDATE thread_import_state SET source_session_id = 'claimed' WHERE thread_id = 't3'`)
	if err == nil || !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("restored trigger did not enforce: err = %v", err)
	}
}

// Stale client-scoped restore state names panes and threads that answer
// sql.ErrNoRows in a different instance — the toast leak HarnessReset
// fixed.
func TestCloneWipesUIState(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	_, targetDB := runCloneInto(t, source, filepath.Join(t.TempDir(), "root"))

	db := openClone(t, targetDB)
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM ui_state`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("ui_state kept %d row(s)", rows)
	}
}

// A store predating a scrubbed table is not a failed clone. It is said
// out loud rather than skipped silently, because "the scrub ran" is the
// claim this verb makes.
func TestCloneReportsAnAbsentScrubTableInsteadOfFailing(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{skipImportState: true})
	e, _ := runCloneInto(t, source, filepath.Join(t.TempDir(), "root"))

	stderr := e.stderr.(interface{ String() string }).String()
	if !strings.Contains(stderr, "thread_import_state") {
		t.Fatalf("an absent table was skipped in silence; stderr = %q", stderr)
	}
}

// The bytes on disk are half the attachments table. Without them every
// item referencing one renders broken, which reads as a rendering bug in
// the repro the clone exists to serve.
func TestCloneCopiesAttachmentBytes(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{attachments: map[string]string{
		"a.png":            "PNG-A",
		"nested/b.txt":     "TEXT-B",
		"nested/deep/c.md": "MD-C",
	}})
	targetRoot := filepath.Join(t.TempDir(), "root")
	runCloneInto(t, source, targetRoot)

	for name, want := range map[string]string{
		"a.png": "PNG-A", "nested/b.txt": "TEXT-B", "nested/deep/c.md": "MD-C",
	} {
		got, err := os.ReadFile(filepath.Join(targetRoot, appDataDirName, attachmentsDirName, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("attachment %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("attachment %s = %q, want %q", name, got, want)
		}
	}
}

func TestCloneForceReconcilesAttachmentsWithoutDeletingUnrelatedContent(t *testing.T) {
	configRootFixture(t)
	sourceBefore := newCloneSource(t, cloneSourceOptions{attachments: map[string]string{
		"removed/old.txt": "old",
		"kept.txt":        "before",
	}})
	targetRoot := filepath.Join(t.TempDir(), "root")
	runCloneInto(t, sourceBefore, targetRoot)

	// Both files are outside the attachment tree. A force clone must not turn
	// its narrow reconciliation into a reset of the target root.
	outsideRoot := filepath.Join(targetRoot, "operator-note.txt")
	outsideData := filepath.Join(targetRoot, appDataDirName, "operator-note.txt")
	for _, path := range []string{outsideRoot, outsideData} {
		if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sourceAfter := newCloneSource(t, cloneSourceOptions{attachments: map[string]string{
		"kept.txt":     "after",
		"added/new.md": "new",
	}})
	runCloneInto(t, sourceAfter, targetRoot, "--force")

	attachments := filepath.Join(targetRoot, appDataDirName, attachmentsDirName)
	if _, err := os.Stat(filepath.Join(attachments, "removed", "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed source attachment still exists: %v", err)
	}
	for name, want := range map[string]string{
		"kept.txt":     "after",
		"added/new.md": "new",
	} {
		got, err := os.ReadFile(filepath.Join(attachments, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("attachment %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("attachment %s = %q, want %q", name, got, want)
		}
	}
	for _, path := range []string{outsideRoot, outsideData} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("unrelated target content %s: %v", path, err)
		}
		if string(got) != "keep me" {
			t.Errorf("unrelated target content %s changed to %q", path, got)
		}
	}
}

func TestCloneForceRefusesASymlinkedAttachmentTree(t *testing.T) {
	configRootFixture(t)
	sourceBefore := newCloneSource(t, cloneSourceOptions{attachments: map[string]string{"kept.txt": "before"}})
	targetRoot := filepath.Join(t.TempDir(), "root")
	runCloneInto(t, sourceBefore, targetRoot)

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "kept.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachments := filepath.Join(targetRoot, appDataDirName, attachmentsDirName)
	if err := os.Remove(filepath.Join(attachments, "kept.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(attachments); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, attachments); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	sourceAfter := newCloneSource(t, cloneSourceOptions{attachments: map[string]string{"new.txt": "new"}})
	e, _, _ := testEnv(t.TempDir())
	err := runClone(e, []string{"--from", sourceAfter, "--data-dir", targetRoot, "--force"})
	if err == nil || !strings.Contains(err.Error(), "attachments target") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("force clone accepted a symlinked attachment tree: %v", err)
	}
	got, readErr := os.ReadFile(outsideFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "outside" {
		t.Fatalf("force clone modified content behind the target symlink: %q", got)
	}
}

// Everything a boot re-derives, or that names the REAL provider home,
// stays behind. provider-accounts.json is the one whose absence is
// load-bearing: its providerHome stamp is the real home, and the boot
// prune refuses a metadata store whose stamp disagrees with the
// credential home.
func TestCloneCopiesNoFileBesidesTheDatabaseAndAttachments(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{attachments: map[string]string{"a.png": "x"}})
	for _, name := range []string{
		"settings.json", "provider-accounts.json", "account-audit.log",
		"usage-backoff.json", "harness-instance.json",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"replay", "ui-trace", "design-workdirs", "logs"} {
		if err := os.MkdirAll(filepath.Join(source, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, dir, "leftover"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	targetRoot := filepath.Join(t.TempDir(), "root")
	runCloneInto(t, source, targetRoot)

	entries, err := os.ReadDir(filepath.Join(targetRoot, appDataDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == attachmentsDirName || strings.HasPrefix(name, storeFileName) {
			continue // the database and its own -wal / -shm sidecars
		}
		if name == cloneWorktreesDirName {
			continue // generated worktree fixtures, not copied source content
		}
		t.Errorf("clone carried %s into the harness root", name)
	}
}

// The source is the developer's real data. A clone reads it and writes
// nothing back — SQLite's own WAL index sidecars aside, which any
// read-only reader of a WAL database creates and which carry no database
// content.
func TestCloneLeavesTheSourceDatabaseByteIdentical(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{attachments: map[string]string{"a.png": "x"}})
	sourceDB := filepath.Join(source, storeFileName)
	before := hashFile(t, sourceDB)

	runCloneInto(t, source, filepath.Join(t.TempDir(), "root"))

	if after := hashFile(t, sourceDB); after != before {
		t.Fatalf("the clone modified its source database (%s -> %s)", before, after)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case name == storeFileName, name == attachmentsDirName:
		case name == storeFileName+"-wal", name == storeFileName+"-shm":
			// SQLite's WAL index. Documented in snapshotDatabase: a
			// read-only reader creates it when it is absent, and a live app
			// already has both.
		default:
			t.Errorf("the clone created %s inside the source data dir", name)
		}
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// The target must pass the refusals `up` applies, because the whole point
// is that `up` boots on it next.
func TestCloneRefusesTheRealAppDataDirAsATarget(t *testing.T) {
	configRoot, appData := configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})

	for _, root := range []string{configRoot, appData} {
		e, _, _ := testEnv(t.TempDir())
		err := runClone(e, []string{"--from", source, "--data-dir", root})
		if err == nil {
			t.Fatalf("clone accepted --data-dir %s", root)
		}
		if !strings.Contains(err.Error(), "where the real app data lives") {
			t.Errorf("refusal for %s does not say why: %v", root, err)
		}
	}
}

// Two backends on one SQLite file is what `up` refuses; a clone that
// overwrote a running instance's database would be worse.
func TestCloneRefusesATargetALiveInstanceHolds(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	targetRoot := t.TempDir()
	writeInstanceFile(t, targetRoot, os.Getpid())

	e, _, _ := testEnv(t.TempDir())
	err := runClone(e, []string{"--from", source, "--data-dir", targetRoot})
	if err == nil {
		t.Fatal("clone wrote onto a live instance's data root")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("error = %v", err)
	}
}

func TestCloneRefusesWritingIntoItsOwnSource(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	sourceRoot := filepath.Dir(source)

	cases := map[string]string{
		"the source root":       sourceRoot,
		"a directory inside it": filepath.Join(source, "nested"),
	}
	for label, targetRoot := range cases {
		e, _, _ := testEnv(t.TempDir())
		err := runClone(e, []string{"--from", source, "--data-dir", targetRoot})
		if err == nil {
			t.Fatalf("clone accepted %s (%s) as a target", label, targetRoot)
		}
		if !strings.Contains(err.Error(), "source") {
			t.Errorf("refusal for %s does not name the source: %v", label, err)
		}
	}
}

// VACUUM INTO refuses an existing output file, so without this the
// operator's second clone fails with SQLite's message rather than a
// choice.
func TestCloneRefusesAnExistingTargetDatabaseUntilForced(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	targetRoot := filepath.Join(t.TempDir(), "root")
	runCloneInto(t, source, targetRoot)

	e, _, _ := testEnv(t.TempDir())
	err := runClone(e, []string{"--from", source, "--data-dir", targetRoot})
	if err == nil {
		t.Fatal("a second clone silently replaced the first")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("the refusal does not name the recourse: %v", err)
	}

	if _, targetDB := runCloneInto(t, source, targetRoot, "--force"); targetDB == "" {
		t.Fatal("--force did not re-clone")
	}
}

func TestCloneRefusesAFromPathWithNoDatabase(t *testing.T) {
	configRootFixture(t)
	e, _, _ := testEnv(t.TempDir())
	err := runClone(e, []string{"--from", t.TempDir(), "--data-dir", filepath.Join(t.TempDir(), "root")})
	if err == nil {
		t.Fatal("clone accepted a --from with no database")
	}
	if !strings.Contains(err.Error(), storeFileName) {
		t.Fatalf("error = %v", err)
	}
}

// --from takes either spelling because both are things an operator has a
// name for: the app data dir, and the root holding it.
func TestCloneAcceptsEitherSpellingOfTheSourcePath(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})

	for _, spelling := range []string{source, filepath.Dir(source)} {
		resolved, err := resolveSourceDataDir(spelling)
		if err != nil {
			t.Fatalf("resolveSourceDataDir(%s): %v", spelling, err)
		}
		if resolved != source {
			t.Errorf("resolveSourceDataDir(%s) = %s, want %s", spelling, resolved, source)
		}
	}
}

// clonePathValue is one workspace path the copy still holds, labelled by
// the row it came from so a failure names the row rather than a value.
type clonePathValue struct {
	label string
	value string
}

func cloneWorkspacePaths(t *testing.T, db *sql.DB) []clonePathValue {
	t.Helper()
	queries := []struct {
		label string
		sql   string
	}{
		{"projects.path", `SELECT id, path FROM projects`},
		{"threads.workspace_path", `SELECT id, workspace_path FROM threads`},
		{"threads.worktree_path", `SELECT id, COALESCE(worktree_path,'') FROM threads`},
		{"work_items.worktree_path", `SELECT id, worktree_path FROM work_items`},
		{"work_item_units.worktree_path", `SELECT item_id || '/' || unit_id, worktree_path FROM work_item_units`},
	}
	var out []clonePathValue
	for _, query := range queries {
		rows, err := db.Query(query.sql)
		if err != nil {
			t.Fatalf("%s: %v", query.label, err)
		}
		for rows.Next() {
			var id, value string
			if err := rows.Scan(&id, &value); err != nil {
				rows.Close()
				t.Fatalf("%s: %v", query.label, err)
			}
			out = append(out, clonePathValue{label: query.label + " " + id, value: value})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatalf("%s: %v", query.label, err)
		}
	}
	return out
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return out
}

func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

// The bug this relocation exists for: a booted clone spawned the mock
// provider with cwd inside a REAL repository, and a scenario writeFile step
// wrote into the developer's checkout. Every workspace path in the copy has
// to name something inside the clone's own root.
func TestCloneRelocatesEveryWorkspacePathIntoTheTargetRoot(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	targetRoot := filepath.Join(t.TempDir(), "root")
	_, targetDB := runCloneInto(t, source, targetRoot)
	targetRoot = resolved(t, targetRoot)

	db := openClone(t, targetDB)
	for _, row := range cloneWorkspacePaths(t, db) {
		if row.value == "" {
			continue
		}
		for _, real := range realClonePaths {
			if strings.Contains(row.value, real) {
				t.Errorf("%s still names the real path %s: %s", row.label, real, row.value)
			}
		}
		if !underDir(row.value, targetRoot) {
			t.Errorf("%s = %s, which is outside the clone root %s", row.label, row.value, targetRoot)
		}
	}
}

// A relocated workspace has to be a real repository, or git status, diffs,
// checkpoints and the branch picker all fail in the repro the clone exists
// to serve.
func TestCloneBuildsGitFixturesForEveryRelocatedWorkspace(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	targetRoot := filepath.Join(t.TempDir(), "root")
	_, targetDB := runCloneInto(t, source, targetRoot)

	db := openClone(t, targetDB)
	rows, err := db.Query(`SELECT id, path FROM projects ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	projectPaths := make(map[string]string)
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			t.Fatal(err)
		}
		projectPaths[id] = path
		if head := gitIn(t, path, "rev-parse", "HEAD"); head == "" {
			t.Errorf("project %s fixture %s has no HEAD commit", id, path)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// t1 has no worktree, so it runs in its project's own fixture.
	if got := queryString(t, db, `SELECT workspace_path FROM threads WHERE id = 't1'`); got != projectPaths["p1"] {
		t.Errorf("t1 workspace = %s, want the p1 fixture %s", got, projectPaths["p1"])
	}
	// t4's worktree_path was '' rather than NULL; it still belongs to p2.
	if got := queryString(t, db, `SELECT workspace_path FROM threads WHERE id = 't4'`); got != projectPaths["p2"] {
		t.Errorf("t4 workspace = %s, want the p2 fixture %s", got, projectPaths["p2"])
	}

	// t2 and t3 named ONE real worktree, so they get one fixture worktree,
	// and it must be a linked worktree of their project's repository.
	worktree2 := queryString(t, db, `SELECT worktree_path FROM threads WHERE id = 't2'`)
	worktree3 := queryString(t, db, `SELECT worktree_path FROM threads WHERE id = 't3'`)
	if worktree2 != worktree3 {
		t.Fatalf("threads sharing one real worktree got two fixtures: %s and %s", worktree2, worktree3)
	}
	if got := queryString(t, db, `SELECT workspace_path FROM threads WHERE id = 't2'`); got != worktree2 {
		t.Errorf("t2 workspace = %s, want its worktree %s", got, worktree2)
	}
	common := resolved(t, gitIn(t, worktree2, "rev-parse", "--git-common-dir"))
	if want := resolved(t, filepath.Join(projectPaths["p1"], ".git")); common != want {
		t.Errorf("worktree %s resolves to %s, want the p1 fixture repo %s", worktree2, common, want)
	}
	if head := gitIn(t, worktree2, "rev-parse", "HEAD"); head == "" {
		t.Errorf("worktree %s has no HEAD commit", worktree2)
	}
	// Production places worktrees under <dataDir>/worktrees/<project dir>.
	worktreesDir := filepath.Join(targetRoot, appDataDirName, cloneWorktreesDirName)
	if !underDir(worktree2, worktreesDir) {
		t.Errorf("worktree %s is not under %s", worktree2, worktreesDir)
	}

	// t5 has no project row. It still gets a repository of its own rather
	// than keeping the real path it named.
	orphan := queryString(t, db, `SELECT workspace_path FROM threads WHERE id = 't5'`)
	if head := gitIn(t, orphan, "rev-parse", "HEAD"); head == "" {
		t.Errorf("orphan thread fixture %s has no HEAD commit", orphan)
	}
	for _, path := range projectPaths {
		if orphan == path {
			t.Errorf("orphan thread was pointed at a project fixture %s", orphan)
		}
	}
}

// Two project slugs that reduce to the same filesystem-safe name must not
// land in one directory: the second would inherit the first's repository.
func TestCloneGivesCollidingProjectNamesDistinctFixtures(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	targetRoot := filepath.Join(t.TempDir(), "root")
	_, targetDB := runCloneInto(t, source, targetRoot)

	db := openClone(t, targetDB)
	first := queryString(t, db, `SELECT path FROM projects WHERE id = 'p1'`)
	second := queryString(t, db, `SELECT path FROM projects WHERE id = 'p2'`)
	if first == second {
		t.Fatalf("p1 and p2 share the fixture %s", first)
	}
	// The slug is preferred, and the empty-slug project falls back to the
	// last component of its real path.
	if filepath.Base(first) != "my-app" {
		t.Errorf("p1 fixture = %s, want a my-app directory", first)
	}
	if base := filepath.Base(second); base != "my-app-2" {
		t.Errorf("p2 fixture = %s, want the deduped my-app-2", second)
	}
	third := queryString(t, db, `SELECT path FROM projects WHERE id = 'p3'`)
	if filepath.Base(third) != "tools" {
		t.Errorf("p3 fixture = %s, want the path-derived tools directory", third)
	}
}

// Workflow rows carry worktree paths of their own. One that a thread also
// named maps onto that thread's fixture; one no thread named has no fixture
// to point at and is cleared, which falls the item back to its project.
func TestCloneMapsWorkflowWorktreesAndClearsTheRest(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	targetRoot := filepath.Join(t.TempDir(), "root")
	e, targetDB := runCloneInto(t, source, targetRoot)

	db := openClone(t, targetDB)
	threadWorktree := queryString(t, db, `SELECT worktree_path FROM threads WHERE id = 't2'`)
	if got := queryString(t, db, `SELECT worktree_path FROM work_items WHERE id = 'w1'`); got != threadWorktree {
		t.Errorf("work_items.w1 = %s, want the shared fixture worktree %s", got, threadWorktree)
	}
	if got := queryString(t, db, `SELECT worktree_path FROM work_item_units WHERE unit_id = 'u1'`); got != threadWorktree {
		t.Errorf("work_item_units.u1 = %s, want the shared fixture worktree %s", got, threadWorktree)
	}
	for _, query := range []string{
		`SELECT worktree_path FROM work_items WHERE id = 'w2'`,
		`SELECT worktree_path FROM work_item_units WHERE unit_id = 'u2'`,
	} {
		if got := queryString(t, db, query); got != "" {
			t.Errorf("%s = %s, want it cleared", query, got)
		}
	}
	stdout := e.stdout.(interface{ String() string }).String()
	for _, want := range []string{"workspaces", "worktrees", "work_items worktrees"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the receipt never mentions %q:\n%s", want, stdout)
		}
	}
}

// A store predating the workflow tables is not a failed clone, and the
// skip is said out loud the way the scrub says it.
func TestCloneReportsAbsentWorkflowTablesInsteadOfFailing(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{skipWorkItems: true})
	e, _ := runCloneInto(t, source, filepath.Join(t.TempDir(), "root"))

	stderr := e.stderr.(interface{ String() string }).String()
	for _, table := range []string{"work_items", "work_item_units"} {
		if !strings.Contains(stderr, table) {
			t.Errorf("an absent %s table was skipped in silence; stderr = %q", table, stderr)
		}
	}
}

// A re-clone rebuilds the fixtures from the new database. The previous
// clone's repos and worktrees are in the way, and git refuses to attach a
// worktree onto occupied state, so --force clears both generated trees.
func TestCloneForceRebuildsWorkspaceFixtures(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	targetRoot := filepath.Join(t.TempDir(), "root")
	runCloneInto(t, source, targetRoot)

	stale := filepath.Join(targetRoot, cloneWorkspacesDirName, "stale-fixture")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	_, targetDB := runCloneInto(t, source, targetRoot, "--force")

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the previous clone's workspace tree survived --force: %v", err)
	}
	db := openClone(t, targetDB)
	for _, row := range cloneWorkspacePaths(t, db) {
		if row.value == "" {
			continue
		}
		if !underDir(row.value, resolved(t, targetRoot)) {
			t.Errorf("%s = %s, which is outside the clone root", row.label, row.value)
		}
	}
	worktree := queryString(t, db, `SELECT worktree_path FROM threads WHERE id = 't2'`)
	project := queryString(t, db, `SELECT path FROM projects WHERE id = 'p1'`)
	common := resolved(t, gitIn(t, worktree, "rev-parse", "--git-common-dir"))
	if want := resolved(t, filepath.Join(project, ".git")); common != want {
		t.Errorf("re-cloned worktree %s resolves to %s, want %s", worktree, common, want)
	}
	// git's own bookkeeping: a rebuilt repo must not still list the
	// previous clone's worktrees.
	if out := gitIn(t, project, "worktree", "list"); strings.Count(out, "\n") != 1 {
		t.Errorf("p1 fixture lists unexpected worktrees:\n%s", out)
	}
}

// The relocation names columns in the real schema. A store missing one is
// not a store this code can relocate, and a partial relocation would leave
// real paths behind.
func TestCloneRefusesAStoreMissingAWorkspaceColumn(t *testing.T) {
	configRootFixture(t)
	source := newCloneSource(t, cloneSourceOptions{})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(source, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE threads DROP COLUMN worktree_path`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	e, _, _ := testEnv(t.TempDir())
	err = runClone(e, []string{"--from", source, "--data-dir", filepath.Join(t.TempDir(), "root")})
	if err == nil {
		t.Fatal("clone relocated workspaces in a store it does not understand")
	}
	if !strings.Contains(err.Error(), "worktree_path") {
		t.Fatalf("the error does not name the missing column: %v", err)
	}
}
