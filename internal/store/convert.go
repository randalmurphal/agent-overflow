package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// File suffixes the conversion owns, both beside the live database.
//
// incrementalTmpSuffix holds the converted snapshot while it is being
// built and verified. asideSuffix holds the outgoing file for the few
// microseconds between the two renames, and is what makes a crash in
// that window recoverable: recoverInterruptedSwap puts it back.
const (
	incrementalTmpSuffix = ".incremental.tmp"
	asideSuffix          = ".replaced"
)

// Sidecar suffixes SQLite keeps beside a WAL database. Their absence is
// the proof the swap needs: the last connection to close a WAL database
// deletes both, so a -wal or -shm still on disk means something still
// holds the file open, in this process or another one.
var walSidecarSuffixes = []string{"-wal", "-shm"}

const (
	// swapAcquireTimeout bounds how long the swap waits for the writer
	// connection. Failing to get it means a long write is in flight,
	// which is not a quiet moment; the conversion step waits for the next one.
	swapAcquireTimeout = 2 * time.Second
	// swapSidecarWait bounds how long the swap waits for the -wal and
	// -shm files to disappear after it has discarded its own
	// connections. Closing them is synchronous, so this only ever
	// absorbs another process's connection closing at the same moment.
	swapSidecarWait = 100 * time.Millisecond
	swapSidecarPoll = 2 * time.Millisecond
)

// convertHooks are test seams in the conversion. Production leaves both
// nil; the tests that need a write to land at an exact point in the swap
// set them, because the two interesting outcomes are decided by when a
// commit arrives relative to the snapshot.
type convertHooks struct {
	// afterSnapshot runs once the snapshot is built and verified, before
	// the swap takes the writer connection.
	afterSnapshot func()
	// insideWindow runs once the writer connection is held, so work
	// started here blocks for the rest of the swap.
	insideWindow func()
}

// ConvertOutcome says what ConvertToIncrementalVacuum did. Only
// ConvertConverted changed the file.
type ConvertOutcome int

const (
	// ConvertNotQuiet means the database was written to between the
	// snapshot and the swap, or a write held the writer connection past
	// swapAcquireTimeout. The snapshot was discarded and nothing
	// changed. It is a normal result, not a failure: retry later.
	ConvertNotQuiet ConvertOutcome = iota
	// ConvertConverted means the file is now auto_vacuum=incremental.
	ConvertConverted
	// ConvertAlreadyIncremental means there was nothing to do.
	ConvertAlreadyIncremental
	// ConvertUnsupported means this database cannot be converted this
	// way: an in-memory database has no file, and a non-WAL mount has no
	// sidecar files to prove the swap is safe.
	ConvertUnsupported
)

// ConvertResult reports the outcome and, for a conversion that ran, the
// file size on either side and how long writes were blocked.
type ConvertResult struct {
	Outcome ConvertOutcome
	// SizeBefore and SizeAfter are the main database file's size in
	// bytes before and after the swap.
	SizeBefore int64
	SizeAfter  int64
	// BlockedWindow is how long writes and reads queued behind the swap.
	// It excludes building the snapshot, which runs with the database
	// fully available, and unlinking the outgoing file, which happens
	// after the swap is visible.
	BlockedWindow time.Duration
}

