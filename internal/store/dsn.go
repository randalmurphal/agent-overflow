package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// connPragma is one connection-scoped PRAGMA carried in a pool's DSN.
//
// Connection-scoped is the load-bearing word: unlike journal_mode (a
// property of the database file), these reset to their defaults on every
// new connection. Applying them with Exec after sql.Open only configures
// whichever connection happened to serve that call — database/sql is free
// to close and replace a pooled connection at any time (driver error
// paths, SetConnMaxLifetime, a Close during a failed transaction), and the
// replacement comes up with foreign_keys=0, busy_timeout=0 and
// synchronous=FULL. Measured against modernc.org/sqlite v1.56.0: a
// MaxOpenConns(1) pool configured by Exec reports fk=1/bt=5000/sync=1
// before a recycle and fk=0/bt=0/sync=2 after one, silently.
//
// Putting them in the DSN makes the driver re-apply them to every
// connection instance it opens, which is the only place that can hold.
type connPragma struct {
	// name is the PRAGMA name, used both to build the DSN token and to
	// read the setting back.
	name string
	// dsnValue is the argument the DSN passes, e.g. "5000" or "NORMAL".
	dsnValue string
	// want is the integer the PRAGMA reads back as once applied.
	// SQLite answers symbolic values numerically (synchronous=NORMAL
	// reads back as 1), so the verification compares integers.
	want int64
}

func (p connPragma) dsnToken() string {
	return "_pragma=" + p.name + "(" + p.dsnValue + ")"
}

// writerConnPragmas are the settings every writer connection must carry.
//
//   - busy_timeout=5000 lets SQLite poll a held lock for up to 5s before
//     surfacing SQLITE_BUSY. WAL allows concurrent readers plus one
//     writer, but UI threads, the checkpoint capture, the replay writer,
//     and the triage flusher all write — without the timeout the rare
//     contention window surfaces as "database is locked" toasts. Five
//     seconds is the canonical SQLite recommendation for a UI-attached
//     database; longer windows would just mask a real deadlock.
//   - foreign_keys=ON enforces the schema's cascades. A connection that
//     lost it writes orphans that no later connection can explain.
//   - synchronous=NORMAL is the WAL-recommended desktop config. With WAL
//     the journal is always fsync'd before commit; NORMAL drops the
//     redundant fsync of the main database file at checkpoint time.
//     Power-loss can lose the last few committed transactions but the
//     database cannot corrupt — and per root CLAUDE.md principle 2 the
//     provider session files are the authoritative history, so a
//     re-stream covers any lost SQLite-side writes. NORMAL meaningfully
//     shortens fsync stalls during stream bursts, which is the
//     per-block-stop freeze hot path.
var writerConnPragmas = []connPragma{
	{name: "busy_timeout", dsnValue: "5000", want: 5000},
	{name: "foreign_keys", dsnValue: "1", want: 1},
	{name: "synchronous", dsnValue: "NORMAL", want: 1},
}

// readerConnPragmas are the settings every read-pool connection must
// carry. query_only(1) makes a mis-routed write fail loudly instead of
// contending with the writer.
var readerConnPragmas = []connPragma{
	{name: "busy_timeout", dsnValue: "5000", want: 5000},
	{name: "query_only", dsnValue: "1", want: 1},
}

// dsnEscaper %-escapes the three characters that can cut a path short or
// corrupt the query string when it is spliced into a SQLite URI. Spaces
// pass through unescaped.
var dsnEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// poolDSN renders a database path as a SQLite URI DSN carrying pragmas.
//
// The URI form is what makes SQLITE_OPEN_URI parsing apply, and
// modernc.org/sqlite runs each _pragma value verbatim as a "PRAGMA ..."
// statement on every connection it opens (driver.go documents the apply
// order; busy_timeout goes first so nothing else can trip over a lock).
// ":memory:" renders as "file::memory:", the URI spelling of the same
// private in-memory database.
func poolDSN(dbPath string, pragmas []connPragma) string {
	tokens := make([]string, 0, len(pragmas))
	for _, p := range pragmas {
		tokens = append(tokens, p.dsnToken())
	}
	return "file:" + dsnEscaper.Replace(dbPath) + "?" + strings.Join(tokens, "&")
}

// verifyConnPragmas reads each pragma back and fails when it did not
// take.
//
// This is not belt-and-braces for a driver that might drop a setting —
// it catches the one DSN mistake the driver structurally cannot: _pragma
// values are executed verbatim and unvalidated, and SQLite silently
// ignores an unknown PRAGMA name. A DSN carrying "foreign_key(1)" opens
// without an error and runs the whole app with foreign keys off
// (measured against modernc.org/sqlite v1.56.0). One boot-time read per
// pragma turns that into a startup failure.
//
// It checks the connection the pool hands out now, which is the same
// connection every later caller gets configured the same way — the point
// is proving the DSN is spelled correctly, not surveying the pool.
func verifyConnPragmas(db *sql.DB, pragmas []connPragma) error {
	for _, p := range pragmas {
		var got int64
		if err := db.QueryRow("PRAGMA " + p.name).Scan(&got); err != nil {
			return fmt.Errorf("store: read back PRAGMA %s: %w", p.name, err)
		}
		if got != p.want {
			return fmt.Errorf(
				"store: PRAGMA %s = %d, want %d — the DSN did not apply it (check the _pragma spelling; SQLite ignores unknown pragma names silently)",
				p.name, got, p.want,
			)
		}
	}
	return nil
}
