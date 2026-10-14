package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestThreadPullRequestsMigrationAddsAnEmptyTable(t *testing.T) {
	db := migrateThrough(t, threadPullRequestsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','claude','m','/p',1,1)`)
	if err := applyMigration(db, migrationByVersion(t, threadPullRequestsMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_pull_requests`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows = %d, %v; want an empty table", rows, err)
	}
	mustExec(t, db, `INSERT INTO thread_pull_requests(thread_id,forge,host,project,number,url,branch,worktree_path,seen_open_at) VALUES ('t','github','github.com','o/r',9,'https://github.com/o/r/pull/9','b','/wt',5)`)
	for _, bad := range []string{
		`INSERT INTO thread_pull_requests(thread_id,forge,host,project,number,url,branch,worktree_path,seen_open_at) VALUES ('t2','bitbucket','h','o/r',1,'u','b','/wt',5)`,
		`UPDATE thread_pull_requests SET outcome = 'maybe'`,
		`UPDATE thread_pull_requests SET number = 0`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 't'`)
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_pull_requests`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after the thread's delete = %d, %v; want the cascade", rows, err)
	}
}

func trackedPR(threadID string, number int) ThreadPullRequest {
	return ThreadPullRequest{
		ThreadID: threadID, Forge: "github", Host: "github.com", Project: "o/r", Number: number,
		URL: fmt.Sprintf("https://github.com/o/r/pull/%d", number), NodeID: "PR_node", Branch: "feature",
		WorktreePath: "/wt/feature", SeenOpenAt: 10,
	}
}

func TestRecordOpenThreadPullRequestTracksTheLatestOpenPR(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "a")
	mustCreateThread(t, s, "b")

	if changed, err := s.RecordOpenThreadPullRequest(trackedPR("a", 1)); err != nil || !changed {
		t.Fatalf("first record = %v, %v", changed, err)
	}
	again := trackedPR("a", 1)
	again.SeenOpenAt = 20
	if changed, err := s.RecordOpenThreadPullRequest(again); err != nil || changed {
		t.Fatalf("the same sighting again = %v, %v; want no write", changed, err)
	}
	if _, err := s.RecordOpenThreadPullRequest(trackedPR("b", 1)); err != nil {
		t.Fatal(err)
	}

	// The merge reaches every thread that tracks the PR, once.
	if n, err := s.MarkThreadPullRequestsMerged("github", "github.com", "o/r", 1, 500); err != nil || n != 2 {
		t.Fatalf("merged = %d, %v; want both threads", n, err)
	}
	if n, err := s.MarkThreadPullRequestsMerged("github", "github.com", "o/r", 1, 600); err != nil || n != 0 {
		t.Fatalf("merged twice = %d, %v; want the first merge time kept", n, err)
	}
	// A lagging open sighting of a merged PR changes nothing.
	moved := trackedPR("a", 1)
	moved.WorktreePath = "/wt/elsewhere"
	if changed, err := s.RecordOpenThreadPullRequest(moved); err != nil || changed {
		t.Fatalf("open sighting of a merged PR = %v, %v; want no write", changed, err)
	}
	pending, err := s.ListPendingThreadPullRequests()
	if err != nil || len(pending) != 2 || pending[0].MergedAt != 500 || pending[0].WorktreePath != "/wt/feature" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}

	if n, err := s.MarkThreadPullRequestsHandled("github", "github.com", "o/r", 1, PROutcomeRemoved, 700); err != nil || n != 2 {
		t.Fatalf("handled = %d, %v", n, err)
	}
	if n, err := s.MarkThreadPullRequestsHandled("github", "github.com", "o/r", 1, PROutcomeKept, 800); err != nil || n != 0 {
		t.Fatalf("handled twice = %d, %v; want once", n, err)
	}
	if pending, err := s.ListPendingThreadPullRequests(); err != nil || len(pending) != 0 {
		t.Fatalf("pending after handling = %+v, %v", pending, err)
	}
	row, ok, err := s.GetThreadPullRequest("a")
	if err != nil || !ok || row.Outcome != PROutcomeRemoved || row.HandledAt != 700 {
		t.Fatalf("handled row = %+v %v %v", row, ok, err)
	}

	// A follow-up PR on the thread replaces the handled one.
	if changed, err := s.RecordOpenThreadPullRequest(trackedPR("a", 2)); err != nil || !changed {
		t.Fatalf("follow-up PR = %v, %v", changed, err)
	}
	row, _, _ = s.GetThreadPullRequest("a")
	if row.Number != 2 || row.MergedAt != 0 || row.HandledAt != 0 || row.Outcome != "" {
		t.Fatalf("follow-up row = %+v", row)
	}

	// A PR closed without merging is tracked again when it reopens.
	if _, err := s.MarkThreadPullRequestsHandled("github", "github.com", "o/r", 2, PROutcomeClosed, 900); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.RecordOpenThreadPullRequest(trackedPR("a", 2)); err != nil || !changed {
		t.Fatalf("reopened PR = %v, %v", changed, err)
	}
	if row, _, _ = s.GetThreadPullRequest("a"); row.HandledAt != 0 || row.Outcome != "" {
		t.Fatalf("reopened row = %+v", row)
	}

	if _, err := s.MarkThreadPullRequestsHandled("github", "github.com", "o/r", 2, "maybe", 1); err == nil || !strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("unknown outcome = %v", err)
	}
	if _, err := s.RecordOpenThreadPullRequest(ThreadPullRequest{ThreadID: "a"}); err == nil {
		t.Fatal("an incomplete record was accepted")
	}
}

func TestLatestHumanUserMessageAtSkipsWhatAgentsWrote(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t-at")
	if _, found, err := s.LatestHumanUserMessageAt("t-at"); err != nil || found {
		t.Fatalf("an empty thread reported a message: found=%v err=%v", found, err)
	}
	for _, row := range []struct {
		id        string
		index     int
		createdAt int64
		meta      string
	}{
		{id: "u0", index: 0, createdAt: 100},
		{id: "u1", index: 1, createdAt: 200, meta: `{"sendId":"send-1"}`},
		{id: "u2", index: 2, createdAt: 300, meta: `{"origin":"agent-thread"}`},
		{id: "u3", index: 3, createdAt: 400, meta: `{"wire_only":1}`},
	} {
		if _, err := appendCarded(s, Item{
			ID: row.id, ThreadID: "t-at", TurnIndex: row.index, Kind: "user_text", Role: "user",
			Status: "completed", Summary: row.id, Meta: row.meta, CreatedAt: row.createdAt, UpdatedAt: row.createdAt,
		}); err != nil {
			t.Fatalf("append %s: %v", row.id, err)
		}
	}
	at, found, err := s.LatestHumanUserMessageAt("t-at")
	if err != nil || !found || at != 200 {
		t.Fatalf("LatestHumanUserMessageAt = %d %v %v, want 200", at, found, err)
	}
}