// ConvertToIncrementalVacuum rebuilds the database file as an
// auto_vacuum=incremental database so ReclaimFreeSpace can shrink it.
//
// auto_vacuum is fixed when a database's first table is created and can
// only be changed by rebuilding the file. The supported in-place rebuild
// is VACUUM, which holds the write lock for as long as it takes to
// rewrite the database (measured: 10-17 s on 4.4 GB) and grows the WAL
// by the size of the database. This does it as a snapshot swap instead:
//
//  1. VACUUM INTO a temporary file from a dedicated connection that has
//     auto_vacuum=incremental set, which readers and writers do not
//     notice, and give that file WAL mode.
//  2. Take the writer connection, quiesce and drain the read pool, and
//     confirm through PRAGMA data_version on the snapshot's connection
//     that nothing committed while the snapshot was being taken. VACUUM
//     INTO copies the database as of the start of its read transaction,
//     so a commit during it is a commit the snapshot does not have.
//  3. Close every connection to the old file, prove it by the absence of
//     its -wal and -shm, then rename the old file aside and the snapshot
//     into place. Both pools reopen lazily against the same path.
//
// The caller must hold no other connection to the database file; a
// CommitWatcher in particular has to be closed first, or step 3 refuses
// the swap.
//
// The old file is untouched until the rename, and a leftover snapshot or
// an interrupted rename is cleaned up by the next Store.New, so a crash
// at any step leaves a usable database.
//
// A ConvertNotQuiet result is a normal outcome and not an error.
func (s *Store) ConvertToIncrementalVacuum(ctx context.Context) (ConvertResult, error) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	if s.path == "" || s.path == ":memory:" || strings.Contains(s.path, "mode=memory") {
		return ConvertResult{Outcome: ConvertUnsupported}, nil
	}
	mode, err := s.AutoVacuumMode()
	if err != nil {
		return ConvertResult{}, err
	}
	if mode == AutoVacuumIncremental {
		return ConvertResult{Outcome: ConvertAlreadyIncremental}, nil
	}
	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return ConvertResult{}, fmt.Errorf("store: convert: probe journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return ConvertResult{Outcome: ConvertUnsupported}, nil
	}
	migrationVersion, err := currentMigrationVersion(s.db)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("store: convert: %w", err)
	}
	sizeBefore, err := fileSize(s.path)
	if err != nil {
		return ConvertResult{}, err
	}

	tmp := s.path + incrementalTmpSuffix
	if err := removeDatabaseFiles(tmp); err != nil {
		return ConvertResult{}, err
	}
	converted := false
	defer func() {
		if !converted {
			if err := removeDatabaseFiles(tmp); err != nil {
				logConvert("discard snapshot: %v", err)
			}
		}
	}()

	// The snapshot runs on its own connection: auto_vacuum is
	// connection-scoped until VACUUM INTO writes it into the copy, and
	// PRAGMA data_version only reports commits made by OTHER
	// connections, so it has to be read from a connection that is not
	// the writer. Opened outside the gate so the swap cannot block it.
	snapshot, err := sql.Open("sqlite", poolDSN(s.path, writerConnPragmas))
	if err != nil {
		return ConvertResult{}, fmt.Errorf("store: convert: open snapshot connection: %w", err)
	}
	snapshot.SetMaxOpenConns(1)
	snapshotClosed := false
	closeSnapshot := func() error {
		if snapshotClosed {
			return nil
		}
		snapshotClosed = true
		return snapshot.Close()
	}
	defer func() {
		if err := closeSnapshot(); err != nil {
			logConvert("close snapshot connection: %v", err)
		}
	}()

	conn, err := snapshot.Conn(ctx)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("store: convert: snapshot connection: %w", err)
	}
	connClosed := false
	closeConn := func() {
		if !connClosed {
			connClosed = true
			_ = conn.Close()
		}
	}
	defer closeConn()

	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		return ConvertResult{}, fmt.Errorf("store: convert: set auto_vacuum: %w", err)
	}
	dataVersionBefore, err := connDataVersion(ctx, conn)
	if err != nil {
		return ConvertResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return ConvertResult{}, fmt.Errorf("store: convert: vacuum into %s: %w", tmp, err)
	}
	if err := prepareConvertedSnapshot(tmp, s.path, migrationVersion); err != nil {
		return ConvertResult{}, err
	}
	if s.convertHooks.afterSnapshot != nil {
		s.convertHooks.afterSnapshot()
	}

	result := ConvertResult{Outcome: ConvertNotQuiet, SizeBefore: sizeBefore}
	err = s.quiesceReads(func() error {
		acquire, cancel := context.WithTimeout(ctx, swapAcquireTimeout)
		defer cancel()
		writer, err := s.db.Conn(acquire)
		if err != nil {
			// A write is holding the single writer connection. Not a
			// quiet moment; nothing has changed.
			return nil
		}
		blockedFrom := time.Now()
		releaseGate := s.gate.hold()
		if s.convertHooks.insideWindow != nil {
			s.convertHooks.insideWindow()
		}
		defer func() {
			result.BlockedWindow = time.Since(blockedFrom)
			releaseGate()
		}()
		writerLive := true
		defer func() {
			if writerLive {
				_ = writer.Close()
			}
		}()

		// Nothing can commit from here on: this holds the only writer
		// connection. Anything committed while the snapshot was being
		// taken is missing from it, so the snapshot is discarded.
		dataVersionNow, err := connDataVersion(ctx, conn)
		if err != nil {
			return err
		}
		if dataVersionNow != dataVersionBefore {
			return nil
		}
		closeConn()
		if err := closeSnapshot(); err != nil {
			return fmt.Errorf("store: convert: close snapshot connection: %w", err)
		}

		// Drop the read pool's connections first so the truncating
		// checkpoint below runs with the writer as the only connection
		// left and cannot report Busy.
		if s.read != nil {
			s.read.SetMaxIdleConns(0)
			defer s.read.SetMaxIdleConns(readPoolConns)
		}
		var checkpoint CheckpointResult
		var busy int64
		if err := writer.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").
			Scan(&busy, &checkpoint.WALFrames, &checkpoint.Checkpointed); err != nil {
			return fmt.Errorf("store: convert: truncate checkpoint: %w", err)
		}
		if busy != 0 {
			// Something still holds a read mark. Leave the file alone.
			return nil
		}

		// Retire the writer connection. Raw returning driver.ErrBadConn
		// makes database/sql close the physical connection and drop it
		// from the pool instead of handing it to the next caller; the
		// gate keeps the replacement from being opened until the new
		// file is in place.
		writerLive = false
		_ = writer.Raw(func(any) error { return driver.ErrBadConn })
		_ = writer.Close()

		if !waitSidecarsGone(s.path) {
			// A connection outside this store still holds the file.
			return nil
		}
		aside := s.path + asideSuffix
		if err := removeDatabaseFiles(aside); err != nil {
			return err
		}
		if err := os.Rename(s.path, aside); err != nil {
			return fmt.Errorf("store: convert: move outgoing database aside: %w", err)
		}
		if err := os.Rename(tmp, s.path); err != nil {
			// Put the live database back before anything can reopen.
			if restoreErr := os.Rename(aside, s.path); restoreErr != nil {
				return fmt.Errorf("store: convert: install converted database: %w (database left at %s: %v)", err, aside, restoreErr)
			}
			return fmt.Errorf("store: convert: install converted database: %w", err)
		}
		converted = true
		result.Outcome = ConvertConverted
		return nil
	})
	if err != nil {
		return ConvertResult{}, err
	}
	if result.Outcome != ConvertConverted {
		return result, nil
	}

	// Unlinking the outgoing file costs about as much as the swap did on
	// a multi-gigabyte database, so it happens here, with both pools
	// already serving the new file.
	if err := removeDatabaseFiles(s.path + asideSuffix); err != nil {
		logConvert("remove outgoing database: %v", err)
	}
	if size, err := fileSize(s.path); err != nil {
		logConvert("size converted database: %v", err)
	} else {
		result.SizeAfter = size
	}
	return result, nil
}

