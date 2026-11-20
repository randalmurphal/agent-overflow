package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SpawnOwner is the thread that answers a worker's questions and approvals
// now: a thread on this computer, or one on a paired computer, named by the
// link that computer's spawn made here.
type SpawnOwner struct {
	// ThreadID and Title are the local spawner, or the link's source thread
	// and the title its spawn carried.
	ThreadID, Title string
	// ComputerID, ComputerName, DeviceID and Token name a paired
	// computer's link: the source computer, the device it authenticated as
	// and the spawn's token. All empty for a local owner.
	ComputerID, ComputerName, DeviceID, Token string
}

// SpawnOwnerOf names the thread that answers threadID's prompts now
// (docs/specs/standing-agents.md, "Owned workers"). An owned worker's are
// its spawner's always, whoever started the run it asks in; the spawner
// takes them when it can (a paused assistant once it is unpaused). A worker
// with a sidebar row asks its spawner only in a run the spawner started: its
// spawn's, or a later send's of the same thread, from when it was made until
// the worker is at rest after it; a spawn that tracks none, made before runs
// were kept, reads open until it settles (ownRunOpenSQL). In any other run
// of such a worker, and while its spawner lives under a paused assistant
// (assistants_paused.go), the prompt is the person's, as for a thread nobody
// spawned. An open prompt keeps the worker from rest, so the run it was
// asked in stays open until it is answered.
//
// A local owner is the caller of its spawn link (spawnerOfSQL) while that
// caller is not archived. Any thread can be the spawner, an assistant, an
// ordinary thread or another worker. A handoff moves its predecessors'
// spawn rows to the current thread, so a worker an earlier thread of an
// assistant spawned names the current one. A worker past the request
// ledger's retention still names its spawner: the sweep keeps its spawn row
// as lineage (spawnLineageSQL), so its prompts keep routing there. A thread
// no local spawn reaches asks a paired computer's spawning thread by the
// same rule (spawnOwnerRemoteSQL): always while it is owned through that
// spawn's link, and with a sidebar row in a run that thread started; only
// that computer knows whether its thread can take a prompt now. Found is
// false for any other thread and for one whose local spawner is archived.
func (s *Store) SpawnOwnerOf(threadID string) (SpawnOwner, bool, error) {
	var owner SpawnOwner
	var archived bool
	err := s.reader().QueryRow(`SELECT t.id, t.title, t.archived FROM thread_requests AS link
		CROSS JOIN threads t ON t.id = link.caller_thread_id CROSS JOIN threads w ON w.id = link.target_thread_id
		WHERE `+agentSpawnLinkSQL("link")+` AND link.target_thread_id = ?
		  AND (w.placement = 'owned' OR (`+spawnActiveSQL("link")+` AND `+s.assistantPauseSQL("t.id")+`))`, threadID).Scan(&owner.ThreadID, &owner.Title, &archived)
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

// spawnOwnerRemoteSQL reads the paired computer's spawn whose thread
// answers the prompts of thread ?1, by the rule a local spawn does: its
// link while ?1 is owned, and, whatever ?1's placement, any spawn of a
// device that still holds an unrevoked session here, as a link needs,
// while a run of its source thread is open on ?1 (spawnRunOpenSQL). A
// spawn made with a sidebar row is no link and tracks nothing else.
var spawnOwnerRemoteSQL = `SELECT rlink.source_thread_id, rlink.source_thread_title, rlink.source_computer_id,
	    rlink.source_computer_name, rlink.owner_device_id, rlink.token
	  FROM thread_request_receipts AS rlink INDEXED BY idx_thread_request_receipts_target
	 CROSS JOIN threads ON threads.id = rlink.target_thread_id
	 WHERE rlink.target_thread_id = ?1 AND rlink.kind = 'spawn'
	   AND ((rlink.spawn_link = 1 AND threads.placement = 'owned')
	        OR (` + spawnRunOpenSQL("rlink") + ` AND EXISTS (SELECT 1 FROM sessions INDEXED BY idx_sessions_device
	             WHERE sessions.device_id = rlink.owner_device_id AND sessions.revoked_at IS NULL)))`

// RemotePromptHolder names the thread here that answers the prompts of the
// worker this computer's spawn token made on a paired computer, an owned
// worker there: the spawn's caller now (a handoff moves it), while that
// thread can hold a prompt: held here (owned_threads, so neither a moved
// thread's cache nor a thread a delete committed) and not archived. A paused
// assistant holds its workers' prompts until it is unpaused, as
// SpawnOwnerOf's local spawner does. Found is false for any other spawn and
// when its caller can hold no prompt.
func (s *Store) RemotePromptHolder(token string) (threadID, title string, found bool, err error) {
	err = s.reader().QueryRow(`SELECT t.id, t.title FROM thread_requests AS r
		JOIN owned_threads AS t ON t.id = r.caller_thread_id
		WHERE r.token = ? AND r.kind = 'spawn' AND r.target_computer_id <> '' AND t.archived = 0`, token).Scan(&threadID, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read the thread that answers the prompts of spawn %s: %w", token, err)
	}
	return threadID, title, true, nil
}
