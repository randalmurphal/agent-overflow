package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// An assistant's own thread is never in a sidebar group: its row holds the
// threads it spawned the way a group holds members
// (docs/specs/standing-agents.md, "Sidebar"). A thread it spawned may be
// grouped like any other (ruling 36, thread_placement.go).

// ErrStandingAgentGrouped is what a move INTO a group reports for a
// standing agent.
var ErrStandingAgentGrouped = errors.New("store: a standing agent is not grouped; its row holds the threads it spawned")

// refuseAgentGroupingTx fails a move of threadID into a group when the
// thread is a standing agent.
func refuseAgentGroupingTx(tx *sql.Tx, threadID string) error {
	var agent bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = ?)`, threadID).Scan(&agent); err != nil {
		return fmt.Errorf("probe standing agent: %w", err)
	}
	if agent {
		return ErrStandingAgentGrouped
	}
	return nil
}

// ListSpawnedThreadIDs lists the live threads a thread spawned on this
// computer. Their rows' spawnedByAgent follows whether the spawner is an
// assistant's current thread, so the app re-emits them when that changes
// (creation, a handoff, a delete, a move).
func (s *Store) ListSpawnedThreadIDs(spawnerID string) ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT id FROM owned_threads WHERE archived = 0 AND id IN (
		SELECT target_thread_id FROM thread_requests
		 WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id = '')
		ORDER BY id`, spawnerID)
	if err != nil {
		return nil, fmt.Errorf("store: list threads spawned by %s: %w", spawnerID, err)
	}
	return ids, nil
}

// MaxArchivedChildrenPage bounds one page of a done footer's archived threads.
const MaxArchivedChildrenPage = 100

// ErrArchivedChildrenPage is what a page request outside the bounds reports.
var ErrArchivedChildrenPage = errors.New("store: an archived threads page names no container or asks for no rows or too many")

// ArchivedChildrenPage is one page of the archived threads one done footer
// of an assistant's rows holds.
type ArchivedChildrenPage struct {
	// Threads are newest activity first, ties by id descending.
	Threads []Thread `json:"threads"`
	// More is true when the footer holds threads past this page.
	More bool `json:"more"`
}

// ArchivedChildrenCursor is where a page starts: after the thread it names,
// in page order. The zero cursor starts at the newest.
type ArchivedChildrenCursor struct {
	UpdatedAt int64  `json:"updatedAt"`
	ID        string `json:"id"`
}

// ListArchivedSpawnedThreads lists one page of the archived threads a done
// footer of an assistant's rows holds (archivedAgentChildrenSQL): those
// filed under scopes, the empty scope for its card and a project id for its row in that
// project, read when the user opens the footer and as it shows more. An
// assistant it created is a row of its own, never a child.
func (s *Store) ListArchivedSpawnedThreads(spawnerID string, scopes []string, after ArchivedChildrenCursor, limit int) (page ArchivedChildrenPage, err error) {
	if len(scopes) == 0 || limit < 1 || limit > MaxArchivedChildrenPage {
		return ArchivedChildrenPage{}, ErrArchivedChildrenPage
	}
	action := fmt.Sprintf("store: list archived threads spawned by %s", spawnerID)
	scopeList, err := json.Marshal(scopes)
	if err != nil {
		return ArchivedChildrenPage{}, fmt.Errorf("%s: encode scopes: %w", action, err)
	}
	// One more than the page, to learn whether more follow.
	query := `SELECT ` + threadColumns + ` FROM owned_threads AS threads
		WHERE id IN (SELECT id FROM (` + archivedAgentChildrenSQL("?1") + `)
		              WHERE scope IN (SELECT value FROM json_each(?2)))`
	args := []any{spawnerID, string(scopeList)}
	if after.ID != "" {
		query += ` AND (updated_at < ?3 OR (updated_at = ?3 AND id < ?4))`
		args = append(args, after.UpdatedAt, after.ID)
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ` + strconv.Itoa(limit+1)
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return ArchivedChildrenPage{}, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	page.Threads = []Thread{}
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return ArchivedChildrenPage{}, fmt.Errorf("%s: scan: %w", action, err)
		}
		page.Threads = append(page.Threads, thread)
	}
	if err := rows.Err(); err != nil {
		return ArchivedChildrenPage{}, fmt.Errorf("%s: %w", action, err)
	}
	if len(page.Threads) > limit {
		page.Threads, page.More = page.Threads[:limit], true
	}
	return page, nil
}
