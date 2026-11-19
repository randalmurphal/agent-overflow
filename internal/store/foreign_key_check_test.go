package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fkRef names one foreign key as PRAGMA foreign_key_check numbers it.
type fkRef struct {
	table string
	id    int
}

// pragmaForeignKeyViolations is the set of foreign keys the whole-database
// pragma reports a row for.
func pragmaForeignKeyViolations(t *testing.T, tx *sql.Tx) map[fkRef]bool {
	t.Helper()
	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	found := map[fkRef]bool{}
	for rows.Next() {
		var table, parent sql.NullString
		var rowid, id sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &id); err != nil {
			t.Fatal(err)
		}
		found[fkRef{table.String, int(id.Int64)}] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

// requireForeignKeyCheckMatchesPragma fails unless every probe finds a row
// exactly when the pragma reports its foreign key, every reported key has
// a probe, and assertForeignKeysIntact fails exactly when the pragma
// reports anything, naming a table it reports. It returns the pragma's set.
func requireForeignKeyCheckMatchesPragma(t *testing.T, tx *sql.Tx) map[fkRef]bool {
	t.Helper()
	want := pragmaForeignKeyViolations(t, tx)
	probes, err := foreignKeyProbes(t.Context(), tx)
	if err != nil {
		t.Fatalf("foreignKeyProbes: %v", err)
	}
	probed := map[fkRef]bool{}
	pragmaTables := map[string]bool{}
	for _, probe := range probes {
		if probe.query == "" {
			pragmaTables[probe.table] = true
			continue
		}
		key := fkRef{probe.table, probe.id}
		probed[key] = true
		found, err := foreignKeyProbeFinds(t.Context(), tx, probe.query)
		if err != nil {
			t.Fatalf("probe %v: %v\n%s", key, err, probe.query)
		}
		if found != want[key] {
			t.Errorf("probe of %s foreign key %d finds a row = %v, foreign_key_check reports one = %v\n%s",
				probe.table, probe.id, found, want[key], probe.query)
		}
	}
	for key := range want {
		if !probed[key] && !pragmaTables[key.table] {
			t.Errorf("foreign_key_check reports %s foreign key %d, which no probe covers", key.table, key.id)
		}
	}
	checkErr := assertForeignKeysIntact(t.Context(), tx)
	switch {
	case len(want) == 0 && checkErr != nil:
		t.Errorf("assertForeignKeysIntact = %v, foreign_key_check reports nothing", checkErr)
	case len(want) > 0 && checkErr == nil:
		t.Errorf("assertForeignKeysIntact passed, foreign_key_check reports %v", want)
	case len(want) > 0:
		named := false
		for key := range want {
			named = named || strings.Contains(checkErr.Error(), fmt.Sprintf("table %q", key.table))
		}
		if !named {
			t.Errorf("assertForeignKeysIntact = %v, want it to name a table of %v", checkErr, want)
		}
	}
	return want
}

// schemaForeignKey is a foreign key of the migrated schema, read from the
// schema rather than from the code under test.
type schemaForeignKey struct {
	fkRef
	from []string
}

func schemaForeignKeys(t *testing.T, db *sql.DB) []schemaForeignKey {
	t.Helper()
	rows, err := db.Query(`
		SELECT m.name, f.id, f."from"
		FROM sqlite_schema AS m, pragma_foreign_key_list(m.name) AS f
		WHERE m.type = 'table'
		ORDER BY m.name, f.id, f.seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []schemaForeignKey
	for rows.Next() {
		var table, from string
		var id int
		if err := rows.Scan(&table, &id, &from); err != nil {
			t.Fatal(err)
		}
		if n := len(keys); n == 0 || keys[n-1].fkRef != (fkRef{table, id}) {
			keys = append(keys, schemaForeignKey{fkRef: fkRef{table, id}})
		}
		keys[len(keys)-1].from = append(keys[len(keys)-1].from, from)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return keys
}

type schemaColumn struct {
	name, declType string
	notNull        bool
	hasDefault     bool
}

func schemaColumns(t *testing.T, tx *sql.Tx, table string) []schemaColumn {
	t.Helper()
	rows, err := tx.Query(`SELECT name, type, "notnull", dflt_value IS NOT NULL FROM pragma_table_xinfo(?) WHERE hidden = 0 ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []schemaColumn
	for rows.Next() {
		var c schemaColumn
		if err := rows.Scan(&c.name, &c.declType, &c.notNull, &c.hasDefault); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

// insertForeignKeyRow inserts a row of table whose columns in missing hold
// a value no parent has and whose columns in null are NULL. Every other
// foreign key column is NULL where it may be, and every other NOT NULL
// column without a default gets a value unique to the row.
func insertForeignKeyRow(t *testing.T, tx *sql.Tx, table string, foreignKeyColumns, missing, null []string, serial int) {
	t.Helper()
	var names, values []string
	var args []any
	for _, column := range schemaColumns(t, tx, table) {
		var value any
		switch {
		case slices.Contains(null, column.name):
			value = nil
		case slices.Contains(missing, column.name):
			value = fmt.Sprintf("fk-missing-%d", serial)
		case slices.Contains(foreignKeyColumns, column.name) && !column.notNull:
			value = nil
		case column.notNull && !column.hasDefault || slices.Contains(foreignKeyColumns, column.name):
			if strings.Contains(strings.ToUpper(column.declType), "INT") {
				value = 1_000_000 + serial
			} else {
				value = fmt.Sprintf("fk-row-%d", serial)
			}
		default:
			continue
		}
		names = append(names, quoteSQLIdent(column.name))
		values = append(values, "?")
		args = append(args, value)
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quoteSQLIdent(table), strings.Join(names, ", "), strings.Join(values, ", "))
	if _, err := tx.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// TestForeignKeyCheckMatchesPragmaOnSchema plants a row of every foreign
// key of the migrated schema, read from the schema so a key added later is
// covered, and requires the check to agree with foreign_key_check: a
// missing parent fails both, a NULL in any key column passes both.
func TestForeignKeyCheckMatchesPragmaOnSchema(t *testing.T) {
	db := migratedDB(t)
	triggers, err := queryIDs(db, `SELECT name FROM sqlite_schema WHERE type = 'trigger'`)
	if err != nil {
		t.Fatal(err)
	}
	// Triggers do not take part in foreign_key_check; dropping them lets a
	// bare row land in any table. CHECK constraints are ignored for the
	// same reason. NOT NULL still holds.
	for _, trigger := range triggers {
		mustExec(t, db, "DROP TRIGGER "+quoteSQLIdent(trigger))
	}
	mustExec(t, db, `PRAGMA foreign_keys = OFF`)
	mustExec(t, db, `PRAGMA ignore_check_constraints = ON`)

	keys := schemaForeignKeys(t, db)
	if len(keys) < 50 {
		t.Fatalf("the migrated schema lists %d foreign keys, want the full schema", len(keys))
	}
	foreignKeyColumns := map[string][]string{}
	for _, key := range keys {
		foreignKeyColumns[key.table] = append(foreignKeyColumns[key.table], key.from...)
	}

	run := func(t *testing.T, plant func(tx *sql.Tx)) map[fkRef]bool {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
		}()
		plant(tx)
		return requireForeignKeyCheckMatchesPragma(t, tx)
	}

	if got := run(t, func(*sql.Tx) {}); len(got) != 0 {
		t.Fatalf("the migrated schema reports %v before any row is planted", got)
	}
	serial := 0
	for _, key := range keys {
		t.Run(fmt.Sprintf("%s_%d", key.table, key.id), func(t *testing.T) {
			serial++
			got := run(t, func(tx *sql.Tx) {
				insertForeignKeyRow(t, tx, key.table, foreignKeyColumns[key.table], key.from, nil, serial)
			})
			if !got[key.fkRef] {
				t.Fatalf("a row with no parent did not make foreign_key_check report %s foreign key %d: %v", key.table, key.id, got)
			}
			nullable := map[string]bool{}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			for _, column := range schemaColumns(t, tx, key.table) {
				nullable[column.name] = !column.notNull
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			for _, column := range key.from {
				if !nullable[column] {
					continue
				}
				serial++
				got := run(t, func(tx *sql.Tx) {
					insertForeignKeyRow(t, tx, key.table, foreignKeyColumns[key.table], key.from, []string{column}, serial)
				})
				if got[key.fkRef] {
					t.Errorf("a row with NULL %s made foreign_key_check report %s foreign key %d", column, key.table, key.id)
				}
			}
		})
	}
}

// adHocForeignKeySchema covers the key shapes the store schema lacks: a
// rowid alias parent, WITHOUT ROWID parents, a primary key whose index
// collation differs from its column's, named columns in another order than
// the parent's index, affinity and collation of the comparison, a missing
// parent, a parent only a temp table names, and identifiers that need
// quoting.
const adHocForeignKeySchema = `
CREATE TABLE p_alias(id INTEGER PRIMARY KEY, label TEXT);
CREATE TABLE p_wr(a TEXT, b INTEGER, PRIMARY KEY(a, b)) WITHOUT ROWID;
CREATE TABLE p_nocase(k TEXT, PRIMARY KEY(k COLLATE NOCASE)) WITHOUT ROWID;
CREATE TABLE p_text(k TEXT PRIMARY KEY);
CREATE TABLE p_coll(k TEXT COLLATE NOCASE UNIQUE);
CREATE TABLE p_comp(x TEXT, y TEXT, UNIQUE(y, x));
CREATE TABLE p_dup(x, y);
CREATE UNIQUE INDEX p_dup_xx ON p_dup(x, x);
CREATE TABLE c_alias_named(r INTEGER REFERENCES p_alias(id));
CREATE TABLE c_alias_implicit(r REFERENCES p_alias);
CREATE TABLE c_wr(a TEXT, b, FOREIGN KEY(a, b) REFERENCES p_wr);
CREATE TABLE c_wr_named(id INTEGER PRIMARY KEY, b2 INTEGER, a2 TEXT, FOREIGN KEY(b2, a2) REFERENCES p_wr(b, a)) WITHOUT ROWID;
CREATE TABLE c_nocase(k REFERENCES p_nocase);
CREATE TABLE c_text(k INTEGER REFERENCES p_text(k));
CREATE TABLE c_coll(k TEXT REFERENCES p_coll(k));
CREATE TABLE c_comp(x TEXT, y TEXT, FOREIGN KEY(x, y) REFERENCES p_comp(x, y));
CREATE TABLE c_missing(m TEXT, n TEXT, FOREIGN KEY(m, n) REFERENCES p_gone(a, b));
CREATE TABLE c_case(k TEXT REFERENCES P_TEXT(K));
CREATE TABLE c_shadow(k TEXT REFERENCES p_temp_only(k));
CREATE TABLE "c q""uote"("we""ird" TEXT REFERENCES p_text(k));
CREATE TABLE c_dup(a, b, FOREIGN KEY(a, b) REFERENCES p_dup(x, y));
CREATE TEMP TABLE p_temp_only(k TEXT PRIMARY KEY);
INSERT INTO p_alias VALUES (5, 'five');
INSERT INTO p_wr VALUES ('a', 1);
INSERT INTO p_nocase VALUES ('ABC');
INSERT INTO p_text VALUES ('05'), ('7');
INSERT INTO p_coll VALUES ('Mixed');
INSERT INTO p_comp VALUES ('x1', 'y1');
INSERT INTO p_dup VALUES (1, 2);
INSERT INTO p_temp_only VALUES ('t');
`

func adHocForeignKeyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(filepath.Join(t.TempDir(), "fk.db"), writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	mustExec(t, db, `PRAGMA foreign_keys = OFF`)
	mustExec(t, db, adHocForeignKeySchema)
	return db
}

// TestForeignKeyCheckMatchesPragmaOnAdHocKeys plants rows the pragma
// passes and fails in each ad hoc shape and requires the same verdict.
func TestForeignKeyCheckMatchesPragmaOnAdHocKeys(t *testing.T) {
	db := adHocForeignKeyDB(t)
	const (
		clean = iota
		violates
		either // the pragma decides; the case pins agreement only
	)
	cases := []struct {
		name  string
		rows  string
		table string
		want  int
	}{
		{"alias named", `INSERT INTO c_alias_named VALUES (5), ('5'), (NULL)`, "c_alias_named", clean},
		{"alias named missing", `INSERT INTO c_alias_named VALUES (6)`, "c_alias_named", violates},
		{"alias named real", `INSERT INTO c_alias_named VALUES (5.5)`, "c_alias_named", violates},
		{"alias implicit", `INSERT INTO c_alias_implicit VALUES (5), (5.0)`, "c_alias_implicit", clean},
		{"alias implicit text", `INSERT INTO c_alias_implicit VALUES ('5')`, "c_alias_implicit", clean},
		{"alias implicit spaced", `INSERT INTO c_alias_implicit VALUES (' 5 ')`, "c_alias_implicit", either},
		{"alias implicit real text", `INSERT INTO c_alias_implicit VALUES ('5.0')`, "c_alias_implicit", either},
		{"alias implicit hex", `INSERT INTO c_alias_implicit VALUES ('0x5')`, "c_alias_implicit", violates},
		{"alias implicit blob", `INSERT INTO c_alias_implicit VALUES (x'05')`, "c_alias_implicit", violates},
		{"alias implicit word", `INSERT INTO c_alias_implicit VALUES ('abc')`, "c_alias_implicit", violates},
		{"without rowid implicit", `INSERT INTO c_wr VALUES ('a', 1), ('a', '1'), ('a', NULL), (NULL, 9)`, "c_wr", clean},
		{"without rowid implicit case", `INSERT INTO c_wr VALUES ('A', 1)`, "c_wr", violates},
		{"without rowid implicit second column", `INSERT INTO c_wr VALUES ('a', 2)`, "c_wr", violates},
		{"without rowid named", `INSERT INTO c_wr_named VALUES (1, 1, 'a'), (2, 3, NULL)`, "c_wr_named", clean},
		{"without rowid named missing", `INSERT INTO c_wr_named VALUES (3, 2, 'a')`, "c_wr_named", violates},
		{"primary key collation", `INSERT INTO c_nocase VALUES ('abc')`, "c_nocase", clean},
		{"primary key collation missing", `INSERT INTO c_nocase VALUES ('abd')`, "c_nocase", violates},
		{"parent affinity", `INSERT INTO c_text VALUES (7), ('7')`, "c_text", clean},
		{"parent affinity no numeric match", `INSERT INTO c_text VALUES (5)`, "c_text", violates},
		{"parent column collation", `INSERT INTO c_coll VALUES ('mixed')`, "c_coll", clean},
		{"parent column collation missing", `INSERT INTO c_coll VALUES ('other')`, "c_coll", violates},
		{"composite", `INSERT INTO c_comp VALUES ('x1', 'y1'), ('x1', NULL), (NULL, 'nope')`, "c_comp", clean},
		{"composite second column", `INSERT INTO c_comp VALUES ('x1', 'y2')`, "c_comp", violates},
		{"composite swapped", `INSERT INTO c_comp VALUES ('y1', 'x1')`, "c_comp", violates},
		{"missing parent", `INSERT INTO c_missing VALUES ('a', 'b')`, "c_missing", violates},
		{"missing parent null", `INSERT INTO c_missing VALUES ('a', NULL), (NULL, 'b'), (NULL, NULL)`, "c_missing", clean},
		{"parent name case", `INSERT INTO c_case VALUES ('7')`, "c_case", clean},
		{"parent name case missing", `INSERT INTO c_case VALUES ('8')`, "c_case", violates},
		{"temp parent is not main's", `INSERT INTO c_shadow VALUES ('t')`, "c_shadow", violates},
		{"quoted names", `INSERT INTO "c q""uote" VALUES ('7')`, `c q"uote`, clean},
		{"quoted names missing", `INSERT INTO "c q""uote" VALUES ('8')`, `c q"uote`, violates},
		{"repeated index column", `INSERT INTO c_dup VALUES (1, 99)`, "c_dup", clean},
		{"repeated index column missing", `INSERT INTO c_dup VALUES (3, 3)`, "c_dup", violates},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}()
			if _, err := tx.Exec(tc.rows); err != nil {
				t.Fatal(err)
			}
			got := requireForeignKeyCheckMatchesPragma(t, tx)
			reported := false
			for key := range got {
				if key.table != tc.table {
					t.Errorf("foreign_key_check reports %v, outside the case's table %s", key, tc.table)
				}
				reported = true
			}
			if tc.want != either && reported != (tc.want == violates) {
				t.Errorf("foreign_key_check reports %v, want a violation = %v", got, tc.want == violates)
			}
		})
	}

	// The repeated-column index makes c_dup's key one only the pragma can
	// map; every other table gets a probe per key.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}()
	probes, err := foreignKeyProbes(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range probes {
		if (probe.query == "") != (probe.table == "c_dup") {
			t.Errorf("probe of %s foreign key %d runs the pragma = %v", probe.table, probe.id, probe.query == "")
		}
	}
}

// TestForeignKeyCheckFailsOnMismatch: a foreign key whose parent columns
// have no usable unique index makes the pragma fail before it reads a
// row; the check fails the same way with no child rows at all.
func TestForeignKeyCheckFailsOnMismatch(t *testing.T) {
	db := adHocForeignKeyDB(t)
	mustExec(t, db, `CREATE TABLE p_mismatch(k TEXT)`)
	mustExec(t, db, `CREATE UNIQUE INDEX p_mismatch_k ON p_mismatch(k COLLATE NOCASE)`)
	mustExec(t, db, `CREATE TABLE c_mismatch(k TEXT REFERENCES p_mismatch(k))`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}()
	if _, err := tx.Query(`PRAGMA foreign_key_check`); err == nil || !strings.Contains(err.Error(), "foreign key mismatch") {
		t.Fatalf("foreign_key_check = %v, want a mismatch", err)
	}
	if err := assertForeignKeysIntact(t.Context(), tx); err == nil || !strings.Contains(err.Error(), "foreign key mismatch") {
		t.Fatalf("assertForeignKeysIntact = %v, want the pragma's mismatch", err)
	}
}

// TestForeignKeyCheckReadsLargeTablesFromIndexes pins the plan of every
// probe of the tables that hold over 100,000 rows on a long-used database:
// the child's key columns come from a covering index and each parent is
// found by key. Dropping the index that covers a key fails here.
func TestForeignKeyCheckReadsLargeTablesFromIndexes(t *testing.T) {
	db := migratedDB(t)
	large := []string{"items", "payloads", "payload_chunks", "thread_search_rows"}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}()
	probes, err := foreignKeyProbes(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	probed := map[string]int{}
	for _, probe := range probes {
		if !slices.Contains(large, probe.table) {
			continue
		}
		probed[probe.table]++
		if probe.query == "" {
			t.Errorf("%s foreign key %d runs the pragma, which reads the table", probe.table, probe.id)
			continue
		}
		plan := queryPlanDetails(t, tx, probe.query)
		var child, parent bool
		for _, step := range plan {
			child = child || strings.HasPrefix(step, "SCAN c USING COVERING INDEX ") || strings.HasPrefix(step, "SEARCH c USING COVERING INDEX ")
			parent = parent || strings.HasPrefix(step, "SEARCH p USING ") && (strings.Contains(step, "INDEX") || strings.Contains(step, "PRIMARY KEY"))
		}
		if !child || !parent {
			t.Errorf("%s foreign key %d plan %q: want the child read from a covering index and the parent searched by key", probe.table, probe.id, plan)
		}
	}
	for _, table := range large {
		if probed[table] == 0 {
			t.Errorf("%s has no foreign key probe; update the list of large tables", table)
		}
	}
}

func queryPlanDetails(t *testing.T, tx *sql.Tx, query string) []string {
	t.Helper()
	rows, err := tx.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

// TestRebuildMigrationFailsOnForeignKeyViolation: a rebuild's ordinary
// statements run with enforcement off, so a delete that strands a child
// row must fail the migration, roll it back and record nothing; a rebuild
// that keeps every parent applies.
func TestRebuildMigrationFailsOnForeignKeyViolation(t *testing.T) {
	latest := migrations[len(migrations)-1].Version
	db := migratedDB(t)
	mustExec(t, db, `CREATE TABLE synthetic_fk_parent (id TEXT PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE synthetic_fk_child (parent_id TEXT NOT NULL REFERENCES synthetic_fk_parent(id))`)
	mustExec(t, db, `INSERT INTO synthetic_fk_parent VALUES ('kept'), ('stranding')`)
	mustExec(t, db, `INSERT INTO synthetic_fk_child VALUES ('kept'), ('stranding')`)

	stranding := Migration{
		Version: latest + 1,
		Name:    "synthetic_stranding_rebuild",
		SQL: `CREATE TABLE synthetic_fk_parent_new (id TEXT PRIMARY KEY);
INSERT INTO synthetic_fk_parent_new SELECT id FROM synthetic_fk_parent;
DROP TABLE synthetic_fk_parent;
ALTER TABLE synthetic_fk_parent_new RENAME TO synthetic_fk_parent;
DELETE FROM synthetic_fk_parent WHERE id = 'stranding';`,
		Rebuild: true,
	}
	err := applyRebuildMigration(db, stranding)
	if err == nil || !strings.Contains(err.Error(), `table "synthetic_fk_child"`) || !strings.Contains(err.Error(), `"synthetic_fk_parent"`) {
		t.Fatalf("stranding rebuild = %v, want a foreign key violation naming synthetic_fk_child and its parent", err)
	}
	var parents, recorded, enforced int
	if err := db.QueryRow(`SELECT COUNT(*) FROM synthetic_fk_parent`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_versions WHERE version = ?`, stranding.Version).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&enforced); err != nil {
		t.Fatal(err)
	}
	if parents != 2 || recorded != 0 || enforced != 1 {
		t.Fatalf("after the failed rebuild: %d parents, recorded %d, foreign_keys %d; want 2, 0, 1", parents, recorded, enforced)
	}

	keeping := stranding
	keeping.Name = "synthetic_keeping_rebuild"
	keeping.SQL = strings.ReplaceAll(stranding.SQL, `DELETE FROM synthetic_fk_parent WHERE id = 'stranding';`, ``)
	if err := applyRebuildMigration(db, keeping); err != nil {
		t.Fatalf("keeping rebuild: %v", err)
	}
}
