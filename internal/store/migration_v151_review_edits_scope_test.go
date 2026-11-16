package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// The rebuild keeps every comment column for column, keeps the claim
// trigger and every index as they were, changes the table only by the scope
// CHECK, and admits an edits-scope comment.
func TestReviewEditsScopeMigrationKeepsCommentsAndAdmitsEdits(t *testing.T) {
	db := migrateThrough(t, reviewEditsScopeMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','claude','m','/p',1,1)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,commit_sha,file_path,status,old_line,new_line,side,selected_text,body,sent_at,sent_turn_id,created_at,updated_at,claim_thread_id,claim_item_id,claim_settles,start_old_line,start_new_line)
		VALUES ('c','t','workspace','diff:1','abc','a.go','sent',4,5,'context','+x','fix',7,'t:2',5,6,'t','user:2','row',2,3)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,reply_thread_id,reply_root_id,body,created_at,updated_at) VALUES ('h','github:o/r:1','pr','k','','reply','PRRT_1',9,'b',1,1)`)
	if _, err := db.Exec(`INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('e0','t','edits','edit:1','a.go','file','b',1,1)`); err == nil {
		t.Fatal("the table before v151 admitted an edits comment; this test no longer exercises the rebuild")
	}
	rows := func() string { return dumpRows(t, db, `SELECT * FROM diff_review_comments ORDER BY id`) }
	before := rows()
	tableBefore, othersBefore := diffReviewSchema(t, db)

	if err := applyRebuildMigration(db, migrationByVersion(t, reviewEditsScopeMigrationVersion)); err != nil {
		t.Fatal(err)
	}

	if after := rows(); after != before {
		t.Fatalf("the rebuild changed comments:\nbefore %s\nafter  %s", before, after)
	}
	tableAfter, othersAfter := diffReviewSchema(t, db)
	if othersAfter != othersBefore {
		t.Fatalf("the rebuild changed the table's indexes or triggers:\nbefore %s\nafter  %s", othersBefore, othersAfter)
	}
	widened := strings.Replace(tableBefore, "'branch', 'pr')", "'branch', 'pr', 'edits')", 1)
	if widened == tableBefore || tableAfter != widened {
		t.Fatalf("the rebuild changed more than the scope CHECK:\nwant %s\n got %s", widened, tableAfter)
	}
	// The claim trigger came back: a user row's insert settles its claim.
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','t',2,0,'user_text','user','completed','x',1,1)`)
	var settles string
	if err := db.QueryRow(`SELECT claim_settles FROM diff_review_comments WHERE id='c'`).Scan(&settles); err != nil || settles != "" {
		t.Fatalf("claim after its row = %q, %v; want the trigger to settle it", settles, err)
	}
	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,new_line,side,body,created_at,updated_at) VALUES ('e','t','edits','edit:1','a.go',3,'new','b',1,1)`)
	for _, bad := range []string{
		`INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('junk','t','junk','k','a.go','file','b',1,1)`,
		`INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('pr-edits','k1','edits','k','a.go','file','b',1,1)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// diffReviewSchema is the table's CREATE statement and the CREATE
// statements of its indexes and triggers, in name order.
func diffReviewSchema(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	var table string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'diff_review_comments'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	return table, dumpRows(t, db, `SELECT type, name, sql FROM sqlite_master WHERE type IN ('index', 'trigger') AND sql IS NOT NULL AND (tbl_name = 'diff_review_comments' OR sql LIKE '%diff_review_comments%') ORDER BY name`)
}

func dumpRows(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			fmt.Fprintf(&out, "%s=%v;", cols[i], v)
		}
		out.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}
