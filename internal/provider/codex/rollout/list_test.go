package rollout

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// fixtureSchema is the subset of Codex's `threads` table this package reads,
// with the same column names and types as the real state_5.sqlite (verified
// against codex-rs/state/migrations). Only the columns listQuery touches are
// present: a fixture that mirrored the whole table would rot without telling
// us anything the query does not ask for.
const fixtureSchema = `
CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    rollout_path TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    source TEXT NOT NULL,
    cwd TEXT NOT NULL,
    title TEXT NOT NULL,
    tokens_used INTEGER NOT NULL DEFAULT 0,
    has_user_event INTEGER NOT NULL DEFAULT 0,
    archived INTEGER NOT NULL DEFAULT 0,
    git_branch TEXT,
    first_user_message TEXT NOT NULL DEFAULT '',
    model TEXT,
    reasoning_effort TEXT,
    created_at_ms INTEGER,
    updated_at_ms INTEGER,
    thread_source TEXT,
    preview TEXT NOT NULL DEFAULT '',
    recency_at_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE thread_spawn_edges (
    parent_thread_id TEXT NOT NULL,
    child_thread_id TEXT NOT NULL PRIMARY KEY,
    status TEXT NOT NULL
);`

type fixtureThread struct {
	id           string
	rolloutPath  string
	source       string
	threadSource string
	preview      string
	archived     int
	recencyMS    int64
}

