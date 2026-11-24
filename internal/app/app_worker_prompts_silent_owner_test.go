package app

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// A spawner whose computer has stopped keeps its worker's prompt for as long
// as it has been silent for less than the retention floor, counted from its
// last contact with the link, and loses it to the person at the floor. The
// contact is the spawner's own read of the link (ReachSpawnLink), and the
// expiry pass reads the time it is handed, so both ends of the silence are
// set here rather than waited for.
func TestAPromptStaysWithASilentSpawnerUntilTheFloor(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkersAt(t, 1, 1)
	worker := f.workers[0].ID
	now := time.Now()
	seen := now.AddDate(0, 0, -store.ThreadRequestRetentionDays).Add(time.Minute)
	if _, found, err := f.app.store.ReachSpawnLink(f.spawns[0], f.device, f.lead, seen.UnixMilli()); err != nil || !found {
		t.Fatalf("the spawner's contact: found %v, %v", found, err)
	}
	f.ask(t, 0)

	f.app.expireThreadRequests(now)
	f.drain(t)
	if f.route(t, worker) == nil || f.handedToPerson(worker) {
		t.Fatal("the prompt left a spawner silent for less than the floor")
	}

	f.app.expireThreadRequests(now.Add(2 * time.Minute))
	f.drain(t)
	if f.route(t, worker) != nil || !f.handedToPerson(worker) {
		t.Fatal("the prompt stayed with a spawner silent past the floor")
	}
}
