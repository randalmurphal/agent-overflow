package main

// The state-clone repro rig, half one: build a harness data root out of a
// COPY of the developer's real app data dir, so a harness instance can run
// the state a bug actually happens in — the user's own threads, their item
// counts, their payload sizes — with every provider process still mocked.
//
// The threat model, so a reviewer can check it rather than trust it:
//
//   - Credentials never live in the data dir. They live under the provider
//     home, which a harness boot redirects to <dataRoot>/home
//     (isolateHarnessHome), so a copy of the data dir cannot carry one.
//     provider-accounts.json — non-secret account metadata, but stamped
//     with the REAL provider home — is deliberately not copied either; a
//     fresh harness store stamps its own, and the boot prune refuses a
//     metadata store whose stamp disagrees with the credential home.
//   - Provider binaries are re-pointed at the mock on EVERY boot
//     (seedHarnessSettings), so settings.json is not copied and could not
//     matter if it were.
//   - Every resume-shaped handle in the copied database is neutralized
//     before the copy is usable: threads.session_ref and the pending-fork
//     pair, and thread_import_state's provider session ids. A cloned
//     thread therefore cannot resume a real provider session even if
//     something did reach a real binary.
//   - ui_state is dropped wholesale. Those rows are client-scoped restore
//     state; carried into a different instance they name panes and threads
//     that resolve to sql.ErrNoRows toasts (the leak HarnessReset fixed).
//
// What the copy DOES carry is the user's real conversation content,
// verbatim. That is the point of the verb and the reason every message it
// prints says where the copy lives and that it must never be committed.
//
// Like cmd_up.go's refusals, the SQL here is written out rather than
// imported: this binary links no App or store code, which is what keeps
// the CLI from becoming a second way to drive the app. The cost of the
// duplication drifting is that a scrub misses a column added later, which
// is why scrubStatements names its tables and columns explicitly and the
// tests assert each one.

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/harness/instanceinfo"

	_ "modernc.org/sqlite"
)

// storeFileName is the database inside an app data dir. Restated here for
// the same reason the refusals are.
const storeFileName = "agent-overflow.db"

// attachmentsDirName is the on-disk half of the attachments table. The
// rows are inside the database; the bytes are not, and an item whose
// attachment file is missing renders broken.
const attachmentsDirName = "attachments"

// cloneSuffix is appended to this worktree's default data root when
// --data-dir is omitted, exactly as instanceinfo.SoakSuffix is for a
// soak. A clone must not land on the root `make harness` uses: those two
// instances answer different questions and one would silently inherit the
// other's threads.
const cloneSuffix = "-clone"