// convertToIncrementalVacuumStep is the last step of migration v119's
// deferred phase. A database created before incremental auto-vacuum keeps
// freed pages on its freelist for good; the step converts it
// (ConvertToIncrementalVacuum) so ReclaimFreeSpace can shrink it. A database
// that is already incremental, which is every database this build creates,
// finishes the step on its first check, and one that cannot be converted
// this way finishes it without converting.
//
// The swap briefly stops writes and a commit during the snapshot wastes it,
// so each attempt waits for the host to report a moment the user will not
// notice (DeferredHost.AwaitFileSwap). A snapshot a commit invalidated is
// discarded and the step waits for the next such moment.
func convertToIncrementalVacuumStep(ctx context.Context, s *Store, run *deferredRun) error {
	for ctx.Err() == nil {
		mode, err := s.AutoVacuumMode()
		if err != nil {
			return err
		}
		if mode == AutoVacuumIncremental {
			return nil
		}
		if err := run.awaitFileSwap(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait for a quiet moment: %w", err)
		}
		result, err := s.ConvertToIncrementalVacuum(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch result.Outcome {
		case ConvertConverted:
			log.Printf("store: database converted to incremental auto-vacuum; %d -> %d bytes, writes blocked %s",
				result.SizeBefore, result.SizeAfter, result.BlockedWindow.Round(time.Millisecond))
			return nil
		case ConvertAlreadyIncremental, ConvertUnsupported:
			return nil
		}
	}
	return nil
}

// prepareConvertedSnapshot gives the VACUUM INTO output the file-level
// properties the live database has and checks it is the database this
// store is running.
//
// VACUUM INTO writes a fresh file, so it comes out in the default
// rollback journal mode whatever the source was in. Setting WAL here and
// closing means the file that lands is already the database the pools
// expect, with its own sidecars deleted by the close.
func prepareConvertedSnapshot(tmp, livePath string, wantMigrationVersion int) error {
	db, err := sql.Open("sqlite", poolDSN(tmp, writerConnPragmas))
	if err != nil {
		return fmt.Errorf("store: convert: open snapshot: %w", err)
	}
	db.SetMaxOpenConns(1)
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()

	var mode int64
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return fmt.Errorf("store: convert: read snapshot auto_vacuum: %w", err)
	}
	if AutoVacuumMode(mode) != AutoVacuumIncremental {
		return fmt.Errorf("store: convert: snapshot is auto_vacuum=%s, want incremental", AutoVacuumMode(mode))
	}
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("store: convert: set snapshot journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("store: convert: snapshot journal_mode=%s, want wal", journalMode)
	}
	version, err := currentMigrationVersion(db)
	if err != nil {
		return fmt.Errorf("store: convert: read snapshot schema version: %w", err)
	}
	if version != wantMigrationVersion {
		return fmt.Errorf("store: convert: snapshot is at migration %d, live database is at %d", version, wantMigrationVersion)
	}
	if info, err := os.Stat(livePath); err == nil {
		if err := os.Chmod(tmp, info.Mode().Perm()); err != nil {
			return fmt.Errorf("store: convert: match snapshot permissions: %w", err)
		}
	}

	// Close here rather than on the way out: only the last connection
	// closing folds the snapshot's own WAL back into its file and
	// deletes the sidecars. Renaming a file whose WAL still held frames
	// would install a database missing its most recent pages.
	closed = true
	if err := db.Close(); err != nil {
		return fmt.Errorf("store: convert: close snapshot: %w", err)
	}
	if !waitSidecarsGone(tmp) {
		return fmt.Errorf("store: convert: snapshot %s still has WAL sidecars after close", tmp)
	}
	return nil
}

