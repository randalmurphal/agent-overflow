package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrEmptyThreadGroupName is returned when a create or rename supplies a
// name that is blank once trimmed. A nameless row is the one state the
// sidebar cannot render, so it is refused at the accessor rather than
// stored and papered over by every reader.
var ErrEmptyThreadGroupName = errors.New("store: thread group name is required")

// ErrThreadGroupGone is what a move INTO a group reports when the group
// resolves to no project: it was deleted (usually by a second client racing
// this move) or it belongs to another project. Both are the same refusal to
// the user, so they share one message.
var ErrThreadGroupGone = errors.New("store: that group no longer exists in this project")

// ErrThreadGone is what a group move reports when a named root thread
// matched nothing — it was deleted under the caller.
var ErrThreadGone = errors.New("store: that thread no longer exists")

// ErrThreadNotRoot is what a group move reports when a named id is a
// discussion child. A child travels with its root and is never grouped on
// its own: the sidebar reads group membership off top-level rows only, so
// a child's own group_id would be state nothing renders.
var ErrThreadNotRoot = errors.New("store: a discussion reply moves with its discussion")

// ErrThreadGrouped is what PinThread reports on a grouped row. The group
// carries the pin from then on ("one pin per visible row"), and the
// schema's CHECK would refuse the write regardless; the guard exists so the
// refusal reads as a rule rather than as a raw constraint failure.
var ErrThreadGrouped = errors.New("store: a grouped thread cannot be pinned")

