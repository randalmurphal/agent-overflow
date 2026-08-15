//go:build !providersmoke

package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	claudesessions "agent-overflow/internal/provider/claude/sessionimport"
	importwriter "agent-overflow/internal/sessionimport"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// transcriptRows reads a Claude session file back as decoded rows.
func transcriptRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	var rows []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("decode row in %s: %v", path, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return rows
}

func threadsBySessionRef(threads []store.Thread) (active store.Thread, abandoned store.Thread, ok bool) {
	for _, thread := range threads {
		if thread.SessionRef == "" {
			abandoned = thread
		} else {
			active = thread
		}
	}
	return active, abandoned, active.ID != "" && abandoned.ID != ""
}

// importClaudeSessionWithLegacyAbandonedBranch recreates the persisted shape
// produced by releases that imported every Claude DAG leaf. New imports only
// create the active thread; the branch materializer remains as an upgrade
// compatibility path for ref-less inactive threads already in user stores.
func importClaudeSessionWithLegacyAbandonedBranch(
	t *testing.T, app *App, home importHome, sessionID string,
) []store.Thread {
	t.Helper()
	current := importOneClaudeSession(t, app, home, sessionID)
	if len(current) != 1 {
		t.Fatalf("current import threads = %d, want one active thread", len(current))
	}
	active := current[0]

	loaded, err := claudesessions.LoadSession(home.claudeSessionPath(sessionID))
	if err != nil {
		t.Fatalf("LoadSession for legacy fixture: %v", err)
	}
	defer loaded.Close()
	if len(loaded.Branches) < 2 {
		t.Fatalf("legacy fixture branches = %d, want an inactive sibling", len(loaded.Branches))
	}
	branch, err := loaded.ConvertBranch(0)
	if err != nil {
		t.Fatalf("convert legacy inactive branch: %v", err)
	}

	abandoned := active
	abandoned.ID = uuid.NewString()
	abandoned.Title = "Legacy imported inactive branch"
	abandoned.SessionRef = ""
	abandoned.Model = branch.Profile.Model
	abandoned.ReasoningEffort = branch.Profile.ReasoningEffort
	abandoned.ContextWindow = branch.Profile.ContextWindow
	abandoned.UpdatedAt = branch.LastActivityAt
	abandoned.LastReadAt = &abandoned.UpdatedAt
	if err := app.store.CreateThread(abandoned); err != nil {
		t.Fatalf("CreateThread legacy inactive branch: %v", err)
	}
	batch, warnings, err := importwriter.NewWriter(app.store, abandoned).Build(branch.Events)
	if err != nil {
		t.Fatalf("Build legacy inactive branch: %v", err)
	}
	for _, warning := range warnings {
		if warning.Code == "import.unmapped-event" {
			t.Fatalf("legacy inactive branch writer drift: %s", warning.Message)
		}
	}
	if err := app.store.ApplyImportBatch(abandoned.ID, batch); err != nil {
		t.Fatalf("ApplyImportBatch legacy inactive branch: %v", err)
	}
	state := store.ThreadImportState{
		ThreadID: abandoned.ID, Provider: importwriter.ProviderClaude,
		SourcePath: home.claudeSessionPath(sessionID),
		// The pre-v63 importer recorded sessionID on every leaf. Migration tests
		// prove those duplicate identities survive v63 and remain refreshable;
		// this App fixture is created after v63, where NEW duplicates are
		// deliberately refused. Branch materialization reads the source path and
		// leaf only, so give this synthetic compatibility row its own identity
		// instead of weakening the production invariant to manufacture old data.
		SourceSessionID: "legacy-branch:" + abandoned.ID,
		LeafUUID:        branch.LeafUUID,
	}
	importwriter.NewCursor(batch, branch.Events).Apply(&state)
	if err := app.store.SetThreadImportState(state); err != nil {
		t.Fatalf("SetThreadImportState legacy inactive branch: %v", err)
	}
	return []store.Thread{abandoned, active}
}

// The App-level progress contract must agree with the importer: one selected
// Claude session emits one thread id even when its transcript has two leaves.
func TestImportedClaudeSessionCreatesOnlyTheActiveThread(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)

	threads := importOneClaudeSession(t, app, home, importFixtureClaudeBranchy)
	if len(threads) != 1 {
		t.Fatalf("threads = %d, want one active provider session", len(threads))
	}
	active := threads[0]
	if active.SessionRef != importFixtureClaudeBranchy {
		t.Fatalf("active branch sessionRef = %q, want the source session %q",
			active.SessionRef, importFixtureClaudeBranchy)
	}
	state, found, err := app.store.GetThreadImportState(active.ID)
	if err != nil || !found {
		t.Fatalf("GetThreadImportState(%s) = %v, %v", active.ID, found, err)
	}
	if state.LeafUUID != "a2b" {
		t.Fatalf("active branch leaf = %q, want file-order-last leaf a2b", state.LeafUUID)
	}
}

