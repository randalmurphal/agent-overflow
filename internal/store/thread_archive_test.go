package store

import (
	"errors"
	"testing"
)

func requireArchiveState(t *testing.T, s *Store, step, id string, archived bool) {
	t.Helper()
	var got bool
	if err := s.db.QueryRow(`SELECT archived FROM threads WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("%s: read the archive flag of %s: %v", step, id, err)
	}
	if got != archived {
		t.Fatalf("%s: %s archived %v, want %v", step, id, got, archived)
	}
}

// Every archive writer flips the flag once: an archive of an archived
// thread, or of a worker whose owner is open, changes nothing.
func TestEveryArchiveWriterFlipsTheFlagOnce(t *testing.T) {
	archiveDoors := map[string]func(s *Store, id string) (bool, error){
		"ArchiveThread": func(s *Store, id string) (bool, error) {
			_, changed, err := s.ArchiveThread(id)
			return changed, err
		},
		"ApplyThreadOrganize": func(s *Store, id string) (bool, error) {
			archived, title := true, "renamed "+id
			result, err := s.ApplyThreadOrganize(id, ThreadOrganizeWrite{Archived: &archived, Title: &title})
			return result.ArchivedChanged, err
		},
	}
	unarchiveDoors := map[string]func(s *Store, id string) (bool, error){
		"UnarchiveThread": func(s *Store, id string) (bool, error) {
			_, changed, err := s.UnarchiveThread(id)
			return changed, err
		},
		"ApplyThreadOrganize": func(s *Store, id string) (bool, error) {
			archived := false
			result, err := s.ApplyThreadOrganize(id, ThreadOrganizeWrite{Archived: &archived})
			return result.ArchivedChanged, err
		},
	}
	for archiveName, archive := range archiveDoors {
		for unarchiveName, unarchive := range unarchiveDoors {
			t.Run(archiveName+"/"+unarchiveName, func(t *testing.T) {
				s := newTestStore(t)
				mustCreateThread(t, s, "t")
				if changed, err := archive(s, "t"); err != nil || !changed {
					t.Fatalf("archive: %v, %v", changed, err)
				}
				requireArchiveState(t, s, "archived", "t", true)
				if changed, err := archive(s, "t"); err != nil || changed {
					t.Fatalf("archive again: %v, %v; want no change", changed, err)
				}
				requireArchiveState(t, s, "archived again", "t", true)
				if changed, err := unarchive(s, "t"); err != nil || !changed {
					t.Fatalf("unarchive: %v, %v", changed, err)
				}
				requireArchiveState(t, s, "unarchived", "t", false)
			})
		}
	}

	t.Run("ArchiveOwnedWorker", func(t *testing.T) {
		s := newTestStore(t)
		for _, id := range []string{"owner", "worker", "done"} {
			mustCreateThread(t, s, id)
		}
		spawnLocal(t, s, "tok-worker", "owner", "worker", true)
		spawnLocal(t, s, "tok-done", "owner", "done", true)
		if _, _, err := s.ArchiveThread("done"); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := s.ArchiveOwnedWorker("worker", "owner"); err != nil || changed {
			t.Fatalf("a worker of an open owner: %v, %v; want no change", changed, err)
		}
		requireArchiveState(t, s, "owner open", "worker", false)
		if _, _, err := s.ArchiveThread("owner"); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := s.ArchiveOwnedWorker("worker", "owner"); err != nil || !changed {
			t.Fatalf("archive the worker: %v, %v", changed, err)
		}
		requireArchiveState(t, s, "worker archived", "worker", true)
		if _, changed, err := s.ArchiveOwnedWorker("done", "owner"); err != nil || changed {
			t.Fatalf("an archived worker: %v, %v; want no change", changed, err)
		}
		requireArchiveState(t, s, "archived worker", "done", true)
	})

	t.Run("HandOffAssistant", func(t *testing.T) {
		s := newTestStore(t)
		homeThread(t, s, "old", "")
		mustRetitle(t, s, "old", "Ops")
		if err := s.CreateStandingAgent(StandingAgent{ID: "a-ops", ThreadID: "old", Home: "/h/old", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		homeThread(t, s, "next", "")
		mustPlaceInHome(t, s, "next", "/h/old")
		handOff, err := s.HandOffAssistant("old", "next", 10)
		if err != nil {
			t.Fatal(err)
		}
		if !handOff.Old.Archived {
			t.Fatalf("the old thread = %+v, want it archived", handOff.Old)
		}
		requireArchiveState(t, s, "handed off", "old", true)
		requireArchiveState(t, s, "successor", "next", false)
	})
}

// A whole-row thread write from a stale struct cannot archive or
// unarchive: the flag has one writer.
func TestUpdateThreadLeavesTheArchiveAlone(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "open")
	mustCreateThread(t, s, "closed")
	if _, _, err := s.ArchiveThread("closed"); err != nil {
		t.Fatal(err)
	}
	for id, flip := range map[string]bool{"open": true, "closed": false} {
		row, err := s.GetThread(id)
		if err != nil {
			t.Fatal(err)
		}
		row.Archived = flip
		row.Title = "retitled"
		if err := s.UpdateThread(row); err != nil {
			t.Fatal(err)
		}
	}
	requireArchiveState(t, s, "open after UpdateThread", "open", false)
	requireArchiveState(t, s, "closed after UpdateThread", "closed", true)
}

// A deleted thread a fork still reads is kept as a holder, unarchived.
func TestAHolderIsNotArchived(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if _, _, err := s.ArchiveThread("S"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireHolder(t, s, "S", "Thread S")
	requireArchiveState(t, s, "holder", "S", false)
}

// Reopening a thread withdraws the archives owed for the paired computers'
// workers whose link it made: one not yet sent is dropped, one sent
// becomes a supersede under the link's next sequence, so the archive in
// flight lands as already applied. Links of the thread's own archived
// workers stay owed, and so does a delete.
func TestUnarchivingSupersedesTheArchivesOwedForItsLinks(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "worker", "other"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "l-worker", "owner", "worker", true)
	seedRemoteSpawn(t, s, "r-unsent", "owner", "pc", "w1")
	seedRemoteSpawn(t, s, "r-sent", "owner", "pc", "w2")
	seedRemoteSpawn(t, s, "r-deeper", "worker", "pc", "w3")
	seedRemoteSpawn(t, s, "r-other", "other", "pc", "w4")
	for _, id := range []string{"owner", "worker", "other"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"owner", "other"} {
		if _, err := s.OweRemoteArchives(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if marked, err := s.MarkOwnerOpSent("r-sent", 1); err != nil || !marked {
		t.Fatalf("mark sent: %v, %v", marked, err)
	}
	before := ownerOps(t, s)
	if len(before) != 4 {
		t.Fatalf("owed before the reopen = %+v", before)
	}

	if _, changed, err := s.UnarchiveThread("owner"); err != nil || !changed {
		t.Fatalf("unarchive: %v, %v", changed, err)
	}
	after := ownerOps(t, s)
	if _, owed := after["r-unsent"]; owed {
		t.Errorf("an unsent archive is still owed: %+v", after["r-unsent"])
	}
	if op := after["r-sent"]; op.Op != OwnerOpSupersede || op.Seq != 2 || op.Sent || op.Attempts != 0 || op.NextAt < before["r-sent"].NextAt {
		t.Errorf("the sent archive became %+v, want an unsent supersede at seq 2", op)
	}
	if seq := ownerOpSeqOf(t, s, "r-sent"); seq != 2 {
		t.Errorf("the superseded link's seq = %d, want 2", seq)
	}
	for _, token := range []string{"r-deeper", "r-other"} {
		if after[token] != before[token] {
			t.Errorf("%s changed from %+v to %+v", token, before[token], after[token])
		}
	}
	// An unarchive that changes nothing supersedes nothing.
	if _, changed, err := s.UnarchiveThread("owner"); err != nil || changed {
		t.Fatalf("unarchive again: %v, %v", changed, err)
	}
	if again := ownerOps(t, s); again["r-sent"] != after["r-sent"] {
		t.Errorf("a repeated unarchive moved %+v to %+v", after["r-sent"], again["r-sent"])
	}

	// A delete is never withdrawn.
	if _, _, err := s.ArchiveThread("other"); err != nil {
		t.Fatal(err)
	}
	commitDelete(t, s, "op", "other")
	owedDelete := ownerOps(t, s)["r-other"]
	if owedDelete.Op != OwnerOpDelete {
		t.Fatalf("the delete owed %+v", owedDelete)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := supersedeOwedArchivesTx(tx, "other", 9_000); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := ownerOps(t, s)["r-other"]; got != owedDelete {
		t.Errorf("the reopen changed an owed delete to %+v", got)
	}
}

// A paired computer's archive of its worker here holds only through the
// link it names, is taken once per sequence, walks again when its sequence
// is sent again while the worker is still archived.
func TestArchiveLinkedWorker(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"worker", "placed", "local", "spawner"} {
		mustCreateThread(t, s, id)
	}
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	acceptLink(t, s, "tok-placed", "dev", "placed", 1000)
	if _, err := s.SetThreadPlacement([]string{"placed"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	spawnLocal(t, s, "tok-local", "spawner", "local", true)

	for name, call := range map[string]struct{ id, token, device string }{
		"another device":   {"worker", "tok", "other"},
		"another token":    {"worker", "tok-placed", "dev"},
		"a sidebar thread": {"placed", "tok-placed", "dev"},
		"a local worker":   {"local", "tok-local", "device"},
	} {
		if _, _, _, err := s.ArchiveLinkedWorker(call.id, call.token, call.device, 1); !errors.Is(err, ErrWorkerNotOwned) {
			t.Errorf("%s: %v, want ErrWorkerNotOwned", name, err)
		}
		requireArchiveState(t, s, name, call.id, false)
	}
	if _, _, _, err := s.ArchiveLinkedWorker("missing", "tok", "dev", 1); !errors.Is(err, ErrWorkerNotOwned) {
		t.Errorf("a thread the link does not name: %v, want ErrWorkerNotOwned", err)
	}
	if receiptLink(t, s, "tok").OwnerOpSeq != 0 {
		t.Fatal("a refused archive took its sequence")
	}

	row, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 2)
	if err != nil || !changed || !applied || !row.Archived {
		t.Fatalf("archive: %+v, %v, %v, %v", row, changed, applied, err)
	}
	requireArchiveState(t, s, "archived", "worker", true)
	if seq := receiptLink(t, s, "tok").OwnerOpSeq; seq != 2 {
		t.Fatalf("the link's seq = %d, want 2", seq)
	}
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 1); err != nil || changed || applied {
		t.Fatalf("an older seq: %v, %v, %v; want nothing done", changed, applied, err)
	}
	// The same seq again, while the worker is archived under it, walks
	// again.
	if row, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 2); err != nil || changed || !applied || !row.Archived {
		t.Fatalf("a replay of seq 2: %+v, %v, %v, %v; want its walk again", row, changed, applied, err)
	}
	requireArchiveState(t, s, "replayed", "worker", true)
	if seq := receiptLink(t, s, "tok").OwnerOpSeq; seq != 2 {
		t.Fatalf("the replay moved the link's seq to %d", seq)
	}
	if _, _, _, err := s.ArchiveLinkedWorker("worker", "tok", "other", 1); !errors.Is(err, ErrWorkerNotOwned) {
		t.Fatalf("another device's replay: %v, want ErrWorkerNotOwned", err)
	}

	// The person reopened nothing, and the owner archives again under a
	// newer sequence: applied with no flip.
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 3); err != nil || changed || !applied {
		t.Fatalf("archive again: %v, %v, %v; want applied with no flip", changed, applied, err)
	}
	requireArchiveState(t, s, "archived again", "worker", true)

	// A supersede that arrived first makes an older archive a replay.
	if _, _, err := s.UnarchiveThread("worker"); err != nil {
		t.Fatal(err)
	}
	// Reopened since, the same seq again changes nothing.
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 3); err != nil || changed || applied {
		t.Fatalf("a replay after a reopen: %v, %v, %v; want nothing done", changed, applied, err)
	}
	requireArchiveState(t, s, "a replay after a reopen", "worker", false)
	if found, err := s.SupersedeLink("tok", "dev", "lead-tok", 5); err != nil || !found {
		t.Fatalf("supersede: %v, %v", found, err)
	}
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 4); err != nil || changed || applied {
		t.Fatalf("an archive older than the supersede: %v, %v, %v; want nothing done", changed, applied, err)
	}
	requireArchiveState(t, s, "after the superseded archive", "worker", false)
}
