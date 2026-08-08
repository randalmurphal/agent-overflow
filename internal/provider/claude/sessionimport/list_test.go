package sessionimport

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/importir"
)

func mustStat(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info
}

func itoa(i int) string { return strconv.Itoa(i) }

const (
	sessionA = "11111111-1111-4111-8111-111111111111"
	sessionB = "22222222-2222-4222-8222-222222222222"
)

func projectsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "-repo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root
}

func listOne(t *testing.T, root string) (SessionInfo, []SessionInfo) {
	t.Helper()
	sessions, _, err := List(context.Background(), Options{ProjectsDir: root, Concurrency: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) == 0 {
		return SessionInfo{}, sessions
	}
	return sessions[0], sessions
}

func TestListRequiresProjectsDir(t *testing.T) {
	if _, _, err := List(context.Background(), Options{}); err == nil {
		t.Fatal("List with no ProjectsDir: want error, got nil")
	}
}

func TestListMissingProjectsDirIsAnError(t *testing.T) {
	_, _, err := List(context.Background(), Options{ProjectsDir: filepath.Join(t.TempDir(), "nope")})
	if err == nil {
		t.Fatal("List over a missing projects dir: want error, got nil")
	}
}

func TestListTitlePrecedence(t *testing.T) {
	base := []any{
		userRow("u1", "", "first typed prompt", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("hi")}, "2026-01-01T00:00:01.000Z"),
	}

	cases := []struct {
		name  string
		extra []any
		want  string
	}{
		{"first prompt is the last resort", nil, "first typed prompt"},
		{
			"legacy summary beats first prompt",
			[]any{map[string]any{"type": "summary", "summary": "legacy summary"}},
			"legacy summary",
		},
		{
			"last prompt beats legacy summary",
			[]any{
				map[string]any{"type": "summary", "summary": "legacy summary"},
				map[string]any{"type": "last-prompt", "lastPrompt": "newest prompt", "leafUuid": "a1"},
			},
			"newest prompt",
		},
		{
			"ai title beats last prompt",
			[]any{
				map[string]any{"type": "last-prompt", "lastPrompt": "newest prompt", "leafUuid": "a1"},
				map[string]any{"type": "ai-title", "aiTitle": "Generated title"},
			},
			"Generated title",
		},
		{
			"custom title beats ai title",
			[]any{
				map[string]any{"type": "last-prompt", "lastPrompt": "newest prompt", "leafUuid": "a1"},
				map[string]any{"type": "ai-title", "aiTitle": "Generated title"},
				map[string]any{"type": "custom-title", "customTitle": "User rename"},
			},
			"User rename",
		},
		{
			"the last custom title wins",
			[]any{
				map[string]any{"type": "custom-title", "customTitle": "First rename"},
				map[string]any{"type": "custom-title", "customTitle": "Second rename"},
			},
			"Second rename",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := projectsFixture(t)
			writeJSONL(t, filepath.Join(root, "-repo", sessionA+".jsonl"), append(append([]any{}, base...), tc.extra...)...)
			got, _ := listOne(t, root)
			if got.Title != tc.want {
				t.Errorf("title = %q, want %q", got.Title, tc.want)
			}
		})
	}
}

func TestListSkipsSidechainFile(t *testing.T) {
	root := projectsFixture(t)
	writeJSONL(t, filepath.Join(root, "-repo", sessionA+".jsonl"),
		userRow("u1", "", "subagent prompt", "2026-01-01T00:00:00.000Z", with("isSidechain", true)),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("done")}, "2026-01-01T00:00:01.000Z", with("isSidechain", true)),
	)
	_, sessions := listOne(t, root)
	if len(sessions) != 0 {
		t.Fatalf("sidechain file listed: %+v", sessions)
	}
}

func TestListSkipsMetadataOnlyFile(t *testing.T) {
	root := projectsFixture(t)
	writeJSONL(t, filepath.Join(root, "-repo", sessionA+".jsonl"),
		map[string]any{"type": "mode", "mode": "chat", "sessionId": sessionA},
		map[string]any{"type": "queue-operation", "sessionId": sessionA},
	)
	_, sessions := listOne(t, root)
	if len(sessions) != 0 {
		t.Fatalf("metadata-only file listed: %+v", sessions)
	}
}

