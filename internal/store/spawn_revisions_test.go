package store

import "testing"

func TestOwnerCanCollectSpawnChangesAcrossAcknowledgments(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "worker")
	acceptLink(t, s, "spawn", "owner", "worker", 1)
	read := func() SpawnRevision {
		t.Helper()
		rows, err := s.PendingSpawnRevisions("owner", "", 1)
		if err != nil || len(rows) != 1 || rows[0].Token != "spawn" {
			t.Fatalf("pending spawn changes = %+v, %v", rows, err)
		}
		return rows[0]
	}
	first := read()
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	standalone := read()
	if standalone.Revision <= first.Revision || standalone.Placement != PlacementStandalone {
		t.Fatalf("after placement change = %+v, previously %+v", standalone, first)
	}
	if err := s.AcknowledgeSpawnRevisions("owner", []SpawnRevisionAck{{Token: "spawn", Revision: first.Revision}}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != standalone {
		t.Fatalf("older acknowledgment changed pending revision %+v to %+v", standalone, got)
	}
	if err := s.AcknowledgeSpawnRevisions("owner", []SpawnRevisionAck{{Token: "spawn", Revision: standalone.Revision}}); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.PendingSpawnRevisions("owner", "", 1); err != nil || len(rows) != 0 {
		t.Fatalf("after acknowledgment = %+v, %v", rows, err)
	}
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	owned := read()
	if owned.Revision <= standalone.Revision || owned.Placement != PlacementOwned {
		t.Fatalf("after re-own = %+v, previously %+v", owned, standalone)
	}
	if err := s.DeleteThread("worker"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Revision <= owned.Revision || got.End != "gone" {
		t.Fatalf("after delete = %+v, previously %+v", got, owned)
	}
}
