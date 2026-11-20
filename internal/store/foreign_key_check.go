package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

// assertForeignKeysIntact fails when PRAGMA foreign_key_check would report
// any row of the main schema, with the first row the pragma reports for
// the failing table. The pragma reads every row of every child table from
// the table, which on a long history is most of the file; each probe here
// reads the child's key columns from an index where one covers them
// (TestForeignKeyCheckReadsLargeTablesFromIndexes).
//
// It checks the whole database rather than the tables a migration
// touched: a rebuild's ordinary statements run with enforcement off and
// can strand rows of any table, and SQLite reports no such set.
//
// Soundness. The probes cover the foreign keys the pragma checks: every
// one pragma_foreign_key_list reports for a table of main.sqlite_schema
// (the unqualified pragma checks main only). EXPLAIN compiles the pragma
// without reading a row, and compiling locates every parent key, so a
// foreign key mismatch fails here as it fails the pragma. Each probe is the
// pragma's test of a row in SQL: a row with a NULL key column passes; with
// no parent table every other row fails; otherwise a row fails when no
// parent row has parent column = +child column on every key column. The
// unary plus gives the child value no affinity, so SQLite applies the
// parent column's affinity to it, as the pragma applies its parent index's
// (numeric affinities compare alike, and an INTEGER PRIMARY KEY compares
// as the pragma's rowid seek does). The parent column on the left supplies
// its collation, which every index the pragma may use for named parent
// columns shares; a key that names no parent columns uses the primary key
// index, whose collation the probe states because it may differ from the
// column's. The pragma pairs child and parent columns as the probe does
// unless the parent has an index that lists a column twice or the foreign
// key names a parent column twice; such a table runs the pragma itself.
// Probes only decide which tables the pragma runs on: a probe that finds a
// row runs the pragma on its table, whose report decides.
func assertForeignKeysIntact(ctx context.Context, tx *sql.Tx) error {
	if err := compileForeignKeyCheck(ctx, tx); err != nil {
		return err
	}
	probes, err := foreignKeyProbes(ctx, tx)
	if err != nil {
		return err
	}
	checked := map[string]bool{}
	for _, probe := range probes {
		if checked[probe.table] {
			continue
		}
		if probe.query != "" {
			found, err := foreignKeyProbeFinds(ctx, tx, probe.query)
			if err != nil {
				return fmt.Errorf("foreign key check of %q (foreign key %d): %w", probe.table, probe.id, err)
			}
			if !found {
				continue
			}
		}
		checked[probe.table] = true
		if err := reportForeignKeyViolation(ctx, tx, probe.table); err != nil {
			return err
		}
		if probe.query != "" {
			log.Printf("store: WARNING foreign key probe of %q (foreign key %d to %q) found a row foreign_key_check does not report", probe.table, probe.id, probe.parent)
		}
	}
	return nil
}

// compileForeignKeyCheck compiles the whole-database pragma without running
// it, so a foreign key mismatch anywhere fails as the pragma does.
func compileForeignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "EXPLAIN PRAGMA main.foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	for rows.Next() {
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	return nil
}

// foreignKeyProbeFinds runs probe, prepared so the one-off text stays out
// of the connection's statement cache.
func foreignKeyProbeFinds(ctx context.Context, tx *sql.Tx, probe string) (bool, error) {
	stmt, err := tx.PrepareContext(ctx, probe)
	if err != nil {
		return false, err
	}
	var one int
	err = stmt.QueryRowContext(ctx).Scan(&one)
	closeErr := stmt.Close()
	if errors.Is(err, sql.ErrNoRows) {
		return false, closeErr
	}
	return err == nil, errors.Join(err, closeErr)
}

// reportForeignKeyViolation runs the pragma on table and returns its first
// row as the migration's failure, or nil when it reports none.
func reportForeignKeyViolation(ctx context.Context, tx *sql.Tx, table string) (err error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA main.foreign_key_check("+quoteSQLIdent(table)+")")
	if err != nil {
		return fmt.Errorf("foreign_key_check(%q): %w", table, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("foreign_key_check(%q): close: %w", table, closeErr))
		}
	}()
	if rows.Next() {
		// Columns: child table, child rowid, referenced table, fk index.
		var child, referred sql.NullString
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&child, &rowid, &referred, &fkid); err != nil {
			return fmt.Errorf("foreign_key_check scan: %w", err)
		}
		return fmt.Errorf("foreign key violation after rebuild: table %q row %d references missing %q (foreign key %d)",
			child.String, rowid.Int64, referred.String, fkid.Int64)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign_key_check(%q): %w", table, err)
	}
	return nil
}