func runClone(e *env, args []string) error {
	flags := e.newFlagSet("clone")
	from := flags.String("from", "", "the app data dir to copy (the real one — that is the point of this verb)")
	dataDir := flags.String("data-dir", "", "data root to build (default: this worktree's default root plus \""+cloneSuffix+"\")")
	force := flags.Bool("force", false, "replace an existing database at the target instead of refusing")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("clone takes no positional arguments (got %v)", rest)
	}
	if strings.TrimSpace(*from) == "" {
		return usagef("clone needs --from <app data dir> (the directory holding %s, or its parent)", storeFileName)
	}

	sourceDir, err := resolveSourceDataDir(*from)
	if err != nil {
		return err
	}
	targetRoot, err := cloneTargetRoot(*dataDir)
	if err != nil {
		return err
	}
	targetDir := filepath.Join(targetRoot, appDataDirName)

	// Every refusal runs BEFORE anything is created, the cmd_up.go rule:
	// a mistyped --data-dir must not leave a half-made tree inside the
	// directory the check was about to reject.
	if err := refuseUnsafeDataRoot(targetRoot); err != nil {
		return err
	}
	if err := refuseSecondInstance(e, targetRoot); err != nil {
		return err
	}
	if err := refuseCloneOntoSource(sourceDir, targetDir); err != nil {
		return err
	}

	sourceDB := filepath.Join(sourceDir, storeFileName)
	targetDB := filepath.Join(targetDir, storeFileName)
	if err := prepareCloneTarget(targetRoot, targetDir, targetDB, *force); err != nil {
		return err
	}

	if err := snapshotDatabase(sourceDB, targetDB); err != nil {
		return err
	}
	scrubbed, err := scrubClonedDatabase(e, targetDB)
	if err != nil {
		return err
	}
	attachments, err := copyAttachments(filepath.Join(sourceDir, attachmentsDirName), filepath.Join(targetDir, attachmentsDirName))
	if err != nil {
		return err
	}
	schemaVersion, schemaKnown := readSchemaVersion(targetDB)

	if e.jsonOutput() {
		out := map[string]any{
			"sourceDataDir": sourceDir,
			"dataRoot":      targetRoot,
			"dataDir":       targetDir,
			"database":      targetDB,
			"scrub":         scrubbed,
			"attachments":   attachments,
			"instance":      instanceinfo.ID(targetRoot),
			"up":            fmt.Sprintf("ao-harness up --data-dir %s", targetRoot),
		}
		if schemaKnown {
			out["schemaVersion"] = schemaVersion
		}
		return e.writeJSON(out)
	}
	e.printf("cloned %s\n", sourceDB)
	e.printf("     → %s\n", targetDB)
	if schemaKnown {
		// A diagnostic, not a gate: the CLI links no store code, so it
		// cannot say what version the booting binary expects. Boot
		// migrates an older store forward and refuses a newer one loudly;
		// this line is what makes that refusal attributable to the clone.
		e.printf("  %-28s v%d (boot migrates forward; a newer store than the binary fails at boot)\n", "schema version", schemaVersion)
	}
	for _, row := range scrubbed {
		e.printf("  %-28s %s\n", row.What, row.Detail)
	}
	e.printf("  %-28s %d file(s) copied", "attachments", attachments.Files)
	if attachments.Skipped > 0 {
		e.printf(", %d skipped (not a regular file)", attachments.Skipped)
	}
	e.printf("\n")
	e.printf("  %-28s %s\n", "not copied", strings.Join(cloneExclusions, ", "))
	e.printf("\nboot it:\n  ao-harness up --data-dir %s\n", targetRoot)
	e.printf("\nthis copy carries your real session content verbatim. It lives only in\n")
	e.printf("%s — never commit it, and delete it when the repro is done.\n", targetRoot)
	return nil
}

// cloneExclusions is what a clone deliberately leaves behind, printed so
// an operator can see the list rather than infer it from a doc.
var cloneExclusions = []string{
	"settings.json",
	"provider-accounts.json",
	"replay/",
	"ui-trace/",
	"design-workdirs/",
	"logs/",
	"account-audit.log",
	"usage-backoff.json",
	"harness-instance.json",
}

// resolveSourceDataDir accepts either the app data dir itself or its
// parent, because both are things an operator has a name for: the app
// data dir is `~/.config/agent-overflow`, and its parent is what
// `--data-dir` means everywhere else in this CLI.
//
// It deliberately does NOT run `db --file`'s real-app-data refusal. That
// guard exists to stop an agent paging through the developer's threads by
// accident; this verb's whole contract is the opposite, and the operator
// typed the path.
func resolveSourceDataDir(from string) (string, error) {
	abs, err := filepath.Abs(from)
	if err != nil {
		return "", fmt.Errorf("resolve --from %q: %w", from, err)
	}
	candidates := []string{abs, filepath.Join(abs, appDataDirName)}
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, storeFileName)); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"no %s under --from %s (looked in %s); point --from at the app data dir or the root holding it",
		storeFileName, abs, strings.Join(candidates, " and "))
}

func cloneTargetRoot(flagValue string) (string, error) {
	if strings.TrimSpace(flagValue) != "" {
		abs, err := filepath.Abs(flagValue)
		if err != nil {
			return "", fmt.Errorf("resolve --data-dir %q: %w", flagValue, err)
		}
		return abs, nil
	}
	return instanceinfo.DefaultDataRoot() + cloneSuffix, nil
}