// ThreadGroup is a named, collapsible sidebar row gathering threads of ONE
// project (migration v76; spec: docs/specs/sidebar-thread-groups.md).
//
// It is not a thread: it has a name, a pin, and nothing else of its own.
// Its status, activity, and sort position are its members' — the same
// bubbling a discussion parent does — so nothing here is derived or cached.
//
// PinnedAt / PinGroup are the thread pin fields verbatim, including their
// NULL semantics: NULL PinGroup on a pinned row is the front burner, and
// an unpinned row never retains a latent group (the schema's CHECK).
type ThreadGroup struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	PinnedAt  *int64 `json:"pinnedAt,omitempty"`
	PinGroup  *int   `json:"pinGroup,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// threadGroupColumns is the column list scanThreadGroup expects, in order.
// pinned_at / pin_group stay uncoalesced for the same reason the thread
// projection leaves them so: the NULL carries meaning.
const threadGroupColumns = `id, project_id, name, pinned_at, pin_group, created_at, updated_at`

func scanThreadGroup(scanner interface{ Scan(...any) error }) (ThreadGroup, error) {
	var g ThreadGroup
	var pinnedAt, pinGroup sql.NullInt64
	if err := scanner.Scan(
		&g.ID, &g.ProjectID, &g.Name, &pinnedAt, &pinGroup, &g.CreatedAt, &g.UpdatedAt,
	); err != nil {
		return ThreadGroup{}, err
	}
	if pinnedAt.Valid {
		v := pinnedAt.Int64
		g.PinnedAt = &v
	}
	if pinGroup.Valid {
		v := int(pinGroup.Int64)
		g.PinGroup = &v
	}
	return g, nil
}

// ListThreadGroups reads every group in the store. The sidebar loads it
// once beside ListThreads and buckets by project itself, so there is no
// per-project read: the table holds one row per user-created group, not
// one per thread.
func (s *Store) ListThreadGroups() ([]ThreadGroup, error) {
	rows, err := s.reader().Query(
		`SELECT ` + threadGroupColumns + ` FROM thread_groups ORDER BY project_id, created_at, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread groups: %w", err)
	}
	defer rows.Close()

	out := []ThreadGroup{}
	for rows.Next() {
		g, err := scanThreadGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetThreadGroup reads one group. Every mutator here reads the row back
// through it so the caller renders exactly what was written.
func (s *Store) GetThreadGroup(id string) (ThreadGroup, error) {
	row := s.reader().QueryRow(
		`SELECT `+threadGroupColumns+` FROM thread_groups WHERE id = ?`, id,
	)
	g, err := scanThreadGroup(row)
	if err != nil {
		return ThreadGroup{}, fmt.Errorf("store: get thread group %s: %w", id, err)
	}
	return g, nil
}

// CreateThreadGroup inserts an empty group in the named project. The id is
// minted here rather than taken from the caller: nothing outside this
// package has a reason to choose one, and the wire is not a place to
// accept a primary key from.
func (s *Store) CreateThreadGroup(projectID, name string) (ThreadGroup, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ThreadGroup{}, ErrEmptyThreadGroupName
	}
	now := nowMillis()
	group := ThreadGroup{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		Name:      trimmed,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := s.db.Exec(
		`INSERT INTO thread_groups (id, project_id, name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		group.ID, group.ProjectID, group.Name, group.CreatedAt, group.UpdatedAt,
	); err != nil {
		return ThreadGroup{}, fmt.Errorf("store: create thread group in project %s: %w", projectID, err)
	}
	return group, nil
}

// RenameThreadGroup overwrites the display name and advances updated_at:
// unlike a pin, a rename IS a change to the group itself.
func (s *Store) RenameThreadGroup(id, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ErrEmptyThreadGroupName
	}
	result, err := s.db.Exec(
		`UPDATE thread_groups SET name = ?, updated_at = ? WHERE id = ?`,
		trimmed, nowMillis(), id,
	)
	if err != nil {
		return fmt.Errorf("store: rename thread group %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: rename thread group %s", id))
}

// DeleteThreadGroup removes the group and ungroups its members — active
// and archived alike — through the FK's ON DELETE SET NULL. No thread is
// deleted, and no Go-side sweep runs: the cascade is the mechanism, and
// every writer connection carries foreign_keys=1 (dsn.go) so it fires.
func (s *Store) DeleteThreadGroup(id string) error {
	result, err := s.db.Exec(`DELETE FROM thread_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete thread group %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete thread group %s", id))
}

// PinThreadGroup places the group on the front burner, mirroring
// PinThread exactly — including the stamped pinned_at that is metadata
// only and does not order within a burner.
func (s *Store) PinThreadGroup(id string) error {
	now := nowMillis()
	return s.setThreadGroupPinnedAt(id, &now)
}

// UnpinThreadGroup clears both pin fields. An unpinned row never retains a
// latent group.
func (s *Store) UnpinThreadGroup(id string) error {
	return s.setThreadGroupPinnedAt(id, nil)
}

// SetThreadGroupPinGroup moves an already-pinned group between the exact
// two burners. The WHERE clause makes assigning a burner to an unpinned
// row impossible even for a future caller that skips prevalidation, the
// same guard SetThreadPinGroup carries.
func (s *Store) SetThreadGroupPinGroup(id string, group int) error {
	if group != PinGroupFront && group != PinGroupBack {
		return fmt.Errorf("%w: %d", ErrInvalidPinGroup, group)
	}
	result, err := s.db.Exec(
		`UPDATE thread_groups SET pin_group = ? WHERE id = ? AND pinned_at IS NOT NULL`,
		group, id,
	)
	if err != nil {
		return fmt.Errorf("store: update pin_group for thread group %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update pin_group for pinned thread group %s", id))
}

// setThreadGroupPinnedAt is the shared pin/unpin primitive. Like
// setThreadPinnedAt it deliberately does NOT touch updated_at: pinning is
// a sidebar-presentation tweak, not a change to the group, and the row's
// clock is what an empty group sorts by.
func (s *Store) setThreadGroupPinnedAt(id string, ts *int64) error {
	var result sql.Result
	var err error
	if ts == nil {
		result, err = s.db.Exec(
			`UPDATE thread_groups SET pinned_at = NULL, pin_group = NULL WHERE id = ?`, id,
		)
	} else {
		result, err = s.db.Exec(
			`UPDATE thread_groups SET pinned_at = ?, pin_group = ? WHERE id = ?`,
			*ts, PinGroupFront, id,
		)
	}
	if err != nil {
		return fmt.Errorf("store: update pin state for thread group %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update pin state for thread group %s", id))
}

// SetThreadGroup is the ONE writer of threads.group_id. It moves each
// named thread — and, by the parent_thread_id disjunct, the discussion
// children that travel with it — into groupID, or out of any group when
// groupID is "".
//
// The rows it wrote are read back inside the same transaction and
// returned, so the caller renders exactly what landed rather than a
// second snapshot that a concurrent writer could have moved on from. That
// is also the only way the caller learns the CHILD ids: it named roots.
//
// Three properties are structural rather than prevalidated:
//
//   - The project subquery refuses a cross-project move. An unknown group
//     resolves to no project and matches nothing, so it fails the same way
//     (ErrThreadGroupGone).
//   - The root id must have been updated, or the whole call fails and
//     rolls back. A partial multi-select move is not a state the sidebar
//     could explain. Only a TOP-LEVEL row matches as a root (a child is
//     refused with ErrThreadNotRoot); a child named beside its own root is
//     still fine, because the root's disjunct carries it.
//   - Grouping strips the pin in the same statement ("one pin per visible
//     row"): the group carries the pin from then on, and the schema's CHECK
//     would refuse the row otherwise. Ungrouping touches ONLY group_id: a
//     grouped row holds no pin by that CHECK, and a bulk selection may name
//     ungrouped rows too, whose pins are theirs to keep.
func (s *Store) SetThreadGroup(threadIDs []string, groupID string) ([]Thread, error) {
	ids := uniqueNonEmptyStrings(threadIDs)
	if len(ids) == 0 {
		return []Thread{}, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin thread group move: %w", err)
	}
	defer tx.Rollback()

	const rootOrChild = `((id = ? AND COALESCE(parent_thread_id, '') = '') OR parent_thread_id = ?)`
	const groupSQL = `UPDATE threads
   SET group_id = ?, pinned_at = NULL, pin_group = NULL
 WHERE ` + rootOrChild + `
   AND project_id = (SELECT project_id FROM thread_groups WHERE id = ?)
 RETURNING id`
	const ungroupSQL = `UPDATE threads
   SET group_id = NULL
 WHERE ` + rootOrChild + `
 RETURNING id`

	named := make(map[string]bool, len(ids))
	for _, id := range ids {
		named[id] = true
	}
	touched := make([]string, 0, len(ids))
	for _, id := range ids {
		var (
			rows *sql.Rows
			err  error
		)
		if groupID == "" {
			rows, err = tx.Query(ungroupSQL, id, id)
		} else {
			rows, err = tx.Query(groupSQL, groupID, id, id, groupID)
		}
		if err != nil {
			return nil, fmt.Errorf("store: set thread group for %s: %w", id, err)
		}
		movedRoot := false
		for rows.Next() {
			var moved string
			if err := rows.Scan(&moved); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan moved thread for %s: %w", id, err)
			}
			if moved == id {
				movedRoot = true
			}
			touched = append(touched, moved)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("store: set thread group for %s: %w", id, err)
		}
		if !movedRoot {
			if err := threadGroupMoveRefusal(tx, id, named); err != nil {
				return nil, fmt.Errorf("store: set thread group for %s: %w", id, err)
			}
		}
	}

	// A child named beside its own root is carried by the root's disjunct,
	// possibly twice over, so the read-back set is deduped before it
	// becomes an IN list.
	moved, err := listThreadsByIDTx(tx, uniqueNonEmptyStrings(touched))
	if err != nil {
		return nil, fmt.Errorf("store: read back moved threads: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit thread group move: %w", err)
	}
	return moved, nil
}

// threadGroupMoveRefusal names why a named id matched nothing as a root,
// inside the move's own transaction. It runs only on that path, so the
// extra read costs nothing on a move that landed. A child whose root is
// ALSO named is not a refusal: the root's disjunct carries it.
func threadGroupMoveRefusal(tx *sql.Tx, threadID string, named map[string]bool) error {
	var parent string
	err := tx.QueryRow(
		`SELECT COALESCE(parent_thread_id, '') FROM threads WHERE id = ?`, threadID,
	).Scan(&parent)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrThreadGone
	case err != nil:
		return fmt.Errorf("probe thread: %w", err)
	case parent == "":
		return ErrThreadGroupGone
	case named[parent]:
		return nil
	}
	return ErrThreadNotRoot
}
