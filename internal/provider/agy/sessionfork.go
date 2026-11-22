package agy

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// cortexStepTypeUserInput is agy's CORTEX_STEP_TYPE_USER_INPUT enum value.
// Verified in W6.B against agy 1.5.13 conversations/<id>.db.
const cortexStepTypeUserInput = 1

// ForkConversation clones `<appDataDir>/conversations/<srcSessionID>.db` into a
// new conversation UUID using SQLite VACUUM INTO (WAL-safe), verifies the schema
// before mutating, optionally slices steps/gen_metadata/executor_metadata to
// `idx <= cutIdx`, and rewrites `trajectory_meta` to the new session ID.
//
// Pass cutIdx < 0 for a full tail fork (keeps every step).
// Returns the newly minted session UUID and a cleanup function that removes the
// cloned DB file if a later step in the caller fails.
func ForkConversation(appDataDir, srcSessionID string, cutIdx int) (newSessionID string, cleanup func() error, err error) {
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

	removeDst := func() error {
		var errs []error
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if rmErr := os.Remove(dstPath + suffix); rmErr != nil && !os.IsNotExist(rmErr) {
				errs = append(errs, rmErr)
			}
		}
		return errors.Join(errs...)
	}

	if err := rewriteClonedConversationDB(dstPath, newSessionID, cutIdx); err != nil {
		_ = removeDst()
		return "", nil, err
	}

	return newSessionID, removeDst, nil
}

func rewriteClonedConversationDB(dstPath, newSessionID string, cutIdx int) error {
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

	if cutIdx >= 0 {
		for _, table := range []string{"steps", "gen_metadata", "executor_metadata"} {
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