// refuseCloneOntoSource keeps the two trees disjoint in both directions.
// Cloning onto the source would VACUUM a database into itself; cloning
// into a directory BELOW the source would put a harness data root inside
// the real app data dir, where the next boot's wholesale directory
// handling would operate on it.
func refuseCloneOntoSource(sourceDir, targetDir string) error {
	if sameResolvedPath(sourceDir, targetDir) {
		return usagef("clone refuses to write into its own source %s (pick a different --data-dir)", sourceDir)
	}
	if underDir(targetDir, sourceDir) {
		return usagef("clone refuses --data-dir %s: it is inside the source %s", targetDir, sourceDir)
	}
	if underDir(sourceDir, targetDir) {
		return usagef("clone refuses --data-dir %s: the source %s is inside it", targetDir, sourceDir)
	}
	return nil
}

// prepareCloneTarget creates the target tree at 0700 — the mode
// refuseUnsafeHarnessDir demands at boot — and settles what happens to a
// database already sitting there. VACUUM INTO refuses an existing file,
// so an operator re-cloning would otherwise get SQLite's own message
// rather than a choice.
func prepareCloneTarget(targetRoot, targetDir, targetDB string, force bool) error {
	if _, err := os.Stat(targetDB); err == nil {
		if !force {
			return fmt.Errorf(
				"%s already exists; pass --force to replace it, or pick another --data-dir", targetDB)
		}
		// The sidecars go with it: a WAL left beside a replaced database
		// is another database's journal.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(targetDB + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", targetDB+suffix, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", targetDB, err)
	}
	for _, dir := range []string{targetRoot, targetDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}
	return nil
}

// snapshotDatabase copies the store with VACUUM INTO rather than copying
// the file, because the real app may be RUNNING while this happens: a
// file copy of a WAL database is a torn read of the main file plus
// whatever the -wal happened to hold, while VACUUM INTO writes one
// consistent single-file snapshot of the reader's own transaction,
// committed WAL content included.
//
// The source connection is mode=ro. It is NOT query_only, and that is a
// measured divergence rather than an oversight: query_only(1) refuses
// VACUUM INTO outright ("attempt to write a readonly database"), even
// though the statement writes only to the output file. mode=ro is the
// layer that actually enforces read-only against the source — cmd_db.go
// says the same thing about its own belt — and it permits the copy, so
// this is the only DSN that can do the job at all.
//
// One honest caveat: opening a WAL database read-only makes SQLite create
// its own -shm / -wal index sidecars beside the source when they are
// absent. No database CONTENT is touched, and a running app already has
// both; a quiesced one gets two sidecar files its next boot would have
// created anyway.
func snapshotDatabase(sourceDB, targetDB string) error {
	db, err := openSourceForSnapshot(sourceDB)
	if err != nil {
		return err
	}
	defer db.Close()
	// A literal rather than a bind parameter: SQLite's VACUUM INTO takes
	// an expression, and modernc's driver has no reason to prefer one
	// form, but a literal is what the statement reads as in a log.
	if _, err := db.Exec("VACUUM INTO '" + strings.ReplaceAll(targetDB, "'", "''") + "'"); err != nil {
		return fmt.Errorf("snapshot %s into %s: %w", sourceDB, targetDB, err)
	}
	return nil
}

func openSourceForSnapshot(path string) (*sql.DB, error) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	dsn := "file:" + escaped + "?mode=ro&immutable=0&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	return db, nil
}

// scrubResult is one scrub statement's receipt, printed so an operator
// sees that the neutralization ran rather than assuming it.
type scrubResult struct {
	What   string `json:"what"`
	Detail string `json:"detail"`
	Rows   int64  `json:"rows"`
}

// scrubStatement names its table so a database predating the table can be
// skipped with a note rather than failing the clone.
type scrubStatement struct {
	what  string
	table string
	sql   string
	// detail renders the receipt; rows is what the UPDATE/DELETE reported.
	detail func(rows int64) string
}

