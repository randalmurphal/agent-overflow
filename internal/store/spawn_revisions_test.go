package store

import (
	"path/filepath"
	"testing"
)

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

func TestEndedRemoteSpawnKeepsHistoryAndOwesNoOwnerOperation(t *testing.T) {
	for _, lineage := range []bool{false, true} {
		s := newTestStore(t)
		mustCreateThread(t, s, "caller")
		if err := s.InsertThreadRequest(ThreadRequest{Token: "link", CallerThreadID: "caller", Kind: ThreadRequestSpawn, TargetComputerID: "desk", TargetThreadID: "worker", State: ThreadRequestRunning}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleThreadRequest("link", ThreadRequestOpenStates(), ThreadRequestSettlement{State: ThreadRequestFinished, AnswerKind: ThreadAnswerFinal, Answer: []byte("completed")}); err != nil {
			t.Fatal(err)
		}
		if lineage {
			mustExec(t, s.db, `UPDATE thread_requests SET lineage_only=1, answer=NULL WHERE token='link'`)
		}
		if _, err := s.ApplySpawnRevision("desk", SpawnRevision{SpawnRevisionAck: SpawnRevisionAck{Token: "link", Revision: 1}, End: "gone"}); err != nil {
			t.Fatal(err)
		}
		tokens, err := s.ListRemoteSpawnTokens([]string{"caller"})
		if len(tokens) != 0 || err != nil {
			t.Fatalf("ended spawn still offered to delete: %v, %v", tokens, err)
		}
		if _, _, err := s.ArchiveThread("caller"); err != nil {
			t.Fatal(err)
		}
		ops, err := s.OweRemoteArchives("caller")
		if err != nil || len(ops) != 0 {
			t.Fatalf("archive owes %+v, %v", ops, err)
		}
		var target string
		var answer []byte
		if err := s.db.QueryRow(`SELECT target_thread_id, answer FROM thread_requests WHERE token='link'`).Scan(&target, &answer); err != nil || target != "worker" || (!lineage && string(answer) != "completed") {
			t.Fatalf("history became %q, %q, %v", target, answer, err)
		}
	}
}

func TestRestoreKeepsOwedSpawnRevisionsAndTheirSequence(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"pending", "collected", "next"} {
		mustCreateThread(t, s, id)
	}
	path := filepath.Join(t.TempDir(), "history.db")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	accept := func(id string) SpawnRevision {
		t.Helper()
		acceptLink(t, s, id, id, id, 1)
		rows, err := s.PendingSpawnRevisions(id, "", 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("accepted %s: %+v, %v", id, rows, err)
		}
		return rows[0]
	}
	pending := accept("pending")
	collected := accept("collected")
	if err := s.AcknowledgeSpawnRevisions("collected", []SpawnRevisionAck{collected.SpawnRevisionAck}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	rows, err := s.PendingSpawnRevisions("pending", "", 1)
	if err != nil || len(rows) != 1 || rows[0].SpawnRevisionAck != pending.SpawnRevisionAck {
		t.Errorf("owed after restore = %+v, %v, want %+v", rows, err, pending)
	}
	if next := accept("next"); next.Revision <= collected.Revision {
		t.Errorf("after restore issued %d, previously collected %d", next.Revision, collected.Revision)
	}
}
