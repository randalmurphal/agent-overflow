package store

import (
	"strings"
	"testing"
)

// v145 leaves every existing comment settled, and a user row's insert
// clears the claim that names it, in its thread only.
func TestReviewClaimRowsMigrationSettlesExistingCommentsAndTheRowInsertClearsAClaim(t *testing.T) {
	db := migrateThrough(t, reviewClaimRowsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','claude','m','/p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('u','p','U','claude','m','/p',1,1)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,status,side,body,sent_at,sent_turn_id,created_at,updated_at)
		VALUES ('old','t','workspace','k','a.go','sent','file','b',7,'t:1',1,1)`)
	if err := applyMigration(db, migrationByVersion(t, reviewClaimRowsMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var claimThread, claimItem string
	if err := db.QueryRow(`SELECT claim_thread_id, claim_item_id FROM diff_review_comments WHERE id = 'old'`).Scan(&claimThread, &claimItem); err != nil || claimThread != "" || claimItem != "" {
		t.Fatalf("existing comment claim = %q/%q (%v), want settled", claimThread, claimItem, err)
	}

	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,status,side,body,sent_at,sent_turn_id,created_at,updated_at,claim_thread_id,claim_item_id)
		VALUES ('new','t','workspace','k','b.go','sent','file','b',8,'t:2','1','1','t','user:2')`)
	// The same row id in another thread, and another kind of row with the
	// id, settle nothing.
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','u',2,0,'user_text','user','completed','x',1,1)`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','t',2,0,'assistant_text','assistant','completed','x',1,1)`)
	if err := db.QueryRow(`SELECT claim_item_id FROM diff_review_comments WHERE id = 'new'`).Scan(&claimItem); err != nil || claimItem != "user:2" {
		t.Fatalf("claim after unrelated rows = %q (%v), want it pending", claimItem, err)
	}
	mustExec(t, db, `DELETE FROM items WHERE thread_id = 't' AND id = 'user:2'`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','t',2,0,'user_text','user','completed','x',1,1)`)
	if err := db.QueryRow(`SELECT claim_thread_id, claim_item_id FROM diff_review_comments WHERE id = 'new'`).Scan(&claimThread, &claimItem); err != nil || claimThread != "" || claimItem != "" {
		t.Fatalf("claim after its row = %q/%q (%v), want settled", claimThread, claimItem, err)
	}

	// Every user row's insert runs the trigger's lookup, so it reads the
	// claim index, not the table.
	rows, err := db.Query(`EXPLAIN QUERY PLAN UPDATE diff_review_comments SET claim_thread_id = '', claim_item_id = ''
		WHERE claim_item_id <> '' AND claim_thread_id = ? AND claim_item_id = ?`, "t", "user:2")
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
