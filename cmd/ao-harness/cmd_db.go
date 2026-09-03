package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"

	_ "modernc.org/sqlite"
)

// readOnlyVerbs are the statement kinds `db` will run. The guard is a
// whitelist rather than a blacklist of writes: SQLite keeps adding
// statements, and a new one must default to refused. WITH is
// deliberately absent — `WITH x AS (...) DELETE FROM ...` is a valid
// SQLite statement whose first keyword says nothing about what it does.
var readOnlyVerbs = []string{"SELECT", "PRAGMA", "EXPLAIN"}

// maxDBRows caps what one query prints. A harness database is small, but
// `db 'SELECT * FROM items'` on a soak run is not, and a CLI that
// buffers a million rows to align a table is a memory bug.
const maxDBRows = 10_000

// defaultDBColWidth keeps a table cell readable. It is not a data
// limit: --max-col-width 0 prints whole values, and -o json never
// truncates at all.
const defaultDBColWidth = 64

func runDB(e *env, args []string) error {
	flags := e.newFlagSet("db '<SELECT ...>'")
	dbPath := flags.String("file", "", "database file to open (default: the instance's own)")
	limit := flags.Int("limit", maxDBRows, "stop after this many rows")
	colWidth := flags.Int("max-col-width", defaultDBColWidth, "truncate text cells at this many runes (0 = print them whole)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *colWidth < 0 {
		return usagef("--max-col-width must not be negative (0 means no truncation)")
	}
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		return usagef("db needs exactly one SQL statement (quote it)")
	}
	statement, err := checkReadOnly(rest[0])
	if err != nil {
		return err
	}

	path := *dbPath
	if path == "" {
		path, err = e.dbPath()
		if err != nil {
			return err
		}
	} else if err := refuseRealAppDatabase(path); err != nil {
		return err
	}

	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Query(statement)
	if err != nil {
		return fmt.Errorf("query %s: %w", path, err)
	}
	defer rows.Close()
	return e.printRows(rows, *limit, *colWidth)
}

// dbPath asks the running instance where its database is. Unlike `logs`
// there is no offline fallback on purpose: the store path is the
// backend's answer, and guessing it would be guessing at the one file a
// wrong guess silently reads stale data from.
func (e *env) dbPath() (string, error) {
	ctx := context.Background()
	var path string
	err := e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		info, err := client.Info(ctx)
		if err != nil {
			return err
		}
		if info.DBPath == "" {
			return fmt.Errorf("the instance reported no database path")
		}
		path = info.DBPath
		return nil
	})
	return path, err
}

