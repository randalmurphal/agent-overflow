package store

import (
	"bytes"
	"log"
	"slices"
	"strings"
	"testing"
)

// TestMigrationV146V147RemoveAutomations applies v146 and v147 over a v145
// store holding what the scheduler wrote: an automation with a cursor, a run
// it started, and a manual, an agent and a called run beside it. v146 drops
// the two tables and the scheduler's partial index, maps the automation run
// to a manual run with no source_ref, logs the count and changes nothing
// else; v147 rebuilds work_items with every other declaration restated and
// a source CHECK that refuses 'automation'.
//
// Not parallel: it captures the process-wide logger.
func TestMigrationV146V147RemoveAutomations(t *testing.T) {
	db := migrateThrough(t, removeAutomationsMigrationVersion-1)
	mustExec(t, db, `INSERT INTO automations(id,project_id,workflow_id,workflow_scope,name,enabled,trigger,notes,created_at,updated_at)
		VALUES('auto','p','build','project','Nightly',1,'{"kind":"cron","expr":"0 3 * * *"}','carry on',1,1)`)
	mustExec(t, db, `INSERT INTO automation_cursors(automation_id,source_key,cursor,updated_at) VALUES('auto','jira','42',1)`)
	mustExec(t, db, `INSERT INTO work_items(id,project_id,goal,workflow_id,workflow_scope,state,source,source_ref,created_at) VALUES
		('run-auto','p','Nightly','build','project','done','automation','auto',1),
		('run-manual','p','By hand','build','project','running','manual','',2),
		('run-agent','p','By agent','build','project','done','agent','item/phase/key',3)`)
	mustExec(t, db, `INSERT INTO work_items(id,project_id,goal,workflow_id,workflow_scope,state,source,source_ref,parent_item_id,parent_phase_id,parent_attempt,call_depth,created_at)
		VALUES('run-call','p','Called','build','project','done','call','site','run-manual','plan',1,1,4)`)

	removedTables := []string{"automations", "automation_cursors"}
	removedIndexes := []string{"idx_work_items_automation_source_ref"}
	schemaBefore := migrationSchema(t, db)
	for _, name := range slices.Concat(removedTables, removedIndexes) {
		if _, ok := schemaBefore[name]; !ok {
			t.Fatalf("%s is missing before v146; the fixture does not exercise its drop", name)
		}
	}
	removed := func(name string) bool {
		if slices.Contains(removedIndexes, name) {
			return true
		}
		return slices.ContainsFunc(removedTables, func(table string) bool {
			return strings.Contains(schemaBefore[name], " ON "+table+":")
		})
	}
	columns, err := queryIDs(db, `SELECT name FROM pragma_table_info('work_items') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	renderRows := func() []string {
		t.Helper()
		parts := make([]string, len(columns))
		for i, column := range columns {
			parts[i] = "'" + column + "=' || quote(" + column + ")"
		}
		rows, err := queryIDs(db, `SELECT `+strings.Join(parts, ` || '|' || `)+` FROM work_items ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	// The rows v146 must produce: every value as it was, with the automation
	// run read as a manual run with no source_ref.
	mustExec(t, db, `UPDATE work_items SET source = 'manual', source_ref = '' WHERE id = 'run-auto'`)
	wantRows := renderRows()
	mustExec(t, db, `UPDATE work_items SET source = 'automation', source_ref = 'auto' WHERE id = 'run-auto'`)
	if slices.Equal(renderRows(), wantRows) {
		t.Fatal("restoring the automation run left the rows unchanged; the fixture does not exercise the mapping")
	}
	shapeBefore := readTableShape(t, db, "work_items")

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	migrateFromThrough(t, db, removeAutomationsMigrationVersion-1, removeAutomationsMigrationVersion)
	log.SetOutput(previous)
	if !strings.Contains(logged.String(), "migration v146 mapped 1 automation-started run(s) to manual") {
		t.Errorf("v146 log does not report the mapped count:\n%s", logged.String())
	}
	if got := renderRows(); !slices.Equal(got, wantRows) {
		t.Errorf("work_items rows after v146:\n got %q\nwant %q", got, wantRows)
	}
	schemaAfter := migrationSchema(t, db)
	for name, sqlText := range schemaBefore {
		if removed(name) {
			if _, ok := schemaAfter[name]; ok {
				t.Errorf("%s survived v146", name)
			}
			continue
		}
		if schemaAfter[name] != sqlText {
			t.Errorf("schema object %s changed in v146:\n got %q\nwant %q", name, schemaAfter[name], sqlText)
		}
	}
	for name := range schemaAfter {
		if _, ok := schemaBefore[name]; !ok {
			t.Errorf("schema object %s is new after v146", name)
		}
	}
	// v146 only drops: 'automation' is still accepted until v147 rebuilds.
	if got := readTableShape(t, db, "work_items"); !slices.Equal(got.checks, shapeBefore.checks) {
		t.Errorf("work_items CHECKs changed in v146: %q", got.checks)
	}

	schemaBefore147 := migrationSchema(t, db)
	migrateFromThrough(t, db, removeAutomationsMigrationVersion, workItemSourcesMigrationVersion)

	if got := renderRows(); !slices.Equal(got, wantRows) {
		t.Errorf("work_items rows after v147:\n got %q\nwant %q", got, wantRows)
	}
	// Every object other than work_items keeps its SQL, and each work_items
	// index is restated with the same declaration.
	schemaAfter147 := migrationSchema(t, db)
	normalized := func(sqlText string) string { return strings.Join(strings.Fields(sqlText), " ") }
	for name, sqlText := range schemaBefore147 {
		if name == "work_items" {
			continue
		}
		if normalized(schemaAfter147[name]) != normalized(sqlText) {
			t.Errorf("schema object %s changed in v147:\n got %q\nwant %q", name, schemaAfter147[name], sqlText)
		}
	}
	for name := range schemaAfter147 {
		if _, ok := schemaBefore147[name]; !ok {
			t.Errorf("schema object %s is new after v147", name)
		}
	}

	// The rebuilt table restates v146's columns and foreign keys, and its
	// CHECKs less 'automation' in the source list.
	shapeAfter := readTableShape(t, db, "work_items")
	if !slices.Equal(shapeAfter.columns, shapeBefore.columns) {
		t.Errorf("work_items columns after v147:\n got %+v\nwant %+v", shapeAfter.columns, shapeBefore.columns)
	}
	if !slices.Equal(shapeAfter.fks, shapeBefore.fks) {
		t.Errorf("work_items foreign keys after v147 = %q, want %q", shapeAfter.fks, shapeBefore.fks)
	}
	wantChecks := make([]string, 0, len(shapeBefore.checks))
	for _, check := range shapeBefore.checks {
		wantChecks = append(wantChecks, strings.ReplaceAll(check, ",'automation'", ""))
	}
	slices.Sort(wantChecks)
	if slices.Equal(wantChecks, shapeBefore.checks) {
		t.Fatalf("work_items CHECKs before v147 never name 'automation'; the fixture does not exercise the change: %q", shapeBefore.checks)
	}
	if !slices.Equal(shapeAfter.checks, wantChecks) {
		t.Errorf("work_items CHECKs after v147:\n got %q\nwant %q", shapeAfter.checks, wantChecks)
	}

	if _, err := db.Exec(`UPDATE work_items SET source = 'automation' WHERE id = 'run-auto'`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("work_items.source = 'automation' after v147: err = %v, want a CHECK violation", err)
	}
	mustExec(t, db, `UPDATE work_items SET source = 'agent', source_ref = 'item/phase/other' WHERE id = 'run-auto'`)
	if _, err := db.Exec(`UPDATE work_items SET source_ref = 'item/phase/key' WHERE id = 'run-auto'`); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Errorf("a duplicate agent source_ref after v147: err = %v, want the unique index to refuse it", err)
	}
	if _, err := db.Exec(`UPDATE work_items SET parent_phase_id = '' WHERE id = 'run-call'`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Errorf("a half-linked call after v147: err = %v, want the linkage CHECK to refuse it", err)
	}
}