func TestListSkipsNonSessionFiles(t *testing.T) {
	root := projectsFixture(t)
	writeJSONL(t, filepath.Join(root, "-repo", "notes.jsonl"),
		userRow("u1", "", "hello", "2026-01-01T00:00:00.000Z"))
	writeJSONL(t, filepath.Join(root, "-repo", sessionA+".txt"),
		userRow("u1", "", "hello", "2026-01-01T00:00:00.000Z"))
	if err := os.MkdirAll(filepath.Join(root, "-repo", sessionA), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, sessions := listOne(t, root)
	if len(sessions) != 0 {
		t.Fatalf("non-session entries listed: %+v", sessions)
	}
}

func TestListExtractsSessionFields(t *testing.T) {
	root := projectsFixture(t)
	writeJSONL(t, filepath.Join(root, "-repo", sessionA+".jsonl"),
		userRow("u1", "", "do the thing", "2026-03-04T05:06:07.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("ok")}, "2026-03-04T05:06:09.000Z"),
		map[string]any{"type": "custom-title", "customTitle": "Titled"},
	)
	// Two subagent transcripts plus their meta sidecars: the count must
	// follow the transcripts, not the directory entries.
	agents := filepath.Join(root, "-repo", sessionA, subagentsSubdir)
	writeJSONL(t, filepath.Join(agents, "agent-aaa.jsonl"), userRow("s1", "", "go", "2026-03-04T05:06:08.000Z"))
	writeJSONL(t, filepath.Join(agents, "agent-bbb.jsonl"), userRow("s1", "", "go", "2026-03-04T05:06:08.000Z"))
	if err := os.WriteFile(filepath.Join(agents, "agent-aaa.meta.json"), []byte(`{"toolUseId":"toolu_1"}`), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agents, "agent-bbb.meta.json"), []byte(`{"toolUseId":"toolu_2"}`), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	got, _ := listOne(t, root)
	if got.SessionID != sessionA {
		t.Errorf("sessionID = %q", got.SessionID)
	}
	if got.Title != "Titled" {
		t.Errorf("title = %q", got.Title)
	}
	if got.ProjectPath != "/repo" {
		t.Errorf("projectPath = %q, want /repo", got.ProjectPath)
	}
	if got.GitBranch != "main" {
		t.Errorf("gitBranch = %q, want main", got.GitBranch)
	}
	if want := parseISOMillis("2026-03-04T05:06:07.000Z"); got.CreatedAt != want {
		t.Errorf("createdAt = %d, want %d", got.CreatedAt, want)
	}
	if got.SubagentCount != 2 {
		t.Errorf("subagentCount = %d, want 2", got.SubagentCount)
	}
	if got.SizeBytes <= 0 {
		t.Errorf("sizeBytes = %d, want > 0", got.SizeBytes)
	}
	if got.ForkedFromSessionID != "" {
		t.Errorf("forkedFromSessionID = %q, want empty", got.ForkedFromSessionID)
	}
}

func TestListDetectsForkProvenance(t *testing.T) {
	root := projectsFixture(t)
	writeJSONL(t, filepath.Join(root, "-repo", sessionB+".jsonl"),
		userRow("u1", "", "forked prompt", "2026-01-01T00:00:00.000Z",
			with("sessionId", sessionB),
			with("forkedFrom", map[string]any{"sessionId": sessionA, "messageUuid": "orig-1"})),
	)
	got, _ := listOne(t, root)
	if got.ForkedFromSessionID != sessionA {
		t.Errorf("forkedFromSessionID = %q, want %q", got.ForkedFromSessionID, sessionA)
	}
	if got.SessionID != sessionB {
		t.Errorf("sessionID = %q, want %q", got.SessionID, sessionB)
	}
}

func TestListToleratesUnparseableLines(t *testing.T) {
	root := projectsFixture(t)
	writeJSONL(t, filepath.Join(root, "-repo", sessionA+".jsonl"),
		"{not json at all",
		userRow("u1", "", "real prompt", "2026-01-01T00:00:00.000Z"),
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"trunc`,
	)
	got, _ := listOne(t, root)
	if got.Title != "real prompt" {
		t.Errorf("title = %q, want %q", got.Title, "real prompt")
	}
}

func TestListSortsNewestFirstAndDedupes(t *testing.T) {
	root := projectsFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "-other"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	older := filepath.Join(root, "-other", sessionA+".jsonl")
	newer := filepath.Join(root, "-repo", sessionA+".jsonl")
	writeJSONL(t, older, userRow("u1", "", "stale copy", "2026-01-01T00:00:00.000Z"))
	writeJSONL(t, newer, userRow("u1", "", "live copy", "2026-01-01T00:00:00.000Z"))
	writeJSONL(t, filepath.Join(root, "-repo", sessionB+".jsonl"),
		userRow("u1", "", "other session", "2026-01-01T00:00:00.000Z"))

	stale := mustStat(t, older)
	if err := os.Chtimes(older, stale.ModTime(), stale.ModTime().Add(-time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	sessions, _, err := List(context.Background(), Options{ProjectsDir: root})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (deduped): %+v", len(sessions), sessions)
	}
	for _, s := range sessions {
		if s.SessionID == sessionA && !strings.HasSuffix(s.Path, filepath.Join("-repo", sessionA+".jsonl")) {
			t.Errorf("dedupe kept the stale copy: %s", s.Path)
		}
	}
	if sessions[0].LastActivityAt < sessions[1].LastActivityAt {
		t.Errorf("sessions not sorted newest first: %+v", sessions)
	}
}

func TestListWarnsOnUnreadableProjectDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := projectsFixture(t)
	locked := filepath.Join(root, "-locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeJSONL(t, filepath.Join(root, "-repo", sessionA+".jsonl"),
		userRow("u1", "", "readable", "2026-01-01T00:00:00.000Z"))
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	sessions, warnings, err := List(context.Background(), Options{ProjectsDir: root})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want the readable one", len(sessions))
	}
	if !hasWarning(warnings, WarnUnreadableProjectDir) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnUnreadableProjectDir)
	}
}