// foreignKeyProbe is one foreign key of a main-schema table. An empty query
// means the table's keys run through the pragma itself.
type foreignKeyProbe struct {
	table  string
	id     int
	parent string
	query  string
}

type foreignKeyDecl struct {
	table  string
	id     int
	parent string
	from   []string
	to     []sql.NullString
}

// foreignKeyProbes returns a probe for every foreign key in the main schema,
// one pragma probe in place of all of a table's keys when any of them
// cannot be written as SQL.
func foreignKeyProbes(ctx context.Context, tx *sql.Tx) ([]foreignKeyProbe, error) {
	decls, err := foreignKeyDecls(ctx, tx)
	if err != nil {
		return nil, err
	}
	parents := map[string]*foreignKeyParent{}
	var probes []foreignKeyProbe
	pragmaTables := map[string]bool{}
	for _, decl := range decls {
		if pragmaTables[decl.table] {
			continue
		}
		key := foldSQLiteName(decl.parent)
		parent, ok := parents[key]
		if !ok {
			parent, err = readForeignKeyParent(ctx, tx, decl.parent)
			if err != nil {
				return nil, err
			}
			parents[key] = parent
		}
		query, ok := foreignKeyProbeSQL(decl, parent)
		if !ok {
			pragmaTables[decl.table] = true
			probes = append(probes, foreignKeyProbe{table: decl.table, id: decl.id, parent: decl.parent})
			continue
		}
		probes = append(probes, foreignKeyProbe{table: decl.table, id: decl.id, parent: decl.parent, query: query})
	}
	// A table that runs the pragma needs no probes of its own.
	kept := probes[:0]
	for _, probe := range probes {
		if probe.query != "" && pragmaTables[probe.table] {
			continue
		}
		kept = append(kept, probe)
	}
	return kept, nil
}

func foreignKeyDecls(ctx context.Context, tx *sql.Tx) (_ []foreignKeyDecl, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.name, f.id, f.seq, f."table", f."from", f."to"
		FROM main.sqlite_schema AS m, pragma_foreign_key_list(m.name, 'main') AS f
		WHERE m.type = 'table'
		ORDER BY m.name, f.id, f.seq`)
	if err != nil {
		return nil, fmt.Errorf("list foreign keys: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("list foreign keys: close: %w", closeErr))
		}
	}()
	var decls []foreignKeyDecl
	for rows.Next() {
		var table, parent, from string
		var id, seq int
		var to sql.NullString
		if err := rows.Scan(&table, &id, &seq, &parent, &from, &to); err != nil {
			return nil, fmt.Errorf("scan foreign key: %w", err)
		}
		if n := len(decls); n == 0 || decls[n-1].table != table || decls[n-1].id != id {
			decls = append(decls, foreignKeyDecl{table: table, id: id, parent: parent})
		}
		last := &decls[len(decls)-1]
		if seq != len(last.from) {
			return nil, fmt.Errorf("foreign key %d of %q: column %d listed at %d", id, table, seq, len(last.from))
		}
		last.from = append(last.from, from)
		last.to = append(last.to, to)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list foreign keys: %w", err)
	}
	return decls, nil
}

// foreignKeyParent is what the probes need of a parent table.
type foreignKeyParent struct {
	// name is the table's own name; empty when main has no such table.
	name string
	// unresolved is set when the name may resolve to a table
	// sqlite_schema does not list, such as sqlite_schema itself.
	unresolved bool
	// rowidAlias is the INTEGER PRIMARY KEY column, if any.
	rowidAlias string
	// pkColumns and pkCollations are the primary key index's key columns
	// in order, when the table has such an index.
	pkColumns    []string
	pkCollations []string
	// repeatsColumn is set when an index lists a column more than once.
	repeatsColumn bool
}

func readForeignKeyParent(ctx context.Context, tx *sql.Tx, name string) (*foreignKeyParent, error) {
	parent := &foreignKeyParent{}
	err := tx.QueryRowContext(ctx,
		`SELECT name FROM main.sqlite_schema WHERE type IN ('table', 'view') AND name = ? COLLATE NOCASE`,
		name).Scan(&parent.name)
	if errors.Is(err, sql.ErrNoRows) {
		parent.unresolved = strings.HasPrefix(foldSQLiteName(name), "sqlite_")
		return parent, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find foreign key parent %q: %w", name, err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_xinfo(?, 'main') WHERE pk > 0`, parent.name)
	if err != nil {
		return nil, fmt.Errorf("read primary key of %q: %w", parent.name, err)
	}
	var pk []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, errors.Join(fmt.Errorf("scan primary key of %q: %w", parent.name, err), rows.Close())
		}
		pk = append(pk, column)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read primary key of %q: %w", parent.name, err)
	}

	rows, err = tx.QueryContext(ctx, `
		SELECT il.name, il.origin, x.name, x.coll
		FROM pragma_index_list(?1, 'main') AS il, pragma_index_xinfo(il.name, 'main') AS x
		WHERE x.key
		ORDER BY il.name, x.seqno`, parent.name)
	if err != nil {
		return nil, fmt.Errorf("read indexes of %q: %w", parent.name, err)
	}
	hasPKIndex := false
	seen := map[string]bool{}
	lastIndex := ""
	for rows.Next() {
		var index, origin, coll string
		var column sql.NullString
		if err := rows.Scan(&index, &origin, &column, &coll); err != nil {
			return nil, errors.Join(fmt.Errorf("scan indexes of %q: %w", parent.name, err), rows.Close())
		}
		if index != lastIndex {
			lastIndex = index
			clear(seen)
		}
		if column.Valid {
			folded := foldSQLiteName(column.String)
			if seen[folded] {
				parent.repeatsColumn = true
			}
			seen[folded] = true
		}
		if origin != "pk" {
			continue
		}
		hasPKIndex = true
		if !column.Valid {
			// No usable primary key mapping; the pragma decides.
			parent.repeatsColumn = true
			continue
		}
		parent.pkColumns = append(parent.pkColumns, column.String)
		parent.pkCollations = append(parent.pkCollations, coll)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read indexes of %q: %w", parent.name, err)
	}
	// A primary key with no index of its own is the rowid alias.
	if !hasPKIndex && len(pk) == 1 {
		parent.rowidAlias = pk[0]
	}
	return parent, nil
}

