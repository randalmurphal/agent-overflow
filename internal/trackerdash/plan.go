package trackerdash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/store"
)

// What a count copies is what the statement it runs reads, as SQLite will
// run it: the wrapped statement (session.wrapped) is planned on an empty
// session of the copy's own schema (store.MirrorExportSchema), and its
// program says which relations it opens and which of their columns it
// reads. The plan depends on the schema and the text alone (the copy has
// no statistics), so it is the plan the count runs on the filled copy:
// a join SQLite drops is not read, the columns of a USING or NATURAL
// join's condition are, and a CTE that shadows a relation's name reads
// what the CTE reads. That one result is what the copy is filled with and
// what How this is counted names; the wrapper's own lookup of each row's
// ticket makes every count read tickets.
//
// The reading rests on the program's form, which the pinned driver fixes:
// a relation is read through a cursor that OpenRead (or ReopenIdx) opens
// on its b-tree, and a column through Column on that cursor, at its place
// in the relation's records. Every relation is one b-tree and no column
// the copy fills only when asked is part of a key (readLayout checks
// both), so no index holds one and no key comparison reads one. A program
// the reader cannot attribute is the widget's error, never a smaller ask.

// asks is what a count of a metric asks the copy for: the relations its
// statement reads and the columns of store.MirrorExportColumns it reads,
// which the copy fills only when asked; or why its plan could not be read.
type asks struct {
	tables, columns []string
	failed          error
}

// fills is a as the export's relations name them (store.ExportMirrorBoard).
func (a asks) fills() []string { return slices.Concat(a.tables, a.columns) }

// layout is how an empty session stores the export: the relation of each
// b-tree by its root page, and each relation's columns by their place in
// its records.
type layout struct {
	roots   map[int64]string
	columns map[string][]string
}

// readLayout reads s's layout from its catalog, which the gate then
// denies. It fails when a relation is stored in more than one b-tree, or
// a column filled only when asked is part of its relation's key: the plan
// reader would not see every read of it (plan.go).
func (s *session) readLayout(ctx context.Context) error {
	l := &layout{roots: map[int64]string{}, columns: map[string][]string{}}
	trees := map[string][]int64{}
	keyed := map[string]string{} // a WITHOUT ROWID relation's key index
	err := s.query(ctx, `SELECT type, name, tbl_name, rootpage FROM main.sqlite_schema WHERE rootpage > 0`, func(scan func(...any) error) error {
		var typ, name, table string
		var root int64
		if err := scan(&typ, &name, &table, &root); err != nil {
			return err
		}
		if !exportTables[table] {
			return fmt.Errorf("the copy's schema holds %s %s, which is none of the export's relations", typ, name)
		}
		if typ == "index" {
			keyed[table] = name
		}
		if !slices.Contains(trees[table], root) {
			trees[table] = append(trees[table], root)
		}
		l.roots[root] = table
		return nil
	})
	if err != nil {
		return fmt.Errorf("read the copy's layout: %w", err)
	}
	for _, table := range store.MirrorExportTables {
		if len(trees[table]) != 1 {
			return fmt.Errorf("relation %s is stored in %d b-trees; a count reads its columns from one", table, len(trees[table]))
		}
		q, arg := `SELECT name, 0 FROM pragma_table_info(?) ORDER BY cid`, table
		if index, ok := keyed[table]; ok {
			q, arg = `SELECT name, key FROM pragma_index_xinfo(?) ORDER BY seqno`, index
		}
		err := s.query(ctx, q, func(scan func(...any) error) error {
			var name string
			var key bool
			if err := scan(&name, &key); err != nil {
				return err
			}
			if key && slices.Contains(store.MirrorExportColumns, table+"."+name) {
				return fmt.Errorf("column %s.%s is part of its relation's key, which a count reads without naming the column", table, name)
			}
			l.columns[table] = append(l.columns[table], name)
			return nil
		}, arg)
		if err != nil {
			return fmt.Errorf("read the layout of %s: %w", table, err)
		}
	}
	s.layout = l
	return nil
}

// query runs q with args on s's connection and calls row for each row.
func (s *session) query(ctx context.Context, q string, row func(scan func(...any) error) error, args ...any) error {
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := row(rows.Scan); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// unattributable is a program the plan reader cannot say the reads of.
func unattributable(why string) *MetricError {
	return &MetricError{Sentence: "This widget's query could not be counted: the app could not tell what it reads of the board (" + why + "). Change its query."}
}

// planAsks is what statement text, the wrapped statement a count runs,
// reads of the copy, from its program on s, an empty session whose layout
// readLayout read.
func (s *session) planAsks(text string) (asks, error) {
	s.take()
	s.setWrapping(true)
	cursors := map[int64]string{}
	used := map[string]map[string]bool{}
	// The plan reads no parameter's value (the copy has no statistics),
	// but the driver binds every one the statement names.
	err := s.query(context.Background(), "EXPLAIN "+text, func(scan func(...any) error) error {
		var addr, p1, p2, p3, p5 int64
		var op string
		var p4, comment any
		if err := scan(&addr, &op, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			return err
		}
		switch op {
		case "OpenRead", "ReopenIdx":
			table, ok := s.layout.roots[p2]
			if !ok || p3 != 0 {
				return unattributable(fmt.Sprintf("%s opens b-tree %d of database %d, which holds none of its relations", op, p2, p3))
			}
			cursors[p1] = table
			if used[table] == nil {
				used[table] = map[string]bool{}
			}
		case "OpenWrite":
			return unattributable("OpenWrite on b-tree " + fmt.Sprint(p2))
		case "OpenDup":
			// A window's buffer duplicates an ephemeral table's cursor; a
			// duplicate of a relation's would read it through another.
			if table, ok := cursors[p2]; ok {
				return unattributable("OpenDup of the cursor on " + table)
			}
		case "Column":
			table, ok := cursors[p1]
			if !ok {
				return nil
			}
			columns := s.layout.columns[table]
			if p2 < 0 || int(p2) >= len(columns) {
				return unattributable(fmt.Sprintf("Column %d of %s, which has %d", p2, table, len(columns)))
			}
			used[table][columns[p2]] = true
		case "RowData":
			if table, ok := cursors[p1]; ok {
				for _, c := range s.layout.columns[table] {
					used[table][c] = true
				}
			}
		}
		return nil
	}, hostParams(0, 0)...)
	s.setWrapping(false)
	if denied := s.take(); err != nil {
		if me, ok := errors.AsType[*MetricError](err); ok {
			return asks{}, me
		}
		return asks{}, sqlError(err, denied)
	}
	var a asks
	for table := range used {
		a.tables = append(a.tables, table)
	}
	slices.Sort(a.tables)
	for _, c := range store.MirrorExportColumns {
		table, column, _ := strings.Cut(c, ".")
		if used[table][column] {
			a.columns = append(a.columns, c)
		}
	}
	return a, nil
}
