package store

import "fmt"

// subThreadLevelWalk bounds the walk up the owned lineage. The setting
// caps a level at four; the bound only has to stop a cycle the links could
// never form.
const subThreadLevelWalk = 16

// SubThreadLevel is threadID's sub-thread level: the number of owned spawn
// links between it and the nearest thread above it that is not an owned
// worker, plus the level a paired computer's spawn carried to that thread
// (threads.spawn_level; such a thread is standalone here, having no spawner
// here to own it). A thread the person talks to, one they placed in the
// sidebar and an assistant's own thread are level 0 unless a paired
// computer's owned spawn made them; a helper is level 1 under its assistant, as
// any owned worker is under its spawner (docs/specs/agent-thread-tools.md,
// "Sub-thread depth"). A thread that is not here is sql.ErrNoRows.
func (s *Store) SubThreadLevel(threadID string) (int, error) {
	var level int
	err := s.reader().QueryRow(`WITH RECURSIVE up(id, level, owned, carried) AS (
		  SELECT id, 0, placement = 'owned', spawn_level FROM threads WHERE id = ?1
		  UNION ALL
		  SELECT threads.id, up.level + 1, threads.placement = 'owned', threads.spawn_level
		    FROM up JOIN threads ON threads.id = `+spawnerOfSQL("up.id")+`
		   WHERE up.owned AND up.level < ?2)
		SELECT level + carried FROM up ORDER BY level DESC LIMIT 1`,
		threadID, subThreadLevelWalk).Scan(&level)
	if err != nil {
		return 0, fmt.Errorf("store: sub-thread level of %s: %w", threadID, err)
	}
	return level, nil
}
