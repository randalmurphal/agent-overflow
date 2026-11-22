package agy

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// cortexStepTypeUserInput is agy's CORTEX_STEP_TYPE_USER_INPUT enum value.
// Verified against live agy 1.5.14 conversations/<id>.db (`steps.step_type`).
const cortexStepTypeUserInput = 14

// CutKeepAll tells ForkConversation to keep every step in the cloned database.
// Any value >= -1 (including -1 from an empty pre-cut checkpoint) is treated as
// an explicit upper-bound step index (`DELETE FROM <table> WHERE idx > cutIdx`).
const CutKeepAll = math.MinInt

// ReadMaxStepIndex returns the highest step `idx` currently committed in
// `<appDataDir>/conversations/<sessionID>.db`, used to pin a live tail fork
// before AO cuts the visible timeline.
func ReadMaxStepIndex(appDataDir, sessionID string) (int, bool) {
	return readMaxStepIndex(appDataDir, sessionID)
}

// ReadForkCheckpoint captures the pre-cut native step ceiling for a tail fork.
// If the conversation DB does not exist yet on disk, it returns (-1, nil) so
// any steps written before VACUUM INTO are truncated rather than leaked.
// Transient lock contention or schema errors return a non-nil error so the
// caller does not silently fall back to an unbounded fork.
func ReadForkCheckpoint(appDataDir, sessionID string) (int, error) {
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return -1, nil
		}
		return 0, fmt.Errorf("stat agy conversation db: %w", err)
	}
	db, err := sql.Open("sqlite", sqliteFileDSN(dbPath, "mode=ro&_pragma=busy_timeout(5000)"))
	if err != nil {
		return 0, fmt.Errorf("open agy conversation db: %w", err)
	}
	defer db.Close()

	var maxIdx sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&maxIdx); err != nil {
		return 0, fmt.Errorf("query agy max step idx: %w", err)
	}
	if !maxIdx.Valid {
		return -1, nil
	}
	return int(maxIdx.Int64), nil
}

// ForkConversation clones `<appDataDir>/conversations/<srcSessionID>.db` into a
// new conversation UUID using SQLite VACUUM INTO (WAL-safe), verifies the schema
// before mutating, optionally slices steps/gen_metadata/executor_metadata to
// `idx <= cutIdx`, copies companion `<appDataDir>/brain/<srcSessionID>` files,
// and rewrites `trajectory_meta` to the new session ID.
//
// Pass CutKeepAll for a full tail fork (keeps every step). Pass -1 to truncate
// all steps (for example, an empty pre-cut checkpoint).
// Returns the newly minted session UUID and a cleanup function that removes the
// cloned DB file and brain directory if a later step in the caller fails.
func ForkConversation(appDataDir, srcSessionID string, cutIdx int) (newSessionID string, cleanup func() error, err error) {
	return forkConversation(appDataDir, srcSessionID, cutIdx, -1)
}

// ForkConversationAtUserTurn snapshots the source conversation DB FIRST via
// VACUUM INTO and resolves the 0-indexed user-prompt cutoff INSIDE the cloned
// snapshot in a single transaction, eliminating any race between cut lookup and
// snapshot. If beforeUserTurnIndex exceeds the snapshot's recorded user prompt
// count, an error is returned rather than silently keeping extra history.
func ForkConversationAtUserTurn(appDataDir, srcSessionID string, beforeUserTurnIndex int) (newSessionID string, cleanup func() error, err error) {
	if beforeUserTurnIndex <= 0 {
		return "", nil, fmt.Errorf("agy fork: beforeUserTurnIndex must be positive, got %d", beforeUserTurnIndex)
	}
	return forkConversation(appDataDir, srcSessionID, CutKeepAll, beforeUserTurnIndex)
}