// scrubStatements is the neutralization, written as SQL because this
// binary links no store code. Each entry is a fact about the schema, so
// each is asserted by a test rather than trusted.
func scrubStatements() []scrubStatement {
	return []scrubStatement{
		{
			what:  "threads",
			table: "threads",
			// The triple store.UpdateSessionRef always writes together. A
			// cloned thread with a live session_ref would try to --resume a
			// real provider session; the pending-fork pair is the same
			// handle held one step earlier.
			sql: `UPDATE threads
			         SET session_ref = NULL,
			             pending_fork_session_ref = NULL,
			             pending_fork_resume_at = ''`,
			detail: func(rows int64) string {
				return fmt.Sprintf("%d row(s), session refs cleared", rows)
			},
		},
		{
			what:  "thread_import_state",
			table: "thread_import_state",
			// Neutralized, not deleted: the rows carry the import cursor an
			// imported thread is read through, and dropping them would
			// change the thread's shape. What goes is the provider session
			// identity a refresh would reach for.
			sql: `UPDATE thread_import_state
			         SET source_session_id = '',
			             leaf_uuid = '',
			             source_parent_session_id = ''`,
			detail: func(rows int64) string {
				return fmt.Sprintf("%d row(s), source session ids neutralized", rows)
			},
		},
		{
			what:  "ui_state",
			table: "ui_state",
			// Client-scoped restore state. Carried into another instance it
			// names panes and threads that answer sql.ErrNoRows, which is
			// exactly the toast storm HarnessReset exists to prevent.
			sql: `DELETE FROM ui_state`,
			detail: func(rows int64) string {
				return fmt.Sprintf("%d row(s) deleted", rows)
			},
		},
	}
}

// stashedTrigger is one trigger's identity plus the DDL sqlite_master
// holds for it — the exact text to recreate it from, so the restore
// cannot drift from whatever schema version the source store was at.
type stashedTrigger struct {
	name string
	sql  string
}