func TestImportedClaudeBranchGetsItsOwnSessionOnFirstStart(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)

	threads := importClaudeSessionWithLegacyAbandonedBranch(t, app, home, importFixtureClaudeBranchy)
	active, abandoned, ok := threadsBySessionRef(threads)
	if !ok {
		t.Fatalf("threads = %+v, want exactly one with a session ref", threads)
	}

	// This is what the session-start path calls before it builds its options.
	materialized := app.materializeImportedClaudeBranch(abandoned)
	if materialized.SessionRef == "" {
		t.Fatal("abandoned branch got no session ref; it would start a fresh session with no history")
	}
	if materialized.SessionRef == importFixtureClaudeBranchy {
		t.Fatal("abandoned branch was pointed at the source session, which resumes the OTHER branch")
	}
	stored, err := app.store.GetThread(abandoned.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.SessionRef != materialized.SessionRef {
		t.Fatalf("stored sessionRef = %q, want the materialized %q", stored.SessionRef, materialized.SessionRef)
	}
	if stored.ImportSource != "claude" {
		t.Errorf("importSource = %q, want it preserved across the update", stored.ImportSource)
	}

	// The session-start path resumes that id directly — no --fork-session,
	// which would fork the SOURCE at its tail (the wrong branch).
	opts := provider.SessionOptionsFromThread(
		stored, provider.AutoCompactDefaults{}, "", stored.PendingForkRef != "")
	if opts.Resume != materialized.SessionRef || opts.ForkSession {
		t.Fatalf("session options = {Resume:%q ForkSession:%v}, want a plain resume of %q",
			opts.Resume, opts.ForkSession, materialized.SessionRef)
	}

	// The cut file is a complete session whose LAST transcript row is this
	// branch's leaf — which is what makes it the file's active branch, and so
	// the one a resume lands on. The row carries the leaf as its fork
	// provenance because every uuid is reminted.
	cut := home.claudeSessionPath(materialized.SessionRef)
	rows := transcriptRows(t, cut)
	if len(rows) == 0 {
		t.Fatalf("cut session %s is empty", cut)
	}
	var lastTranscript map[string]any
	for _, row := range rows {
		switch row["type"] {
		case "user", "assistant", "system", "attachment":
			lastTranscript = row
		}
	}
	if lastTranscript == nil {
		t.Fatalf("cut session %s has no transcript rows", cut)
	}
	provenance, _ := lastTranscript["forkedFrom"].(map[string]any)
	if provenance["messageUuid"] != "a2a" {
		t.Fatalf("cut session's last row came from %v, want the branch leaf a2a", provenance["messageUuid"])
	}
	if lastTranscript["sessionId"] != materialized.SessionRef {
		t.Fatalf("cut session rows claim session %v, want %q",
			lastTranscript["sessionId"], materialized.SessionRef)
	}
	// The other branch's rows are not in the cut: the slice stops at the leaf.
	for _, row := range rows {
		if from, _ := row["forkedFrom"].(map[string]any); from != nil && from["messageUuid"] == "a2b" {
			t.Fatal("the cut session carried the other branch's leaf; a resume would see both")
		}
	}
	cutUUIDs := make(map[string]bool, len(rows))
	for _, row := range rows {
		if id, _ := row["uuid"].(string); id != "" {
			cutUUIDs[id] = true
		}
	}
	items, err := app.store.ListItems(abandoned.ID)
	if err != nil {
		t.Fatalf("ListItems after materializing branch: %v", err)
	}
	for _, item := range items {
		if item.Kind != "user_text" || item.Role != "user" {
			continue
		}
		providerID := usermessage.ReadProviderItemID(item.Meta)
		if providerID == "" || !cutUUIDs[providerID] {
			t.Fatalf("materialized user item %s points at provider uuid %q absent from its cut transcript", item.ID, providerID)
		}
	}

	// Idempotent for a thread that already has a ref: the active branch and a
	// second start on the same thread both pass through untouched.
	if got := app.materializeImportedClaudeBranch(active); got.SessionRef != active.SessionRef {
		t.Fatalf("active branch sessionRef changed to %q; it already resumes correctly", got.SessionRef)
	}
	if got := app.materializeImportedClaudeBranch(stored); got.SessionRef != stored.SessionRef {
		t.Fatalf("second start minted another session %q; the first cut is the thread's session", got.SessionRef)
	}
}