func forkConversation(appDataDir, srcSessionID string, cutIdx int, cutBeforeUserTurn int) (newSessionID string, cleanup func() error, err error) {
	if strings.TrimSpace(appDataDir) == "" {
		return "", nil, errors.New("agy fork: appDataDir is required")
	}
	if strings.TrimSpace(srcSessionID) == "" {
		return "", nil, errors.New("agy fork: source session id is required")
	}

	srcPath, err := conversationDBPath(appDataDir, srcSessionID)
	if err != nil {
		return "", nil, fmt.Errorf("agy fork: %w", err)
	}
	srcDB, err := openConversationDBReadOnly(srcPath)
	if err != nil {
		return "", nil, fmt.Errorf("agy fork: open source conversation %s: %w", srcSessionID, err)
	}
	defer srcDB.Close()

	if err := verifyConversationSchema(srcDB); err != nil {
		return "", nil, fmt.Errorf("agy fork: %w", err)
	}

	newSessionID = uuid.NewString()
	dstPath, err := conversationDBPath(appDataDir, newSessionID)
	if err != nil {
		return "", nil, fmt.Errorf("agy fork: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
		return "", nil, fmt.Errorf("agy fork: ensure conversations dir: %w", err)
	}

	// VACUUM INTO creates a clean, WAL-free snapshot safe even while the live
	// agy server holds WAL locks on the source conversation.
	if _, err := srcDB.Exec(`VACUUM INTO ?`, dstPath); err != nil {
		_ = os.Remove(dstPath)
		return "", nil, fmt.Errorf("agy fork: snapshot conversation %s: %w", srcSessionID, err)
	}

	dstBrainDir := filepath.Join(appDataDir, "brain", newSessionID)
	removeDst := func() error {
		var errs []error
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if rmErr := os.Remove(dstPath + suffix); rmErr != nil && !os.IsNotExist(rmErr) {
				errs = append(errs, rmErr)
			}
		}
		if rmErr := os.RemoveAll(dstBrainDir); rmErr != nil && !os.IsNotExist(rmErr) {
			errs = append(errs, rmErr)
		}
		return errors.Join(errs...)
	}

	if err := rewriteClonedConversationDB(dstPath, newSessionID, cutIdx, cutBeforeUserTurn); err != nil {
		_ = removeDst()
		return "", nil, err
	}

	if err := copyForkBrainArtifacts(appDataDir, srcSessionID, newSessionID); err != nil {
		_ = removeDst()
		return "", nil, err
	}

	return newSessionID, removeDst, nil
}

