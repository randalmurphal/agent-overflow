package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
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
		`CREATE TABLE threads (
		    id                       TEXT PRIMARY KEY,
		    title                    TEXT NOT NULL DEFAULT '',
		    provider                 TEXT NOT NULL,
		    session_ref              TEXT,
		    pending_fork_session_ref TEXT,
		    pending_fork_resume_at   TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO threads VALUES
		    ('t1','one','claude','claude-session-aaaa','pending-bbbb','leaf-cccc'),
		    ('t2','two','codex','codex-thread-dddd',NULL,'')`,
		`CREATE TABLE ui_state (
		    scope      TEXT NOT NULL,
		    key        TEXT NOT NULL,
		    value      TEXT NOT NULL,
		    updated_at INTEGER NOT NULL,
		    PRIMARY KEY (scope, key)
		)`,
		`INSERT INTO ui_state VALUES ('client:abc','panes','{"open":["t1"]}',1), ('client:abc','sidebar','w',1)`,
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
			`INSERT INTO thread_import_state VALUES ('t1','claude','/home/real/.claude/x.jsonl','sess-1111','leaf-2222','parent-3333',1)`,
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
	if threads != 2 {
		t.Fatalf("threads = %d, want 2 (the clone dropped rows)", threads)
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
	var sessionID, leaf, parent, path string
	if err := db.QueryRow(
		`SELECT source_session_id, leaf_uuid, source_parent_session_id, source_path
		   FROM thread_import_state WHERE thread_id = 't1'`,
	).Scan(&sessionID, &leaf, &parent, &path); err != nil {
		t.Fatal(err)
	}
	if sessionID != "" || leaf != "" || parent != "" {
		t.Fatalf("import identity survived: session=%q leaf=%q parent=%q", sessionID, leaf, parent)
	}
	if path == "" {
		t.Fatal("the import row was rewritten past its identity columns")
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
