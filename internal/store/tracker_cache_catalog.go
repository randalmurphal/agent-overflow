package store

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
)

type mirrorUserRow struct {
	ID   string `json:"i"`
	Name string `json:"n"`
	Data string `json:"d"`
}

type mirrorStatusRow struct {
	ID    string `json:"i"`
	Name  string `json:"n"`
	Group int    `json:"g"`
}

// The catalog's set statements: ?1 root, ?2 the JSON list, ?3 when the
// read began. A row is written by a read that began later than its
// read_at, which moves read_at whether or not the content differs, or by
// other content from the same read; changed_at moves only with the
// content, so RETURNING reports a change. A status's done_changed_at moves
// when it enters or leaves the done and canceled groups, the one change of
// a status that moves rows between sections.
const (
	mirrorUsersUpsertSQL = `INSERT INTO tracker_cache_users(root_id, user_id, name, data, read_at, changed_at)
		SELECT ?1, u.value ->> 'i', u.value ->> 'n', unhex(u.value ->> 'd'), ?3, ?3 FROM json_each(?2) u WHERE true
		ON CONFLICT(root_id, user_id) DO UPDATE SET name = excluded.name, data = excluded.data, read_at = excluded.read_at,
		    changed_at = iif(name IS NOT excluded.name OR data IS NOT excluded.data, excluded.read_at, changed_at)
		WHERE excluded.read_at > read_at OR (excluded.read_at = read_at AND (name IS NOT excluded.name OR data IS NOT excluded.data))
		RETURNING user_id, changed_at = ?3`
	mirrorStatusesUpsertSQL = `INSERT INTO tracker_cache_statuses(root_id, status_id, name, status_group, read_at, changed_at, done_changed_at)
		SELECT ?1, st.value ->> 'i', st.value ->> 'n', st.value ->> 'g', ?3, ?3, iif(st.value ->> 'g' IN (4, 5), ?3, 0)
		  FROM json_each(?2) st WHERE true
		ON CONFLICT(root_id, status_id) DO UPDATE SET name = excluded.name, status_group = excluded.status_group,
		    read_at = excluded.read_at,
		    changed_at = iif(name IS NOT excluded.name OR status_group IS NOT excluded.status_group, excluded.read_at, changed_at),
		    done_changed_at = iif((status_group IN (4, 5)) IS NOT (excluded.status_group IN (4, 5)), excluded.read_at, done_changed_at)
		WHERE excluded.read_at > read_at
		   OR (excluded.read_at = read_at AND (name IS NOT excluded.name OR status_group IS NOT excluded.status_group))
		RETURNING status_id, changed_at = ?3, done_changed_at = ?3`
	// mirrorStatusGroupRevisionSQL advances, once each, the revision and
	// the totals revision of the boards with a row whose issue has a
	// status in ?2. A status moves between groups rarely, so this walks the
	// root's issues rather than keep an index by status that every card
	// write would maintain.
	mirrorStatusGroupRevisionSQL = `UPDATE tracker_cache_boards SET revision = revision + 1, totals_revision = totals_revision + 1
		WHERE root_id = ?1 AND board_id IN (
		SELECT b.board_id FROM tracker_cache_issues i
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.issue_id = i.issue_id
		 WHERE i.root_id = ?1 AND i.status_id IN (SELECT value FROM json_each(?2)))`
)

// MirrorCatalogWrite is what a write of a catalog changed: whether any
// user or status changed, and the threads whose row shows a status it
// renamed or regrouped (mirrorStatusLinkCopySQL), sorted.
type MirrorCatalogWrite struct {
	Changed bool
	Threads []string
}

// upsertMirrorCatalogTx upserts what a read that began at readAt names and
// reports what changed: one statement per kind it names for each
// mirrorCatalogChunk of them, one more when a status entered or left the
// done groups, which advances the revision of the boards whose rows have
// it, and one more when a status changed its name or group, which copies
// it to the links whose card has it (mirrorStatusLinkCopySQL).
func upsertMirrorCatalogTx(tx *sql.Tx, rootID int64, c MirrorCatalog, readAt int64) (MirrorCatalogWrite, error) {
	changed := false
	for chunk := range slices.Chunk(c.Users, mirrorCatalogChunk) {
		users := make([]mirrorUserRow, len(chunk))
		for i, u := range chunk {
			users[i] = mirrorUserRow{ID: u.ID, Name: u.Name, Data: hex.EncodeToString(u.Data)}
		}
		encoded, err := encodeMirrorJSON(users)
		if err != nil {
			return MirrorCatalogWrite{}, err
		}
		err = scanMirrorRows(tx, mirrorUsersUpsertSQL, []any{rootID, encoded, readAt}, func(r rowScanner) error {
			var id string
			var userChanged bool
			err := r.Scan(&id, &userChanged)
			changed = changed || userChanged
			return err
		})
		if err != nil {
			return MirrorCatalogWrite{}, fmt.Errorf("upsert users: %w", err)
		}
	}
	var moved, renamed []string
	for chunk := range slices.Chunk(c.Statuses, mirrorCatalogChunk) {
		statuses := make([]mirrorStatusRow, len(chunk))
		for i, st := range chunk {
			statuses[i] = mirrorStatusRow(st)
		}
		encoded, err := encodeMirrorJSON(statuses)
		if err != nil {
			return MirrorCatalogWrite{}, err
		}
		err = scanMirrorRows(tx, mirrorStatusesUpsertSQL, []any{rootID, encoded, readAt}, func(r rowScanner) error {
			var id string
			var statusChanged, doneChanged bool
			err := r.Scan(&id, &statusChanged, &doneChanged)
			changed = changed || statusChanged
			if statusChanged {
				renamed = append(renamed, id)
			}
			if doneChanged {
				moved = append(moved, id)
			}
			return err
		})
		if err != nil {
			return MirrorCatalogWrite{}, fmt.Errorf("upsert statuses: %w", err)
		}
	}
	out := MirrorCatalogWrite{Changed: changed}
	if len(moved) > 0 {
		list, err := encodeMirrorIDs(moved)
		if err != nil {
			return MirrorCatalogWrite{}, err
		}
		if _, err := tx.Exec(mirrorStatusGroupRevisionSQL, rootID, list); err != nil {
			return MirrorCatalogWrite{}, fmt.Errorf("advance the revision of boards with a regrouped status: %w", err)
		}
	}
	if len(renamed) > 0 {
		list, err := encodeMirrorIDs(renamed)
		if err != nil {
			return MirrorCatalogWrite{}, err
		}
		if out.Threads, err = queryIDs(tx, mirrorStatusLinkCopySQL, rootID, list); err != nil {
			return MirrorCatalogWrite{}, fmt.Errorf("copy the renamed statuses to their links: %w", err)
		}
		slices.Sort(out.Threads)
	}
	return out, nil
}