// connDataVersion reads PRAGMA data_version, which changes whenever
// another connection commits. It is only comparable against an earlier
// read from the SAME connection.
func connDataVersion(ctx context.Context, conn *sql.Conn) (int64, error) {
	var version int64
	if err := conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("store: read data_version: %w", err)
	}
	return version, nil
}

// waitSidecarsGone reports whether the database's -wal and -shm are
// gone, which is how the swap proves no connection holds the file.
// SQLite deletes both when the last connection closes; closing is
// synchronous, so the wait only absorbs another process closing at the
// same moment.
func waitSidecarsGone(dbPath string) bool {
	deadline := time.Now().Add(swapSidecarWait)
	for {
		remaining := false
		for _, suffix := range walSidecarSuffixes {
			if _, err := os.Stat(dbPath + suffix); err == nil {
				remaining = true
				break
			}
		}
		if !remaining {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(swapSidecarPoll)
	}
}

// recoverInterruptedSwap cleans up after a conversion that did not
// finish, before anything opens the database.
//
// A leftover snapshot is always stale, because the live database has
// been written to since; it is removed. An outgoing file beside a live
// database is the swap's own bookkeeping, likewise removed. An outgoing
// file with NO live database is the one interesting case: the process
// died between the two renames, and the file moved aside is the whole
// database.
func recoverInterruptedSwap(dbPath string) error {
	if dbPath == "" || dbPath == ":memory:" || strings.Contains(dbPath, "mode=memory") {
		return nil
	}
	aside := dbPath + asideSuffix
	if _, err := os.Stat(aside); err == nil {
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(aside, dbPath); err != nil {
				return fmt.Errorf("store: restore database interrupted mid-swap: %w", err)
			}
		} else if err := removeDatabaseFiles(aside); err != nil {
			return err
		}
	}
	return removeDatabaseFiles(dbPath + incrementalTmpSuffix)
}

// removeDatabaseFiles removes a database file and its sidecars. A file
// that is not there is not an error.
func removeDatabaseFiles(path string) error {
	for _, suffix := range append([]string{""}, walSidecarSuffixes...) {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: remove %s: %w", path+suffix, err)
		}
	}
	return nil
}

// logConvert reports a cleanup failure the conversion decided not to
// fail on: the swap either happened or did not, and a file left behind
// is picked up by the next Store.New.
func logConvert(format string, args ...any) {
	log.Printf("store: convert: "+format, args...)
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("store: stat %s: %w", path, err)
	}
	return info.Size(), nil
}

// CommitWatcher observes commits made by other connections through
// PRAGMA data_version, which only changes for a connection when another
// one commits. It owns a connection of its own for that reason, and
// because the value is comparable only against an earlier read from the
// same connection.
//
// Its connection holds the database file open, so a caller that is also
// converting the database must Close it before calling
// ConvertToIncrementalVacuum.
type CommitWatcher struct {
	db   *sql.DB
	conn *sql.Conn
}

// NewCommitWatcher opens the watcher's connection. Opened outside the
// connection gate so a swap cannot block it, and outside both pools so
// it never consumes a connection an accessor needs.
func (s *Store) NewCommitWatcher(ctx context.Context) (*CommitWatcher, error) {
	if s.path == "" {
		return nil, errors.New("store: commit watcher needs a file-backed database")
	}
	db, err := sql.Open("sqlite", poolDSN(s.path, readerConnPragmas))
	if err != nil {
		return nil, fmt.Errorf("store: open commit watcher: %w", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: commit watcher connection: %w", err)
	}
	return &CommitWatcher{db: db, conn: conn}, nil
}

// DataVersion returns the current value. Compare it against the
// previous call's: a different value means at least one commit landed in
// between.
func (w *CommitWatcher) DataVersion(ctx context.Context) (int64, error) {
	return connDataVersion(ctx, w.conn)
}

// Close releases the watcher's connection.
func (w *CommitWatcher) Close() error {
	if w == nil {
		return nil
	}
	connErr := w.conn.Close()
	dbErr := w.db.Close()
	return errors.Join(connErr, dbErr)
}