// foreignKeyProbeSQL is a query that returns a row exactly when the pragma
// would report a row of decl, or false when only the pragma can decide.
func foreignKeyProbeSQL(decl foreignKeyDecl, parent *foreignKeyParent) (string, bool) {
	if parent.unresolved {
		return "", false
	}
	var b strings.Builder
	b.WriteString("SELECT 1 FROM main.")
	b.WriteString(quoteSQLIdent(decl.table))
	b.WriteString(" AS c WHERE ")
	for i, from := range decl.from {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.WriteString("c." + quoteSQLIdent(from) + " IS NOT NULL")
	}
	if parent.name == "" {
		b.WriteString(" LIMIT 1")
		return b.String(), true
	}

	columns, collations, ok := foreignKeyParentColumns(decl, parent)
	if !ok {
		return "", false
	}
	b.WriteString(" AND NOT EXISTS (SELECT 1 FROM main.")
	b.WriteString(quoteSQLIdent(parent.name))
	b.WriteString(" AS p WHERE ")
	for i, column := range columns {
		if i > 0 {
			b.WriteString(" AND ")
		}
		b.WriteString("p." + quoteSQLIdent(column) + " = +c." + quoteSQLIdent(decl.from[i]))
		if collations[i] != "" {
			b.WriteString(" COLLATE " + quoteSQLIdent(collations[i]))
		}
	}
	b.WriteString(") LIMIT 1")
	return b.String(), true
}

// foreignKeyParentColumns maps decl's child columns, in order, to the parent
// columns the pragma compares them with, and the collation to state for
// each (empty: the parent column's own).
func foreignKeyParentColumns(decl foreignKeyDecl, parent *foreignKeyParent) ([]string, []string, bool) {
	if parent.repeatsColumn {
		return nil, nil, false
	}
	named := 0
	for _, to := range decl.to {
		if to.Valid {
			named++
		}
	}
	switch named {
	case 0:
		if len(decl.from) == 1 && parent.rowidAlias != "" {
			return []string{parent.rowidAlias}, []string{""}, true
		}
		if len(parent.pkColumns) != len(decl.from) {
			return nil, nil, false
		}
		return parent.pkColumns, parent.pkCollations, true
	case len(decl.to):
		columns := make([]string, len(decl.to))
		seen := map[string]bool{}
		for i, to := range decl.to {
			folded := foldSQLiteName(to.String)
			if seen[folded] {
				return nil, nil, false
			}
			seen[folded] = true
			columns[i] = to.String
		}
		return columns, make([]string, len(columns)), true
	}
	return nil, nil, false
}

// quoteSQLIdent quotes name as an SQL identifier.
func quoteSQLIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// foldSQLiteName folds a table or column name as SQLite compares names:
// ASCII letters only. Names that differ in the case of any other letter are
// different tables or columns.
func foldSQLiteName(name string) string {
	folded := []byte(name)
	for i, c := range folded {
		if 'A' <= c && c <= 'Z' {
			folded[i] = c + ('a' - 'A')
		}
	}
	return string(folded)
}
