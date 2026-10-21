package store

import (
	"strings"
	"testing"
)

// v145 leaves every existing comment settled, and a user row's insert
// settles the claim of a message its row delivers, in its thread only. A
// queued message's claim waits for the provider's confirmation, and the
// claimed row stays named either way.
func TestReviewClaimRowsMigrationSettlesExistingCommentsAndTheRowInsertSettlesARowClaim(t *testing.T) {
	db := migrateThrough(t, reviewClaimRowsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','claude','m','/p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('u','p','U','claude','m','/p',1,1)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,status,side,body,sent_at,sent_turn_id,created_at,updated_at)
		VALUES ('old','t','workspace','k','a.go','sent','file','b',7,'t:1',1,1)`)
	if err := applyMigration(db, migrationByVersion(t, reviewClaimRowsMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	claim := func(id string) (string, string, string) {
		t.Helper()
		var thread, item, settles string
		if err := db.QueryRow(`SELECT claim_thread_id, claim_item_id, claim_settles FROM diff_review_comments WHERE id = ?`, id).Scan(&thread, &item, &settles); err != nil {
			t.Fatal(err)
		}
		return thread, item, settles
	}
	if thread, item, settles := claim("old"); thread != "" || item != "" || settles != "" {
		t.Fatalf("existing comment claim = %q/%q/%q, want none", thread, item, settles)
	}
	if _, err := db.Exec(`UPDATE diff_review_comments SET claim_settles = 'later' WHERE id = 'old'`); err == nil {
		t.Fatal("claim_settles took a value outside its set")
	}

	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,status,side,body,sent_at,sent_turn_id,created_at,updated_at,claim_thread_id,claim_item_id,claim_settles)
		VALUES ('new','t','workspace','k','b.go','sent','file','b',8,'t:2','1','1','t','user:2','row'),
		       ('queued','t','workspace','k','c.go','sent','file','b',8,'t:2','1','1','t','user:2','confirm')`)
	// The same row id in another thread, and another kind of row with the
	// id, settle nothing.
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','u',2,0,'user_text','user','completed','x',1,1)`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','t',2,0,'assistant_text','assistant','completed','x',1,1)`)
	if _, _, settles := claim("new"); settles != "row" {
		t.Fatalf("claim after unrelated rows = %q, want it pending", settles)
	}
	mustExec(t, db, `DELETE FROM items WHERE thread_id = 't' AND id = 'user:2'`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','t',2,0,'user_text','user','completed','x',1,1)`)
	if thread, item, settles := claim("new"); thread != "t" || item != "user:2" || settles != "" {
		t.Fatalf("claim after its row = %q/%q/%q, want settled naming the row", thread, item, settles)
	}
	if thread, item, settles := claim("queued"); thread != "t" || item != "user:2" || settles != "confirm" {
		t.Fatalf("queued claim after its row = %q/%q/%q, want it waiting for the confirmation", thread, item, settles)
	}

	// Every user row's insert runs the trigger's lookup, so it reads the
	// claim index, not the table.
	rows, err := db.Query(`EXPLAIN QUERY PLAN UPDATE diff_review_comments SET claim_settles = ''
		WHERE claim_item_id <> '' AND claim_thread_id = ? AND claim_item_id = ? AND claim_settles = 'row'`, "t", "user:2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if len(plan) != 1 || !strings.Contains(plan[0], "USING INDEX idx_diff_review_comments_claim") {
		t.Fatalf("trigger lookup plan = %q, want the claim index", plan)
	}
}
