package store

import (
	"database/sql"
	"strings"
	"testing"
)

// Inspect the SQL the production readers execute, including nested hydrators.
type lookupPlanReader struct {
	sqlQueryer
	t                *testing.T
	db               *sql.DB
	sawID, sawParent bool
}

func (p *lookupPlanReader) Query(query string, args ...any) (*sql.Rows, error) {
	plan, err := p.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		p.t.Fatal(err)
	}
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			p.t.Fatal(err)
		}
		if strings.Contains(detail, "SEARCH refs") && !strings.Contains(detail, "chunk_id=?") && (strings.Contains(query, "WITH selected") || strings.Contains(query, "WITH RECURSIVE rel")) {
			// Bounded selection may enumerate chunks once. Hydration and recursive
			// hops must probe a known chunk instead of repeating that enumeration.
			if strings.Contains(query, "WITH RECURSIVE rel") || !strings.Contains(query, "ORDER BY turn_index") {
				p.t.Errorf("chunk fan-out: %s", detail)
			}
		}
		if strings.Contains(detail, "idx_import_history_items_id") {
			p.sawID = true
		}
		if strings.Contains(detail, "idx_import_history_items_parent_lookup") {
			p.sawParent = true
		}
	}
	if err := plan.Err(); err != nil {
		p.t.Fatal(err)
	}
	if err := plan.Close(); err != nil {
		p.t.Fatal(err)
	}
	return p.sqlQueryer.Query(query, args...)
}
func TestImportedLookupProbesIdentityBeforeChunkMembership(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	q := &lookupPlanReader{sqlQueryer: s.reader(), t: t, db: s.reader()}
	items, err := queryHydratedTimelineItems(q, timelineParityThreadID, "SELECT 'imp-child-0' AS id UNION ALL SELECT 'loc-child-2'")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("hydrated %d rows", len(items))
	}
	scan, err := queryActivityScanRows(q, timelineParityThreadID, 2, "SELECT 'imp-child-0' AS id UNION ALL SELECT 'loc-child-2'")
	if err != nil {
		t.Fatal(err)
	}
	if len(scan) != 2 {
		t.Fatalf("scanned %d rows", len(scan))
	}
	aggregates, err := s.subagentAggregatesByRoot(q, timelineParityThreadID, []string{"imp-launch-0", "loc-launch-2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"imp-launch-0", "loc-launch-2"} {
		if aggregates[id].descendantCount != 2 {
			t.Fatalf("%s: %+v", id, aggregates[id])
		}
	}
	if !q.sawID || !q.sawParent {
		t.Fatalf("missing lookup indexes: id=%v parent=%v", q.sawID, q.sawParent)
	}
}
func TestMigrationV115ImportedParentLookup(t *testing.T) {
	db := migrateThrough(t, 114)
	migrateFrom(t, db, 114)
	assertPlanUses(t, db, "idx_import_history_items_parent_lookup", `EXPLAIN QUERY PLAN SELECT chunk_id FROM import_history_items WHERE parent_id<>'' AND parent_id=?`, "root")
	assertPlanUses(t, db, "idx_import_history_items_parent", `EXPLAIN QUERY PLAN SELECT id FROM import_history_items WHERE parent_id<>'' AND parent_id=? AND chunk_id=? ORDER BY turn_index,item_index LIMIT 30`, "root", "chunk")
}