func writeStateDB(t *testing.T, home string, schema string, rows []fixtureThread, spawnChildren []string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, StateDBName))
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}
	for _, row := range rows {
		if _, err := db.Exec(`
INSERT INTO threads (id, rollout_path, created_at, updated_at, source, cwd, title,
                     first_user_message, archived, thread_source, preview, recency_at_ms,
                     created_at_ms, updated_at_ms, git_branch, model, reasoning_effort, tokens_used)
VALUES (?, ?, 1700, 1800, ?, '/repo', ?, ?, ?, ?, ?, ?, 1700000, 1800000, 'main', 'gpt-5.6-sol', 'high', 42)`,
			row.id, row.rolloutPath, row.source, "Title "+row.id, "Prompt "+row.id,
			row.archived, nullable(row.threadSource), row.preview, row.recencyMS,
		); err != nil {
			t.Fatalf("insert fixture thread %s: %v", row.id, err)
		}
	}
	for _, child := range spawnChildren {
		if _, err := db.Exec(
			`INSERT INTO thread_spawn_edges (parent_thread_id, child_thread_id, status) VALUES ('parent', ?, 'done')`,
			child,
		); err != nil {
			t.Fatalf("insert spawn edge %s: %v", child, err)
		}
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func touchRollout(t *testing.T, home, name string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

func TestListExcludesArchivedSubagentAndEmptyThreads(t *testing.T) {
	home := t.TempDir()
	keep := touchRollout(t, home, "keep.jsonl")
	rows := []fixtureThread{
		{id: "keep", rolloutPath: keep, source: "cli", preview: "hello", recencyMS: 900},
		{id: "archived", rolloutPath: keep, source: "cli", preview: "hello", archived: 1, recencyMS: 800},
		{id: "sub-by-source", rolloutPath: keep, source: `{"subagent":{"thread_spawn":{}}}`, preview: "hello", recencyMS: 700},
		{id: "sub-by-column", rolloutPath: keep, source: "cli", threadSource: "subagent", preview: "hello", recencyMS: 600},
		{id: "guardian-review", rolloutPath: keep, source: "cli", threadSource: "guardian_review", preview: "hello", recencyMS: 550},
		{id: "sub-by-edge", rolloutPath: keep, source: "cli", preview: "hello", recencyMS: 500},
		{id: "no-preview", rolloutPath: keep, source: "cli", preview: "", recencyMS: 400},
	}
	writeStateDB(t, home, fixtureSchema, rows, []string{"sub-by-edge"})

	got, warnings, err := List(context.Background(), ListOptions{CodexHome: home})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	if len(got) != 1 || got[0].ThreadID != "keep" {
		t.Fatalf("want only the top-level non-archived thread, got %+v", got)
	}
	if got[0].CreatedAt != 1700000 || got[0].LastActivityAt != 1800000 {
		t.Fatalf("timestamps not carried: %+v", got[0])
	}
	if got[0].SizeBytes == 0 {
		t.Fatalf("rollout size not stat'ed: %+v", got[0])
	}
	if got[0].Model != "gpt-5.6-sol" || got[0].ReasoningEffort != "high" {
		t.Fatalf("indexed fallback profile not carried: %+v", got[0])
	}
}

// The legacy `has_user_event` column is 0 on every row current Codex writes.
// Filtering on it (as an earlier plan did) would hide every session, so this
// pins that a row with has_user_event=0 still lists.
func TestListDoesNotFilterOnHasUserEvent(t *testing.T) {
	home := t.TempDir()
	keep := touchRollout(t, home, "keep.jsonl")
	writeStateDB(t, home, fixtureSchema, []fixtureThread{
		{id: "keep", rolloutPath: keep, source: "cli", preview: "hello", recencyMS: 900},
	}, nil)

	got, _, err := List(context.Background(), ListOptions{CodexHome: home})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("has_user_event=0 row was filtered out: %+v", got)
	}
}

func TestListWarnsAndDropsMissingRollout(t *testing.T) {
	home := t.TempDir()
	present := touchRollout(t, home, "present.jsonl")
	writeStateDB(t, home, fixtureSchema, []fixtureThread{
		{id: "present", rolloutPath: present, source: "cli", preview: "hi", recencyMS: 900},
		{id: "gone", rolloutPath: filepath.Join(home, "sessions", "gone.jsonl"), source: "cli", preview: "hi", recencyMS: 800},
	}, nil)

	got, warnings, err := List(context.Background(), ListOptions{CodexHome: home})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ThreadID != "present" {
		t.Fatalf("missing-rollout row should be dropped, got %+v", got)
	}
	if len(warnings) != 1 || warnings[0].Code != WarnRolloutMissing {
		t.Fatalf("want one missing-rollout warning, got %+v", warnings)
	}
	if !strings.Contains(warnings[0].Message, "gone") {
		t.Fatalf("warning should name the session: %q", warnings[0].Message)
	}
}

func TestListFailsLoudlyWhenDatabaseIsAbsent(t *testing.T) {
	_, _, err := List(context.Background(), ListOptions{CodexHome: t.TempDir()})
	if err == nil {
		t.Fatal("want an error when the codex thread index does not exist")
	}
	if !strings.Contains(err.Error(), StateDBName) {
		t.Fatalf("error should name the database: %v", err)
	}
}

func TestListFailsLoudlyOnSchemaMismatch(t *testing.T) {
	home := t.TempDir()
	// A future Codex that moved the columns we read: the table exists, the
	// query does not. This must be an error, never an empty list.
	writeStateDB(t, home, `CREATE TABLE threads (id TEXT PRIMARY KEY, something_else TEXT);`, nil, nil)

	_, _, err := List(context.Background(), ListOptions{CodexHome: home})
	if err == nil {
		t.Fatal("want an error when the schema has moved")
	}
	if !strings.Contains(err.Error(), "schema may have moved") {
		t.Fatalf("error should name the diagnosis: %v", err)
	}
}

func TestListRequiresCodexHome(t *testing.T) {
	if _, _, err := List(context.Background(), ListOptions{}); err == nil {
		t.Fatal("want an error when CodexHome is empty")
	}
}

// `rollout_path` comes out of a database AO does not own, so a row naming a
// file outside the Codex home must be skipped with a warning — and the file
// must not be stat'ed at all, which is what makes the check a containment
// boundary rather than an after-the-fact diagnosis. A moved home lands on the
// same branch by construction: its recorded paths are outside the current one.
func TestListSkipsRolloutPathsOutsideTheCodexHome(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	inside := touchRollout(t, home, "keep.jsonl")
	writeStateDB(t, home, fixtureSchema, []fixtureThread{
		{id: "keep", rolloutPath: inside, source: "cli", preview: "hi", recencyMS: 900},
		{id: "escape", rolloutPath: outside, source: "cli", preview: "hi", recencyMS: 800},
		{id: "traverse", rolloutPath: filepath.Join(home, "sessions", "..", "..", "etc", "passwd"),
			source: "cli", preview: "hi", recencyMS: 700},
	}, nil)

	got, warnings, err := List(context.Background(), ListOptions{CodexHome: home})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ThreadID != "keep" {
		t.Fatalf("only the contained rollout should list, got %+v", got)
	}
	if len(warnings) != 2 {
		t.Fatalf("want one warning per escaping row, got %+v", warnings)
	}
	for _, warning := range warnings {
		if warning.Code != WarnRolloutOutside {
			t.Fatalf("want the outside-home code, got %+v", warning)
		}
		if strings.Contains(warning.Message, outside) {
			t.Fatalf("warning must not echo the path it refused to read: %q", warning.Message)
		}
	}
}

func TestPathInHomeAcceptsContainedRejectsEverythingElse(t *testing.T) {
	home := t.TempDir()
	contained := filepath.Join(home, "sessions", "a.jsonl")
	got, err := PathInHome(home, contained)
	if err != nil || got != filepath.Clean(contained) {
		t.Fatalf("contained path: got %q, %v", got, err)
	}
	for name, path := range map[string]string{
		"parent":    filepath.Join(home, ".."),
		"traversal": filepath.Join(home, "..", "elsewhere", "x.jsonl"),
		"absolute":  "/etc/passwd",
		"home":      home,
		"empty":     "",
	} {
		if _, err := PathInHome(home, path); !errors.Is(err, ErrOutsideCodexHome) {
			t.Errorf("%s (%q): want ErrOutsideCodexHome, got %v", name, path, err)
		}
	}
	if _, err := PathInHome("", contained); !errors.Is(err, ErrOutsideCodexHome) {
		t.Errorf("an empty home must refuse every path, got %v", err)
	}
}