func TestListHeadTailWindowOnLargeFile(t *testing.T) {
	root := projectsFixture(t)
	filler := strings.Repeat("x", 4000)
	lines := []any{userRow("u1", "", "first prompt", "2026-01-01T00:00:00.000Z")}
	for i := 0; i < 60; i++ {
		lines = append(lines, assistantRow("a"+strings.Repeat("z", i%5)+itoa(i), "u1", "msg_"+itoa(i),
			[]any{textBlock(filler)}, "2026-01-01T00:01:00.000Z"))
	}
	lines = append(lines, map[string]any{"type": "custom-title", "customTitle": "Tail title"})
	path := filepath.Join(root, "-repo", sessionA+".jsonl")
	writeJSONL(t, path, lines...)

	info := mustStat(t, path)
	if info.Size() <= liteReadBufSize {
		t.Fatalf("fixture is %d bytes, needs to exceed the %d-byte window", info.Size(), liteReadBufSize)
	}

	got, _ := listOne(t, root)
	if got.Title != "Tail title" {
		t.Errorf("title = %q, want the tail-window title", got.Title)
	}
	if got.ProjectPath != "/repo" {
		t.Errorf("projectPath = %q — head window lost", got.ProjectPath)
	}
}

func hasWarning(warnings []importir.Warning, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestListSkipsSessionWithNoWorkspace(t *testing.T) {
	root := projectsFixture(t)
	writeJSONL(t, filepath.Join(root, "-repo", sessionA+".jsonl"),
		map[string]any{
			"type": "user", "uuid": "u1", "parentUuid": nil, "isSidechain": false,
			"timestamp": "2026-01-01T00:00:00.000Z",
			"message":   map[string]any{"role": "user", "content": "no cwd anywhere"},
		},
	)
	sessions, warnings, err := List(context.Background(), Options{ProjectsDir: root})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("a session with no workspace was listed as importable: %+v", sessions)
	}
	if !hasWarning(warnings, WarnMissingWorkspace) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnMissingWorkspace)
	}
}
