package store

import (
	"database/sql"
	"strings"
	"testing"
)

var revTriggerNames = []string{"trg_items_rev_insert", "trg_items_rev_update", "trg_items_rev_delete"}

// revTriggerCarrierProbes EXPLAINs every statement of the installed row
// stamping triggers, with NEW/OLD references bound as parameters, and
// returns each plan step that reads idx_items_transcript_root.
func revTriggerCarrierProbes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var probes []string
	for _, name := range revTriggerNames {
		var text string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).Scan(&text); err != nil {
			t.Fatalf("read trigger %s: %v", name, err)
		}
		m := triggerPattern.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("cannot parse trigger %s:\n%s", name, text)
		}
		found := false
		for _, body := range strings.Split(m[2], ";") {
			statement := triggerRowRef.ReplaceAllString(strings.TrimSpace(body), "?")
			if statement == "" {
				continue
			}
			rows, err := db.Query("EXPLAIN QUERY PLAN "+statement, make([]any, strings.Count(statement, "?"))...)
			if err != nil {
				t.Fatalf("explain %s: %v\n%s", name, err, statement)
			}
			for rows.Next() {
				var id, parent, notUsed int
				var detail string
				if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(detail, "idx_items_transcript_root") {
					probes = append(probes, name+": "+detail)
					found = true
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
		}
		if !found {
			t.Fatalf("%s does not read idx_items_transcript_root", name)
		}
	}
	return probes
}

// carrierProbeByValue is the plan step of a carrier lookup keyed on the
// transcript root; a step keyed on thread_id alone reads every carrier of
// the thread.
const carrierProbeByValue = "idx_items_transcript_root (thread_id=? AND <expr>=?)"

func TestItemRevisionTriggersProbeCarriersByValue(t *testing.T) {
	s := newTestStore(t)
	for _, probe := range revTriggerCarrierProbes(t, s.db) {
		if !strings.Contains(probe, carrierProbeByValue) {
			t.Errorf("carrier leg is not keyed on the transcript root: %s", probe)
		}
	}
}

func TestMigrationV118ReinstallsRevTriggersWithCarrierProbe(t *testing.T) {
	db := migrateThrough(t, 117)
	thread := 0
	for _, probe := range revTriggerCarrierProbes(t, db) {
		if !strings.Contains(probe, carrierProbeByValue) {
			thread++
		}
	}
	if thread == 0 {
		t.Fatal("the v100 trigger generation already keys its carrier probe on the transcript root")
	}
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES('t','p','t','claude','/p',1,1)`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,tool_name,meta,created_at,updated_at)
 VALUES('launch','t',0,0,'tool_call','assistant','completed','launch','Agent','{}',1,1),
       ('carrier','t',0,1,'tool_call','assistant','completed','resume','Agent','{"transcript_root_id":"launch"}',1,1),
       ('other','t',0,2,'assistant_text','assistant','completed','other','','{}',1,1)`)

	migrateFrom(t, db, 117)
	for _, probe := range revTriggerCarrierProbes(t, db) {
		if !strings.Contains(probe, carrierProbeByValue) {
			t.Errorf("v118 left a carrier probe keyed on the thread alone: %s", probe)
		}
	}
	// The reinstalled insert trigger still stamps the parent's carrier and
	// leaves an unrelated row alone.
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,parent_id,meta,created_at,updated_at)
 VALUES('child','t',0,3,'assistant_text','assistant','completed','child','launch','{}',1,1)`)
	var rev, carrier, launch, other int64
	if err := db.QueryRow(`SELECT history_rev,
 (SELECT rev FROM items WHERE thread_id='t' AND id='carrier'),
 (SELECT rev FROM items WHERE thread_id='t' AND id='launch'),
 (SELECT rev FROM items WHERE thread_id='t' AND id='other')
 FROM threads WHERE id='t'`).Scan(&rev, &carrier, &launch, &other); err != nil {
		t.Fatal(err)
	}
	if carrier != rev || launch != rev || other == rev {
		t.Fatalf("after a child insert: history_rev=%d carrier=%d launch=%d other=%d", rev, carrier, launch, other)
	}
}