// stashTableTriggers reads and DROPs every trigger attached to the named
// table, returning the DDL to recreate each one.
//
// It exists because the scrub is an offline neutralization of a dead
// copy, not an app write, and the store's integrity triggers judge it as
// if it were live traffic. Migration v63's
// thread_import_state_unique_source_update ABORTs when a second row of
// one provider reaches the same source_session_id — which is exactly
// what blanking every row's identity to the empty string does on any
// real store with two imports from one provider (found live 2026-08-26,
// 1811 rows).
// Generic over the table rather than naming that trigger, so a future
// trigger on a scrubbed table cannot re-open the hole.
func stashTableTriggers(db *sql.DB, table string) ([]stashedTrigger, error) {
	rows, err := db.Query(
		`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = ?`, table)
	if err != nil {
		return nil, fmt.Errorf("list triggers on %s: %w", table, err)
	}
	defer rows.Close()
	var stashed []stashedTrigger
	for rows.Next() {
		var t stashedTrigger
		var ddl sql.NullString
		if err := rows.Scan(&t.name, &ddl); err != nil {
			return nil, fmt.Errorf("scan trigger on %s: %w", table, err)
		}
		if !ddl.Valid || strings.TrimSpace(ddl.String) == "" {
			// A trigger whose DDL cannot be read back cannot be restored;
			// dropping it anyway would silently weaken the copy's schema.
			return nil, fmt.Errorf("trigger %s on %s has no DDL in sqlite_master; refusing to drop what cannot be restored", t.name, table)
		}
		t.sql = ddl.String
		stashed = append(stashed, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list triggers on %s: %w", table, err)
	}
	for _, t := range stashed {
		if _, err := db.Exec(`DROP TRIGGER "` + strings.ReplaceAll(t.name, `"`, `""`) + `"`); err != nil {
			return nil, fmt.Errorf("drop trigger %s on %s: %w", t.name, table, err)
		}
	}
	return stashed, nil
}

// restoreTableTriggers re-executes the stashed DDL. A failure here is a
// loud error: a clone whose copy silently lost its integrity triggers
// would enforce less than the store it claims to reproduce.
func restoreTableTriggers(db *sql.DB, table string, stashed []stashedTrigger) error {
	for _, t := range stashed {
		if _, err := db.Exec(t.sql); err != nil {
			return fmt.Errorf("restore trigger %s on %s: %w", t.name, table, err)
		}
	}
	return nil
}

// scrubClonedDatabase runs the neutralization against the COPY, opened
// read-write. The source is never opened this way.
//
// Each statement runs with its table's triggers dropped and restored
// around it — see stashTableTriggers for why the store's own integrity
// triggers must not judge an offline scrub.
func scrubClonedDatabase(e *env, targetDB string) ([]scrubResult, error) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(targetDB)
	db, err := sql.Open("sqlite", "file:"+escaped+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", targetDB, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("open %s: %w", targetDB, err)
	}

	results := make([]scrubResult, 0, len(scrubStatements()))
	for _, statement := range scrubStatements() {
		present, err := tableExists(db, statement.table)
		if err != nil {
			return nil, err
		}
		if !present {
			// A database predating the table is not a failed scrub: there
			// is nothing there to neutralize. Say so rather than skipping
			// in silence, because "the scrub ran" is the claim this verb
			// makes.
			fmt.Fprintf(e.stderr, "ao-harness: %s has no %s table; nothing to scrub there\n", targetDB, statement.table)
			results = append(results, scrubResult{What: statement.what, Detail: "absent in this database"})
			continue
		}
		triggers, err := stashTableTriggers(db, statement.table)
		if err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		result, err := db.Exec(statement.sql)
		if err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		if err := restoreTableTriggers(db, statement.table, triggers); err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("scrub %s in %s: %w", statement.table, targetDB, err)
		}
		results = append(results, scrubResult{What: statement.what, Detail: statement.detail(rows), Rows: rows})
	}
	return results, nil
}

// readSchemaVersion reads the copy's migration level
// (MAX(version) FROM migration_versions) for the clone receipt. Best
// effort: a database with no version table, or one that cannot be
// opened for this read, answers "unknown" rather than failing a clone
// that already succeeded.
func readSchemaVersion(targetDB string) (int64, bool) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(targetDB)
	db, err := sql.Open("sqlite", "file:"+escaped+"?mode=ro")
	if err != nil {
		return 0, false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM migration_versions`).Scan(&version); err != nil || !version.Valid {
		return 0, false
	}
	return version.Int64, true
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look for table %s: %w", name, err)
	}
	return true, nil
}

// attachmentCopy is what the byte half of the attachments table cost.
type attachmentCopy struct {
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Skipped int   `json:"skipped"`
}

// copyAttachments carries the on-disk half of the attachments table.
// Without it every item referencing an attachment renders broken, which
// looks like a rendering bug in the very repro the clone exists to serve.
//
// Symlinks are skipped rather than followed: the source is a real user
// directory, and a link there would copy whatever it points at into a
// harness root.
func copyAttachments(sourceDir, targetDir string) (attachmentCopy, error) {
	var out attachmentCopy
	info, err := os.Stat(sourceDir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("inspect %s: %w", sourceDir, err)
	}
	if !info.IsDir() {
		return out, fmt.Errorf("%s is not a directory", sourceDir)
	}
	walkErr := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(targetDir, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		if !entry.Type().IsRegular() {
			out.Skipped++
			return nil
		}
		written, err := copyFile(path, destination)
		if err != nil {
			return err
		}
		out.Files++
		out.Bytes += written
		return nil
	})
	if walkErr != nil {
		return out, fmt.Errorf("copy attachments from %s: %w", sourceDir, walkErr)
	}
	return out, nil
}

func copyFile(source, destination string) (int64, error) {
	in, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return written, copyErr
	}
	return written, closeErr
}
