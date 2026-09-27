package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// The connection-scoped PRAGMAs ride the DSN precisely so a recycled
// connection comes back configured. These tests pin that property (which
// the previous Exec-after-Open approach did not have), the verification
// that catches a misspelled _pragma, and the WAL truncation at the two
// lifecycle moments that own it.

func pragmaInt(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestWriterPragmasSurviveConnectionRecycling(t *testing.T) {
	s := newTestStore(t)

	assertWriterPragmas := func(when string) {
		t.Helper()
		for _, p := range writerConnPragmas {
			if got := pragmaInt(t, s.db, p.name); got != p.want {
				t.Fatalf("%s: PRAGMA %s = %d, want %d", when, p.name, got, p.want)
			}
		}
	}
	assertWriterPragmas("fresh connection")

	// Force database/sql to discard the pooled connection and open a
	// replacement. This is the real-world path (driver error handling,
	// a Close during a failed transaction) that used to silently return
	// foreign_keys=0 / busy_timeout=0 / synchronous=FULL.
	s.db.SetConnMaxLifetime(time.Nanosecond)
	time.Sleep(20 * time.Millisecond)
	assertWriterPragmas("recycled connection")
	s.db.SetConnMaxLifetime(0)

	// The behavioural half: foreign keys are actually enforced on the
	// replacement connection, not merely reported as on.
	_, err := s.db.Exec(
		`INSERT INTO threads (id, project_id, title, provider, model, workspace_path, mode, created_at, updated_at)
		 VALUES ('fk-orphan', 'no-such-project', 't', 'claude', 'm', '/tmp', 'chat', 1, 1)`,
	)
	if err == nil {
		t.Fatal("insert with a dangling project_id succeeded — foreign keys are not enforced after recycling")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("insert failed with %v, want a FOREIGN KEY constraint error", err)
	}
}

func TestReaderPragmasSurviveConnectionRecycling(t *testing.T) {
	s := newTestStore(t)
	if s.read == nil {
		t.Fatal("file-backed WAL store must open a read pool")
	}
	s.read.SetConnMaxLifetime(time.Nanosecond)
	time.Sleep(20 * time.Millisecond)
	for _, p := range readerConnPragmas {
		if got := pragmaInt(t, s.read, p.name); got != p.want {
			t.Fatalf("recycled read connection: PRAGMA %s = %d, want %d", p.name, got, p.want)
		}
	}
	s.read.SetConnMaxLifetime(0)
}

// TestPageCachesStayWithinTheirBudget reads a table larger than every page
// cache on each read connection. The driver pools the pages of all
// connections in one budget, the sum of their cache_size limits, and any
// connection may fill it, so the bound holds for the store's total.
func TestPageCachesStayWithinTheirBudget(t *testing.T) {
	s := newTestStore(t)
	if s.read == nil {
		t.Fatal("file-backed WAL store must open a read pool")
	}
	if _, err := s.db.Exec(`CREATE TABLE zz_pages (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	// 6 MB, three times SQLite's default cache.
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1500)
		INSERT INTO zz_pages (b) SELECT randomblob(4000) FROM n`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	writer, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	conns := []*sql.Conn{writer}
	for range readPoolConns {
		conn, err := s.read.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT sum(length(b)) FROM zz_pages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	total := 0
	for _, conn := range conns {
		used, err := connCacheUsed(conn)
		if err != nil {
			t.Fatal(err)
		}
		total += used
	}
	// The writer keeps SQLite's default of 2000 KiB. Page headers add
	// about 5% to a cache's configured size.
	budget := (2000 + readPoolConns*readerCacheKiB) << 10
	if limit := budget * 11 / 10; total > limit {
		t.Fatalf("page caches hold %d KiB after each reader read a 6 MB table, want at most %d KiB", total>>10, limit>>10)
	}
}

func connCacheUsed(conn *sql.Conn) (int, error) {
	used := 0
	err := conn.Raw(func(driverConn any) error {
		cache, ok := driverConn.(*stmtCacheConn)
		if !ok {
			return fmt.Errorf("read connection is %T, not the statement cache", driverConn)
		}
		status, ok := cache.sqliteConn.(sqlite.DBStatus)
		if !ok {
			return fmt.Errorf("driver connection %T reports no status", cache.sqliteConn)
		}
		current, _, err := status.Status(sqlite.DBStatusCacheUsed, false)
		used = current
		return err
	})
	return used, err
}

func TestPoolDSNRendersPragmasAndMemoryPaths(t *testing.T) {
	got := poolDSN("/data/agent overflow.db", writerConnPragmas)
	want := "file:/data/agent overflow.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=recursive_triggers(0)&_pragma=wal_autocheckpoint(16384)"
	if got != want {
		t.Fatalf("poolDSN = %q, want %q", got, want)
	}
	if got := poolDSN(":memory:", readerConnPragmas); got != "file::memory:?_pragma=busy_timeout(5000)&_pragma=query_only(1)&_pragma=cache_size(-500)" {
		t.Fatalf("poolDSN(:memory:) = %q", got)
	}
	// '?' and '#' would otherwise cut the path short or open a fragment.
	if got := poolDSN("/data/a?b#c%d.db", nil); got != "file:/data/a%3Fb%23c%25d.db?" {
		t.Fatalf("poolDSN escaping = %q", got)
	}
}

// A misspelled _pragma is the one DSN mistake the driver cannot catch:
// values are executed verbatim and SQLite ignores unknown PRAGMA names
// without complaining. verifyConnPragmas is what turns that into a
// startup failure instead of an app-lifetime silent misconfiguration.
func TestVerifyConnPragmasRejectsAPragmaThatDidNotApply(t *testing.T) {
	db, err := sql.Open("sqlite", poolDSN(filepath.Join(t.TempDir(), "typo.db"), []connPragma{
		{name: "foreign_key", dsnValue: "1", want: 1},
	}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	err = verifyConnPragmas(db, []connPragma{{name: "foreign_keys", dsnValue: "1", want: 1}})
	if err == nil {
		t.Fatal("verifyConnPragmas accepted a database whose foreign_keys pragma never applied")
	}
	if !strings.Contains(err.Error(), "foreign_keys") {
		t.Fatalf("error = %v, want it to name the pragma", err)
	}
}

// growWAL writes about 2,000 frames to the WAL, more than SQLite's default
// autocheckpoint threshold of 1,000, then returns the file's size.
func growWAL(t *testing.T, s *Store, dbPath string) int64 {
	t.Helper()
	blob := strings.Repeat("x", 4096)
	for i := 0; i < 2000; i++ {
		if err := s.SetUIState("client:wal", map[string]string{"k": blob + string(rune('a'+i%26))}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	info, err := os.Stat(dbPath + "-wal")
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	return info.Size()
}

func walSize(t *testing.T, dbPath string) int64 {
	t.Helper()
	info, err := os.Stat(dbPath + "-wal")
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	return info.Size()
}

func TestTruncateCheckpointShrinksTheWALFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wal.sqlite")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer s.Close()

	before := growWAL(t, s, dbPath)
	if before == 0 {
		t.Fatal("WAL did not grow — the fixture proves nothing")
	}

	// PASSIVE is what the checkpointer runs: it recycles WAL pages so the
	// file stops growing, but it never shrinks it. Pinning that here is
	// the reason TruncateCheckpoint has to exist at all.
	if _, err := s.checkpointWAL(context.Background()); err != nil {
		t.Fatalf("passive checkpoint: %v", err)
	}
	afterPassive := walSize(t, dbPath)
	if afterPassive != before {
		t.Fatalf("passive checkpoint changed the WAL file size (%d -> %d); it is not supposed to", before, afterPassive)
	}

	res, err := s.TruncateCheckpoint()
	if err != nil {
		t.Fatalf("truncate checkpoint: %v", err)
	}
	if res.Busy {
		t.Fatalf("truncate checkpoint reported busy with no other reader: %+v", res)
	}
	afterTruncate := walSize(t, dbPath)
	if afterTruncate != 0 {
		t.Fatalf("WAL = %d bytes after TRUNCATE, want 0 (was %d)", afterTruncate, before)
	}
	t.Logf("WAL bytes: %d written -> %d after PASSIVE -> %d after TRUNCATE", before, afterPassive, afterTruncate)
}

func TestCloseTruncatesTheWAL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "close.sqlite")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	before := growWAL(t, s, dbPath)

	// Hold a second connection on the file for the whole of Close, so
	// SQLite's own last-connection-closes checkpoint cannot be what
	// removes the WAL. What's measured is Store.Close's checkpoint.
	guard, err := sql.Open("sqlite", poolDSN(dbPath, readerConnPragmas))
	if err != nil {
		t.Fatalf("open guard: %v", err)
	}
	defer guard.Close()
	guardConn, err := guard.Conn(context.Background())
	if err != nil {
		t.Fatalf("guard conn: %v", err)
	}
	defer guardConn.Close()
	var probe int
	if err := guardConn.QueryRowContext(context.Background(), "SELECT 1").Scan(&probe); err != nil {
		t.Fatalf("guard probe: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	after := walSize(t, dbPath)
	if after != 0 {
		t.Fatalf("WAL = %d bytes after Close, want 0 (was %d)", after, before)
	}
	t.Logf("WAL bytes across Close: %d -> %d", before, after)
}

// A hard-killed process leaves its WAL on disk at whatever high-water
// mark it reached — no Close ran, so nothing truncated it. The next boot
// has to be what reclaims it, which is the whole reason the Windows Job
// Object teardown does not have to be graceful for WAL size to recover.
func TestNewTruncatesAWALLeftByAnUngracefulExit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crash.sqlite")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	before := growWAL(t, s, dbPath)

	// Simulate the kill: drop the pools without Store.Close's
	// checkpoint, holding a guard connection so SQLite's own
	// last-connection checkpoint cannot stand in for it either.
	guard, err := sql.Open("sqlite", poolDSN(dbPath, readerConnPragmas))
	if err != nil {
		t.Fatalf("open guard: %v", err)
	}
	guardConn, err := guard.Conn(context.Background())
	if err != nil {
		t.Fatalf("guard conn: %v", err)
	}
	var probe int
	if err := guardConn.QueryRowContext(context.Background(), "SELECT 1").Scan(&probe); err != nil {
		t.Fatalf("guard probe: %v", err)
	}
	if s.read != nil {
		_ = s.read.Close()
	}
	_ = s.db.Close()

	stranded := walSize(t, dbPath)
	if stranded == 0 {
		t.Fatal("WAL was already reclaimed — the fixture is not simulating an ungraceful exit")
	}

	reopened, err := New(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	after := walSize(t, dbPath)
	if after != 0 {
		t.Fatalf("WAL = %d bytes after boot, want 0 (stranded at %d)", after, stranded)
	}
	t.Logf("WAL bytes across an ungraceful exit: %d written -> %d stranded -> %d after boot", before, stranded, after)

	_ = guardConn.Close()
	_ = guard.Close()
}
