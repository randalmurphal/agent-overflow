package store

import "testing"

// The rebuild keeps every comment and its claim as it was, keeps the claim
// trigger and every index, and admits a range or a held reply only in the
// shapes the accessors write.
func TestReviewRangesRepliesMigrationKeepsCommentsAndAdmitsRangesAndReplies(t *testing.T) {
	db := migrateThrough(t, reviewRangesRepliesMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','claude','m','/p',1,1)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,commit_sha,file_path,status,old_line,new_line,side,selected_text,body,sent_at,sent_turn_id,created_at,updated_at,claim_thread_id,claim_item_id,claim_settles)
		VALUES ('c','t','pr','pr:github:o/r:1','abc','a.go','sent',2,3,'context','+x','fix',7,'t:2',5,6,'t','user:2','row')`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('d','github:o/r:1','pr','k','a.go','file','b',1,1)`)
	if err := applyRebuildMigration(db, migrationByVersion(t, reviewRangesRepliesMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(`SELECT thread_id||'|'||scope||'|'||source_key||'|'||commit_sha||'|'||file_path||'|'||status||'|'||old_line||'|'||new_line||'|'||side||'|'||selected_text||'|'||body||'|'||sent_at||'|'||sent_turn_id||'|'||created_at||'|'||updated_at||'|'||claim_thread_id||'|'||claim_item_id||'|'||claim_settles||'|'||start_old_line||'|'||start_new_line||'|'||reply_thread_id||'|'||reply_root_id FROM diff_review_comments WHERE id='c'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "t|pr|pr:github:o/r:1|abc|a.go|sent|2|3|context|+x|fix|7|t:2|5|6|t|user:2|row|0|0||0" {
		t.Fatalf("the rebuild changed a comment: %s", got)
	}
	var prKey string
	if err := db.QueryRow(`SELECT pr_key FROM diff_review_comments WHERE id='d'`).Scan(&prKey); err != nil || prKey != "github:o/r:1" {
		t.Fatalf("PR-owned comment = %q, %v", prKey, err)
	}
	// The claim trigger came back: a user row's insert settles its claim.
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES ('user:2','t',2,0,'user_text','user','completed','x',1,1)`)
	var settles string
	if err := db.QueryRow(`SELECT claim_settles FROM diff_review_comments WHERE id='c'`).Scan(&settles); err != nil || settles != "" {
		t.Fatalf("claim after its row = %q, %v; want the trigger to settle it", settles, err)
	}
	for _, index := range []string{"idx_diff_review_comments_scope", "idx_diff_review_comments_pr", "idx_diff_review_comments_claim"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s = %d, %v", index, n, err)
		}
	}
	mustExec(t, db, `INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,old_line,new_line,side,start_old_line,start_new_line,body,created_at,updated_at) VALUES ('r','k1','pr','k','a.go',0,9,'new',0,4,'b',1,1)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,reply_thread_id,reply_root_id,body,created_at,updated_at) VALUES ('h','k1','pr','k','','reply','PRRT_1',7,'b',1,1)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,reply_thread_id,body,created_at,updated_at) VALUES ('h2','k1','pr','other','','reply','PRRT_1','b',1,1)`)
	for _, bad := range []string{
		`INSERT INTO diff_review_comments(id,thread_id,scope,source_key,file_path,side,reply_thread_id,body,created_at,updated_at) VALUES ('scope','t','workspace','k','','reply','PRRT_1','b',1,1)`,
		`INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('nothread','k1','pr','k','','reply','b',1,1)`,
		`INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,reply_thread_id,body,created_at,updated_at) VALUES ('notreply','k1','pr','k','a.go','file','PRRT_2','b',1,1)`,
		`INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,start_new_line,body,created_at,updated_at) VALUES ('filerange','k1','pr','k','a.go','file',3,'b',1,1)`,
		`INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,new_line,side,start_new_line,body,created_at,updated_at) VALUES ('neg','k1','pr','k','a.go',3,'new',-1,'b',1,1)`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	// Two draft replies to one thread are rows the table keeps: the create
	// call refuses a second, recovery may make one.
	mustExec(t, db, `INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,reply_thread_id,body,created_at,updated_at) VALUES ('again','k1','pr','k','','reply','PRRT_1','b',1,1)`)
}
