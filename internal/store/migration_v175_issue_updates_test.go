package store

import (
	"testing"
	"time"
)

// v175 indexes each mirror's issues by their update time, over the issues
// stored before it too. The export's intervals read the issues updated
// since a time through it (TestMirrorExportSearchesByTheBoard); the
// history-due reads, which do not state its condition, keep their own
// (TestMirrorHistoryDueReadsCostAPage, TestMirrorStatementsReadByKey).
func TestIssueUpdatesMigration(t *testing.T) {
	db := migrateThrough(t, issueUpdatesMigrationVersion-1)
	mustExec(t, db, `INSERT INTO tracker_connections(id, kind, site, account, created_at) VALUES
		('j1', 'jira', 'https://example.atlassian.net', 'user1@example.com', 1)`)
	mustExec(t, db, `INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (7, 'j1', 1)`)
	mustExec(t, db, `WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < 20000)
		INSERT INTO tracker_cache_issues(root_id, issue_id, issue_key, updated_at) SELECT 7, 'i' || x, 'WEB-' || x, x FROM seq`)
	m := migrationByVersion(t, issueUpdatesMigrationVersion)
	began := time.Now()
	if err := applyMigration(db, m); err != nil {
		t.Fatalf("apply v%d: %v", m.Version, err)
	}
	t.Logf("v175 over 20,000 issues: %v", time.Since(began))
	assertRows(t, db, `SELECT group_concat(name, ',') FROM pragma_index_info('idx_tracker_cache_issues_updated')`, "root_id,updated_at")
	assertRows(t, db, `SELECT count(*) FROM tracker_cache_issues INDEXED BY idx_tracker_cache_issues_updated
		WHERE root_id = 7 AND updated_at > 0 AND updated_at > 19990`, "10")
}