// refuseRealAppDatabase keeps `--file` inside the harness's own world.
//
// Without the flag this command asks a HARNESS instance where its store
// is, and a harness boot has already refused to run against the OS config
// root (main_harness.go's refuseRealDataDir). `--file` skips both, so
// `ao-harness db --file ~/.config/agent-overflow/agent-overflow.db
// 'SELECT * FROM items'` reads the developer's real threads through a
// tool whose whole contract is "this only ever touches a test instance".
// Read-only is not the point: those rows are the user's real
// conversations, and an agent handed this CLI must not be able to page
// through them by accident.
//
// The app-managed root comes from internal/appdirs — the same
// UserConfigDir-then-UserHomeDir chain the app itself resolves. Using
// os.UserConfigDir alone here was a hole rather than a simplification:
// on a machine with no config dir the app falls back to $HOME and this
// check returned nil, so --file could reach exactly the real store it
// exists to keep out. Importing appdirs (a stdlib-only path helper, no
// App code) is what makes the two answers one answer.
//
// A root that cannot be resolved AT ALL is a refusal, not a pass: with
// nothing to compare against, "this is not the real database" is a claim
// this function cannot make.
func refuseRealAppDatabase(path string) error {
	realDir, err := appdirs.Root()
	if err != nil {
		return usagef(
			"db refuses --file %s: neither a config dir nor a home dir resolves on this machine, "+
				"so there is no way to prove the path is not the real app data dir (drop --file to read the instance's own store)", path)
	}
	realDir, err = filepath.Abs(realDir)
	if err != nil {
		return usagef("db refuses --file %s: cannot resolve the app data dir to compare against (%v)", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve --file %q: %w", path, err)
	}
	if !underDir(abs, realDir) {
		return nil
	}
	return usagef(
		"db refuses --file %s: it is inside the real app data dir %s, and this CLI only reads harness instances "+
			"(drop --file to read the instance's own store, or point it at a harness data dir)", abs, realDir)
}

// underDir reports whether path is dir itself or sits beneath it, after
// resolving links on whichever components exist. Symlinks are resolved
// because a link planted in a scratch dir is the obvious way past a
// string comparison, and EvalSymlinks on a missing path fails — so a
// non-existent --file falls back to the lexical form, which is the right
// answer for a path that names nothing yet.
func underDir(path, dir string) bool {
	path, pathErr := instanceinfo.CanonicalPath(path)
	dir, dirErr := instanceinfo.CanonicalPath(dir)
	if pathErr != nil || dirErr != nil {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// openReadOnly opens the store as a reader that cannot write.
//
// mode=ro is the file-open flag; immutable=0 is explicit because
// immutable=1 would tell SQLite the file cannot change underneath it,
// which is false — a live backend is writing to it — and would serve
// this process a stale page cache with no WAL recovery. query_only is
// the belt: mode=ro refuses at the OS layer, query_only refuses at the
// statement layer, and a guard bug should hit the second one too.
//
// The belt is only a belt. `PRAGMA` is on the verb whitelist, so
// `PRAGMA query_only=0` reaches this connection and SQLite honours it —
// writes then fail anyway, at mode=ro. That is fine as long as mode=ro
// stays: it is the layer doing the work, and one invocation runs one
// statement, so nothing can loosen the flag and then use it. Never drop
// mode=ro on the theory that query_only covers it.
func openReadOnly(path string) (*sql.DB, error) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	dsn := "file:" + escaped + "?mode=ro&immutable=0&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	// One connection: a read-only CLI running one statement has nothing to
	// parallelise, and a pool would open the file several times.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	return db, nil
}

// checkReadOnly enforces the two rules that make this command safe to
// hand an agent: exactly one statement, and its first keyword names a
// read.
//
// The scan is conservative and literal — it understands SQLite's string,
// identifier and comment forms well enough to know which semicolons are
// separators, and refuses anything it cannot classify. It is NOT a SQL
// parser and must not grow into one; the safety that matters comes from
// mode=ro plus query_only, and this is what turns a violation into a
// clear message instead of a driver error.
func checkReadOnly(raw string) (string, error) {
	statement := strings.TrimSpace(raw)
	body, err := splitSingleStatement(statement)
	if err != nil {
		return "", err
	}
	verb := leadingKeyword(body)
	if verb == "" {
		return "", usagef("db: %q contains no statement", raw)
	}
	for _, allowed := range readOnlyVerbs {
		if verb == allowed {
			return body, nil
		}
	}
	return "", usagef("db refuses %s: only %s statements are allowed (this connection is read-only)",
		verb, strings.Join(readOnlyVerbs, "/"))
}

// splitSingleStatement returns the statement with a single trailing
// semicolon removed, and errors when a second statement follows. A
// semicolon inside a quoted string or a comment is not a separator.
//
// The walk is over BYTES. Every delimiter SQLite gives meaning to is
// ASCII, and UTF-8 continuation bytes are all >= 0x80, so a multi-byte
// rune inside a string literal cannot be mistaken for one.
func splitSingleStatement(statement string) (string, error) {
	for i := 0; i < len(statement); i++ {
		switch statement[i] {
		case '\'', '"', '`':
			end := closingQuote(statement, i, statement[i])
			if end < 0 {
				return "", usagef("db: unterminated %c-quoted text", statement[i])
			}
			i = end
		case '[':
			end := strings.IndexByte(statement[i+1:], ']')
			if end < 0 {
				return "", usagef("db: unterminated [identifier]")
			}
			i += 1 + end
		case '-':
			if i+1 < len(statement) && statement[i+1] == '-' {
				end := strings.IndexByte(statement[i:], '\n')
				if end < 0 {
					return strings.TrimSpace(statement[:i]), nil
				}
				i += end
			}
		case '/':
			if i+1 < len(statement) && statement[i+1] == '*' {
				end := strings.Index(statement[i+2:], "*/")
				if end < 0 {
					return "", usagef("db: unterminated /* comment")
				}
				i += 2 + end + 1
			}
		case ';':
			if strings.TrimSpace(statement[i+1:]) != "" {
				return "", usagef("db accepts exactly one statement; a second one starts after byte %d", i+1)
			}
			return strings.TrimSpace(statement[:i]), nil
		}
	}
	return statement, nil
}

// closingQuote finds the end of a quoted run, honouring SQLite's doubled
// quote escape (two single quotes inside a quoted run stand for one).
func closingQuote(statement string, start int, quote byte) int {
	for i := start + 1; i < len(statement); i++ {
		if statement[i] != quote {
			continue
		}
		if i+1 < len(statement) && statement[i+1] == quote {
			i++
			continue
		}
		return i
	}
	return -1
}

// leadingKeyword returns the first word, skipping leading comments and
// whitespace, uppercased.
func leadingKeyword(statement string) string {
	rest := statement
	for {
		rest = strings.TrimLeftFunc(rest, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' })
		switch {
		case strings.HasPrefix(rest, "--"):
			cut := strings.IndexByte(rest, '\n')
			if cut < 0 {
				return ""
			}
			rest = rest[cut+1:]
		case strings.HasPrefix(rest, "/*"):
			cut := strings.Index(rest[2:], "*/")
			if cut < 0 {
				return ""
			}
			rest = rest[2+cut+2:]
		default:
			end := strings.IndexFunc(rest, func(r rune) bool {
				return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ';'
			})
			if end < 0 {
				return strings.ToUpper(rest)
			}
			return strings.ToUpper(rest[:end])
		}
	}
}

// printRows renders a result set. Text output is an aligned table; JSON
// is an array of objects keyed by column name, which is what a script
// pipes into jq.
func (e *env) printRows(rows *sql.Rows, limit, colWidth int) error {
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	var (
		textRows  [][]string
		objects   []map[string]any
		count     int
		truncated bool
	)
	for rows.Next() {
		if limit > 0 && count >= limit {
			fmt.Fprintf(e.stderr, "ao-harness: stopped at --limit %d rows\n", limit)
			break
		}
		count++
		scanned := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range scanned {
			targets[i] = &scanned[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		if e.jsonOutput() {
			object := make(map[string]any, len(columns))
			for i, name := range columns {
				object[name] = jsonValue(scanned[i])
			}
			objects = append(objects, object)
			continue
		}
		cells := make([]string, len(columns))
		for i := range columns {
			value := textValue(scanned[i])
			if colWidth > 0 {
				cut := truncate(value, colWidth)
				truncated = truncated || cut != value
				value = cut
			} else {
				// Newlines and tabs still have to go: they are the tabwriter's
				// own delimiters, and a multi-line cell would shear the table.
				value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\t", " ")
			}
			cells[i] = value
		}
		textRows = append(textRows, cells)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if e.jsonOutput() {
		if objects == nil {
			objects = []map[string]any{}
		}
		return e.writeJSON(objects)
	}
	if len(textRows) == 0 {
		e.printf("no rows\n")
		return nil
	}
	if err := e.table(columns, textRows); err != nil {
		return err
	}
	if truncated {
		// Say it once, and say what the recourse is. A silently cut value
		// is how a reader concludes a column holds something it does not.
		e.printf("(truncated — use -o json or --max-col-width 0)\n")
	}
	return nil
}

// jsonValue keeps SQLite's own types where JSON has them. A BLOB
// arrives as []byte; rendering it as a string is right for the text
// columns this database is almost entirely made of, and a caller who
// wanted bytes would ask for hex() in the query.
func jsonValue(value any) any {
	if raw, ok := value.([]byte); ok {
		return string(raw)
	}
	return value
}

func textValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(typed)
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}
