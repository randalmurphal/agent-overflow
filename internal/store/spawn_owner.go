package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SpawnOwner is the thread that owns a worker, the one its questions and
// approvals go to: a thread on this computer, or one on a paired computer,
// named by the link that computer's spawn made here.
type SpawnOwner struct {
	// ThreadID and Title are the local spawner, or the link's source thread
	// and the title its spawn carried.
	ThreadID, Title string
	// ComputerID, ComputerName, DeviceID and Token name a paired
	// computer's link: the source computer, the device it authenticated as
	// and the spawn's token. All empty for a local owner.
	ComputerID, ComputerName, DeviceID, Token string
}

// SpawnOwnerOf names the owner of a thread on this computer. A local owner
// is the caller of its spawn link (spawnerOfSQL), while that caller is not
// archived. Any thread can be the spawner, an assistant, an ordinary thread
// or another worker. A handoff moves its predecessors' spawn rows to the
// current thread, so a worker an earlier thread of an assistant spawned
// names the current one. A worker past the request ledger's retention
// still names its spawner: the sweep keeps its spawn row as lineage
// (spawnLineageSQL), so its prompts keep routing there. While assistants
// are paused, a spawner that lives under one takes no prompt either
// (assistants_paused.go): the worker asks the user. A thread no local spawn
// reaches is owned remotely while it is owned and a paired computer's link
// reaches it; only that computer knows whether its thread can take a
// prompt. Found is false for any other thread and for one whose local
// spawner is archived.
func (s *Store) SpawnOwnerOf(threadID string) (SpawnOwner, bool, error) {
	var owner SpawnOwner
	var archived bool
	err := s.reader().QueryRow(`SELECT t.id, t.title, t.archived FROM threads t
		WHERE t.id = `+spawnerOfSQL("?")+` AND `+s.assistantPauseSQL("t.id"), threadID).Scan(&owner.ThreadID, &owner.Title, &archived)
	switch {
	case err == nil && archived:
		return SpawnOwner{}, false, nil
	case err == nil:
		return owner, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return SpawnOwner{}, false, fmt.Errorf("store: read the thread that spawned %s: %w", threadID, err)
	}
	err = s.reader().QueryRow(spawnOwnerRemoteSQL, threadID).Scan(
		&owner.ThreadID, &owner.Title, &owner.ComputerID, &owner.ComputerName, &owner.DeviceID, &owner.Token)
	if errors.Is(err, sql.ErrNoRows) {
		return SpawnOwner{}, false, nil
	}
	if err != nil {
		return SpawnOwner{}, false, fmt.Errorf("store: read the link that owns %s: %w", threadID, err)
	}
	return owner, true, nil
}

// spawnOwnerRemoteSQL reads the link that owns thread ?1.
const spawnOwnerRemoteSQL = `SELECT rlink.source_thread_id, rlink.source_thread_title, rlink.source_computer_id,
	    rlink.source_computer_name, rlink.owner_device_id, rlink.token
	  FROM thread_request_receipts AS rlink CROSS JOIN threads ON threads.id = rlink.target_thread_id
	 WHERE rlink.target_thread_id = ?1 AND rlink.spawn_link = 1 AND threads.placement = 'owned'`

// RemotePromptHolder names the thread here that answers the prompts of the
// worker this computer's spawn token made on a paired computer: the
// spawn's caller now (a handoff moves it), while that thread can take a
// prompt, the rule SpawnOwnerOf applies to a local spawner and more: held
// here (owned_threads, so neither a moved thread's cache nor a thread a
// delete committed), not archived, and not under an assistant while
// assistants are paused. Found is false for any other spawn and when its
// caller can take no prompt.
func (s *Store) RemotePromptHolder(token string) (threadID, title string, found bool, err error) {
	err = s.reader().QueryRow(`SELECT t.id, t.title FROM thread_requests AS r
		JOIN owned_threads AS t ON t.id = r.caller_thread_id
		WHERE r.token = ? AND r.kind = 'spawn' AND r.target_computer_id <> '' AND t.archived = 0 AND `+
		s.assistantPauseSQL("t.id"), token).Scan(&threadID, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read the thread that answers the prompts of spawn %s: %w", token, err)
	}
	return threadID, title, true, nil
}