func rewriteClonedConversationDB(dstPath, newSessionID string, cutIdx int, cutBeforeUserTurn int) error {
	dstDB, err := sql.Open("sqlite", sqliteFileDSN(dstPath, "_pragma=busy_timeout(200)"))
	if err != nil {
		return fmt.Errorf("agy fork: open cloned conversation: %w", err)
	}
	defer dstDB.Close()

	tx, err := dstDB.Begin()
	if err != nil {
		return fmt.Errorf("agy fork: begin mutation tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if cutBeforeUserTurn > 0 {
		rows, err := tx.Query(`SELECT idx FROM steps WHERE step_type = ? ORDER BY idx ASC`, cortexStepTypeUserInput)
		if err != nil {
			return fmt.Errorf("agy fork: query cloned user steps: %w", err)
		}
		var userIdxs []int
		for rows.Next() {
			var idx int
			if scanErr := rows.Scan(&idx); scanErr != nil {
				_ = rows.Close()
				return scanErr
			}
			userIdxs = append(userIdxs, idx)
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		if cutBeforeUserTurn > len(userIdxs) {
			return fmt.Errorf("agy fork: requested cut at user prompt %d exceeds %d recorded native prompts", cutBeforeUserTurn, len(userIdxs))
		}
		if cutBeforeUserTurn < len(userIdxs) {
			targetIdx := userIdxs[cutBeforeUserTurn]
			if targetIdx <= 0 {
				return fmt.Errorf("agy fork: cut before first step leaves no prefix")
			}
			cutIdx = targetIdx - 1
		}
	}

	if cutIdx != CutKeepAll {
		for _, table := range []string{"steps", "gen_metadata", "executor_metadata", "parent_references", "battle_mode_infos"} {
			hasTable, checkErr := tableExists(tx, table)
			if checkErr != nil {
				return fmt.Errorf("agy fork: check table %s: %w", table, checkErr)
			}
			if !hasTable {
				continue
			}
			if _, delErr := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE idx > ?`, table), cutIdx); delErr != nil {
				return fmt.Errorf("agy fork: slice %s at idx %d: %w", table, cutIdx, delErr)
			}
		}
	}

	newTrajID := uuid.NewString()
	if _, err := tx.Exec(`UPDATE trajectory_meta SET trajectory_id = ?, cascade_id = ?`, newTrajID, newSessionID); err != nil {
		return fmt.Errorf("agy fork: rewrite trajectory_meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agy fork: commit cloned conversation: %w", err)
	}
	return nil
}

// copyForkBrainArtifacts copies regular companion files (plans, uploaded media,
// generated images) from `<appDataDir>/brain/<srcSessionID>` into
// `<appDataDir>/brain/<newSessionID>`, skipping symlinks and `.system_generated`
// runtime logs. Missing source brain directories are a no-op.
func copyForkBrainArtifacts(appDataDir, srcSessionID, newSessionID string) error {
	if !isSafeConversationID(srcSessionID) || !isSafeConversationID(newSessionID) {
		return nil
	}
	srcRoot := filepath.Join(appDataDir, "brain", srcSessionID)
	info, err := os.Lstat(srcRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("agy fork: stat source brain dir: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	dstRoot := filepath.Join(appDataDir, "brain", newSessionID)
	const maxCopyBytes = 128 << 20 // 128 MiB cap across companion artifacts
	var copiedBytes int64
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil || rel == "." {
			return nil
		}
		if d.IsDir() && rel == ".system_generated" {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		dstPath := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if copiedBytes+fi.Size() > maxCopyBytes {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			return err
		}
		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			_ = srcFile.Close()
			return err
		}
		n, copyErr := io.Copy(dstFile, io.LimitReader(srcFile, maxCopyBytes-copiedBytes))
		closeErr := dstFile.Close()
		_ = srcFile.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		copiedBytes += n
		return nil
	})
}

// FindCutIndexBeforeUserTurn returns the highest step `idx` strictly before the
// 0-indexed user turn `beforeUserTurnIndex` in `<appDataDir>/conversations/<sessionID>.db`.
// Returns (-1, false, nil) when beforeUserTurnIndex == 0 (no prefix to keep).
func FindCutIndexBeforeUserTurn(appDataDir, sessionID string, beforeUserTurnIndex int) (cutIdx int, hasPrefix bool, err error) {
	if beforeUserTurnIndex <= 0 {
		return -1, false, nil
	}
	dbPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return -1, false, fmt.Errorf("agy fork: %w", err)
	}
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return -1, false, fmt.Errorf("agy fork: open conversation %s: %w", sessionID, err)
	}
	defer db.Close()

	if err := verifyConversationSchema(db); err != nil {
		return -1, false, fmt.Errorf("agy fork: %w", err)
	}

	rows, err := db.Query(`SELECT idx FROM steps WHERE step_type = ? ORDER BY idx ASC`, cortexStepTypeUserInput)
	if err != nil {
		return -1, false, fmt.Errorf("agy fork: query user steps: %w", err)
	}
	defer rows.Close()

	var userIdxs []int
	for rows.Next() {
		var idx int
		if scanErr := rows.Scan(&idx); scanErr != nil {
			return -1, false, scanErr
		}
		userIdxs = append(userIdxs, idx)
	}
	if err := rows.Err(); err != nil {
		return -1, false, err
	}
	if beforeUserTurnIndex >= len(userIdxs) {
		// Target is past the last recorded user turn: keep everything.
		var maxIdx sql.NullInt64
		if err := db.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&maxIdx); err != nil {
			return -1, false, err
		}
		if !maxIdx.Valid {
			return -1, false, nil
		}
		return int(maxIdx.Int64), true, nil
	}
	targetIdx := userIdxs[beforeUserTurnIndex]
	if targetIdx <= 0 {
		return -1, false, nil
	}
	return targetIdx - 1, true, nil
}

// verifyConversationSchema refuses mutation when agy's on-disk schema no longer
// matches the verified 1.5.x layout (`trajectory_meta` + `steps.idx` + `steps.step_type`).
func verifyConversationSchema(db *sql.DB) error {
	requiredCols := map[string][]string{
		"trajectory_meta": {"trajectory_id", "cascade_id"},
		"steps":           {"idx", "step_type"},
	}
	for table, cols := range requiredCols {
		found := map[string]bool{}
		rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
		if err != nil {
			return fmt.Errorf("inspect schema for %s: %w", table, err)
		}
		for rows.Next() {
			var cid int
			var name, colType string
			var notNull, pk int
			var dflt sql.NullString
			if scanErr := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); scanErr == nil {
				found[name] = true
			}
		}
		_ = rows.Close()
		if len(found) == 0 {
			return fmt.Errorf("unsupported conversation schema: missing table %q", table)
		}
		for _, col := range cols {
			if !found[col] {
				return fmt.Errorf("unsupported conversation schema: table %q missing column %q", table, col)
			}
		}
	}
	return nil
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func tableExists(q queryRower, name string) (bool, error) {
	var found string
	err := q.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