// TestImportedClaudeBranchIsCutUnderTheThreadsCurrentWorkspace is the
// brick this fixes.
//
// Claude resolves `--resume <id>` against the slug of the cwd it is launched
// in. An imported branch has no session_ref until it is materialized, which
// is exactly what makes a workspace change before the first send a silent
// no-op — copyClaudeSessionForWorkspaceChange has no transcript to relocate.
// Cutting the branch beside its SOURCE would therefore leave it under the
// original cwd's slug while the resume looks under the new workspace's, and
// the first send hard-fails with "No conversation found".
func TestImportedClaudeBranchIsCutUnderTheThreadsCurrentWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)

	threads := importClaudeSessionWithLegacyAbandonedBranch(t, app, home, importFixtureClaudeBranchy)
	_, abandoned, ok := threadsBySessionRef(threads)
	if !ok {
		t.Fatalf("threads = %+v, want exactly one with a session ref", threads)
	}

	// The user moves the thread to a worktree before ever sending. Nothing
	// relocates, because there is no transcript for this thread yet.
	moved := filepath.Join(home.root, "worktrees", "fixture-repo", "BLITZ-188")
	if err := os.MkdirAll(moved, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", moved, err)
	}
	if err := app.store.UpdateWorkspacePath(abandoned.ID, moved); err != nil {
		t.Fatalf("UpdateWorkspacePath: %v", err)
	}
	abandoned.WorkspacePath = moved

	materialized := app.materializeImportedClaudeBranch(abandoned)
	if materialized.SessionRef == "" {
		t.Fatal("abandoned branch got no session ref; it would start a fresh session with no history")
	}
	stored, err := app.store.GetThread(abandoned.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.SessionRef != materialized.SessionRef {
		t.Fatalf("stored sessionRef = %q, want the materialized %q", stored.SessionRef, materialized.SessionRef)
	}

	destDir, ok, err := sessionfork.WorkspaceProjectDir(home.claudeProjectsDir(), moved)
	if err != nil || !ok {
		t.Fatalf("resolve project dir for %s: ok=%v err=%v", moved, ok, err)
	}
	cut := filepath.Join(destDir, materialized.SessionRef+".jsonl")
	if _, err := os.Stat(cut); err != nil {
		t.Fatalf("cut session is not under the new workspace's slug dir (%s): %v", destDir, err)
	}
	// And nowhere else: a copy beside the source would be an orphan
	// transcript in the user's `claude --resume` picker.
	beside := home.claudeSessionPath(materialized.SessionRef)
	if _, err := os.Stat(beside); err == nil {
		t.Fatalf("cut session also written beside the source at %s", beside)
	}
	// It is a real, complete session file, cut at this branch's own leaf.
	rows := transcriptRows(t, cut)
	if len(rows) == 0 {
		t.Fatalf("cut session %s is empty", cut)
	}
	if rows[len(rows)-1]["sessionId"] != materialized.SessionRef {
		t.Fatalf("cut session rows claim session %v, want %q",
			rows[len(rows)-1]["sessionId"], materialized.SessionRef)
	}
}

// When the destination slug cannot be computed the cut still happens beside
// the source — the behaviour that shipped, and never worse than refusing to
// write at all. A workspace directory that is GONE is the reachable case: a
// worktree removed before the thread was ever resumed.
func TestImportedClaudeBranchFallsBackToTheSourceDirectory(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)

	threads := importClaudeSessionWithLegacyAbandonedBranch(t, app, home, importFixtureClaudeBranchy)
	_, abandoned, ok := threadsBySessionRef(threads)
	if !ok {
		t.Fatalf("threads = %+v, want exactly one with a session ref", threads)
	}
	gone := filepath.Join(home.root, "worktrees", "removed")
	if err := app.store.UpdateWorkspacePath(abandoned.ID, gone); err != nil {
		t.Fatalf("UpdateWorkspacePath: %v", err)
	}
	abandoned.WorkspacePath = gone

	materialized := app.materializeImportedClaudeBranch(abandoned)
	if materialized.SessionRef == "" {
		t.Fatal("sessionRef = \"\", want the cut to still happen beside the source")
	}
	if _, err := os.Stat(home.claudeSessionPath(materialized.SessionRef)); err != nil {
		t.Fatalf("cut session is not beside the source: %v", err)
	}
}

// A thread AO created itself must never be touched by the branch materializer
// — it has no import state, and a stray session file would be worse than the
// fresh session it is entitled to.
func TestMaterializeImportedClaudeBranchIgnoresOrdinaryThreads(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-plain")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if got := app.materializeImportedClaudeBranch(thread); got.SessionRef != "" {
		t.Fatalf("sessionRef = %q, want an AO-created thread left alone", got.SessionRef)
	}
}

