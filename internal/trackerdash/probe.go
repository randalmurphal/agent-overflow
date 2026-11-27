package trackerdash

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"agent-overflow/internal/store"
	"modernc.org/sqlite/vtab"
)

// The probe is how a count learns what its metrics read of the copy, as
// SQLite resolves their statements. Its session holds each export
// relation as a probe table, a virtual table with the relation's columns
// (store.MirrorExportSchema) and no rows. SQLite plans every table a
// statement keeps in its plan when the statement prepares, and tells a
// virtual table which of its columns the statement uses (colUsed): the
// columns of a USING or NATURAL join's condition included, a CTE that
// shadows a relation's name resolved to the CTE, and nothing for a *
// SQLite expands but never reads. So the tables planned and their columns
// used are what the statement reads, which the count copies and How this
// is counted names (asks).
//
// The probe can only ask for more than the real copy reads: a LEFT JOIN
// on a whole unique key that the statement reads nothing of is dropped
// from the real plan, not from a virtual table's, so it names a table the
// statement names and does not read.

// probeModuleName is the probe tables' module, registered on every
// session's driver (driverFor).
const probeModuleName = "ao_dashboard_probe"

// probeColumnsMax is the most columns a probe table can say a statement
// uses: colUsed's last bit stands for every column from the 64th on.
const probeColumnsMax = 63

// asks is what a count of a metric asks the copy for: the relations its
// statement reads and the columns of store.MirrorExportColumns it reads,
// which the copy fills only when asked.
type asks struct {
	tables, columns []string
}

// fills is a as the export's relations name them (store.ExportMirrorBoard).
func (a asks) fills() []string { return slices.Concat(a.tables, a.columns) }

// exportColumns is each export relation's columns, with their declared
// types, in their order: what a probe table declares.
var exportColumns = sync.OnceValues(func() (map[string][]string, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	out, err := func() (map[string][]string, error) {
		if _, err := db.Exec(store.MirrorExportSchema); err != nil {
			return nil, err
		}
		out := map[string][]string{}
		for _, table := range store.MirrorExportTables {
			rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?) ORDER BY cid`, table)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var name, typ string
				if err := rows.Scan(&name, &typ); err != nil {
					return nil, errors.Join(err, rows.Close())
				}
				out[table] = append(out[table], strings.TrimSpace(name+" "+typ))
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			if len(out[table]) > probeColumnsMax {
				return nil, fmt.Errorf("relation %s has %d columns, more than a probe table tells apart (%d)", table, len(out[table]), probeColumnsMax)
			}
		}
		return out, nil
	}()
	if closeErr := db.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return nil, fmt.Errorf("read the export's relations for the dashboard's probe: %w", err)
	}
	return out, nil
})

// probeUse is what the statements a probe session prepares use of each
// probe table: the colUsed bits of every plan of it, by table.
type probeUse map[string]uint64

// createProbeTables makes s a probe session: every export relation a
// probe table that records into s.probe.
func (s *session) createProbeTables(exec func(string) error) error {
	columns, err := exportColumns()
	if err != nil {
		return err
	}
	s.probe = probeUse{}
	for _, table := range store.MirrorExportTables {
		if err := exec(`CREATE VIRTUAL TABLE main.` + table + ` USING ` + probeModuleName + `('` + s.key + `', ` + strings.Join(columns[table], ", ") + `)`); err != nil {
			return fmt.Errorf("create the probe of %s: %w", table, err)
		}
	}
	return nil
}

// takeAsks clears and answers what the probe tables recorded since the
// last take: the tables planned, and each column of
// store.MirrorExportColumns whose bit its table's plans set.
func (s *session) takeAsks() (asks, error) {
	columns, err := exportColumns()
	if err != nil {
		return asks{}, err
	}
	s.mu.Lock()
	used := s.probe
	s.probe = probeUse{}
	s.mu.Unlock()
	var a asks
	for _, table := range store.MirrorExportTables {
		if _, ok := used[table]; ok {
			a.tables = append(a.tables, table)
		}
	}
	slices.Sort(a.tables)
	for _, c := range store.MirrorExportColumns {
		table, column, _ := strings.Cut(c, ".")
		i := slices.IndexFunc(columns[table], func(d string) bool { n, _, _ := strings.Cut(d, " "); return n == column })
		if i < 0 {
			return asks{}, fmt.Errorf("the export has no column %s", c)
		}
		if used[table]&(1<<i) != 0 {
			a.columns = append(a.columns, c)
		}
	}
	return a, nil
}

// probeModule makes probe tables: its arguments are the session's key and
// the table's column declarations.
type probeModule struct{}

func (probeModule) Create(ctx vtab.Context, args []string) (vtab.Table, error) {
	// args: the module, the database, the table, then the module's own.
	if len(args) < 5 {
		return nil, fmt.Errorf("a probe table takes a session key and its columns, not %q", args)
	}
	s, ok := sessionOf(strings.Trim(args[3], "' "))
	if !ok {
		return nil, errors.New("a probe table names no open dashboard session")
	}
	if err := ctx.Declare("CREATE TABLE x(" + strings.Join(args[4:], ", ") + ")"); err != nil {
		return nil, err
	}
	return &probeTable{s: s, name: args[2]}, nil
}

func (m probeModule) Connect(ctx vtab.Context, args []string) (vtab.Table, error) {
	return m.Create(ctx, args)
}

type probeTable struct {
	s    *session
	name string
}

// BestIndex records the columns a plan of the table uses. SQLite calls it
// while a statement prepares, on the goroutine that prepares it.
func (t *probeTable) BestIndex(info *vtab.IndexInfo) error {
	t.s.mu.Lock()
	t.s.probe[t.name] |= info.ColUsed
	t.s.mu.Unlock()
	info.EstimatedCost = 1e6
	return nil
}

// errProbeRun is a statement run on a probe session, which only
// prepares.
var errProbeRun = errors.New("a dashboard probe holds no rows to run a statement on")

func (t *probeTable) Open() (vtab.Cursor, error) { return nil, errProbeRun }
func (t *probeTable) Disconnect() error          { return nil }
func (t *probeTable) Destroy() error             { return nil }