// The source transcript can be gone by the time the user sends. That degrades
// to a fresh session — the thread's own history is in SQLite either way — and
// must not fail the start.
func TestMaterializeImportedClaudeBranchDegradesWhenTheSourceIsGone(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)

	threads := importClaudeSessionWithLegacyAbandonedBranch(t, app, home, importFixtureClaudeBranchy)
	_, abandoned, ok := threadsBySessionRef(threads)
	if !ok {
		t.Fatalf("threads = %+v, want exactly one with a session ref", threads)
	}
	if err := os.Remove(home.claudeSessionPath(importFixtureClaudeBranchy)); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}
	if got := app.materializeImportedClaudeBranch(abandoned); got.SessionRef != "" {
		t.Fatalf("sessionRef = %q, want no ref when the source is gone", got.SessionRef)
	}
}

// claudeSessionFiles lists the transcripts in the fixture project directory,
// which is where a cut branch would land.
func claudeSessionFiles(t *testing.T, home importHome) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home.claudeProjectDir(), "*.jsonl"))
	if err != nil {
		t.Fatalf("glob transcripts: %v", err)
	}
	sort.Strings(matches)
	return matches
}

// The transcript can still be there while the branch this thread was cut from
// is not — the user rewound the session in Claude, or compaction rewrote the
// file. The materializer must degrade exactly as it does for a missing file:
// no ref, no half-written transcript left behind in the user's Claude home.
func TestMaterializeImportedClaudeBranchDegradesWhenTheLeafIsGone(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeBranchedSession(t, importFixtureClaudeBranchy)

	threads := importClaudeSessionWithLegacyAbandonedBranch(t, app, home, importFixtureClaudeBranchy)
	_, abandoned, ok := threadsBySessionRef(threads)
	if !ok {
		t.Fatalf("threads = %+v, want exactly one with a session ref", threads)
	}

	// Rewrite the source without the abandoned branch: its leaf a2a no longer
	// names a row in the file the thread's import state points at.
	home.writeClaudeSession(t, importFixtureClaudeBranchy,
		home.claudeUserRow("u1", "", "add a test", 0),
		home.claudeAssistantRow("a1", "u1", "msg-1", "Added it.", 1_000),
		home.claudeUserRow("u2b", "a1", "now benchmark it", 4_000),
		home.claudeAssistantRow("a2b", "u2b", "msg-2b", "Benchmarked.", 5_000),
		claudeLastPrompt("a2b", "now benchmark it"),
	)
	before := claudeSessionFiles(t, home)

	got := app.materializeImportedClaudeBranch(abandoned)
	if got.SessionRef != "" {
		t.Fatalf("sessionRef = %q, want no ref when the leaf is no longer in the file", got.SessionRef)
	}
	stored, err := app.store.GetThread(abandoned.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.SessionRef != "" {
		t.Fatalf("stored sessionRef = %q, want the row left alone on a degrade", stored.SessionRef)
	}
	if after := claudeSessionFiles(t, home); !slices.Equal(before, after) {
		t.Fatalf("transcripts = %v, want %v — a failed cut must leave no orphan session file", after, before)
	}
}

// The cut's destination is derived from the SOURCE transcript's own location,
// never from $HOME. The two agree in production and diverge under
// AO_HARNESS_KEEP_HOME — the app's Claude home is `<dataRoot>/home` while
// $HOME is the developer's own — where resolving through $HOME would write an
// imported branch's transcript into the real `~/.claude/projects` and break
// the harness's read-only-widening property.
func TestImportedBranchDestDirStaysUnderTheSourcesOwnHome(t *testing.T) {
	decoy := t.TempDir()
	t.Setenv("HOME", decoy)
	t.Setenv("USERPROFILE", decoy)

	appHome := t.TempDir()
	projects := filepath.Join(appHome, ".claude", "projects")
	workspace := filepath.Join(appHome, "repo")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workspace, err)
	}
	// The slug the source sits under is deliberately NOT the workspace's: a
	// thread that moved workspace is the whole reason the destination is
	// resolved rather than reused.
	source := filepath.Join(projects, "-an-older-slug", "11111111.jsonl")

	got := importedBranchDestDir(store.Thread{ID: "thread-1", WorkspacePath: workspace}, source)
	want, ok, err := sessionfork.WorkspaceProjectDir(projects, workspace)
	if err != nil || !ok {
		t.Fatalf("resolve project dir for %s: ok=%v err=%v", workspace, ok, err)
	}
	if got != want {
		t.Fatalf("destDir = %q, want %q — the projects dir the source was read from", got, want)
	}
	if strings.HasPrefix(got, decoy) {
		t.Fatalf("destDir = %q resolved through $HOME (%s) instead of the source's home", got, decoy)
	}
}
