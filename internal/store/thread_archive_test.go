package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// archiveStateOf reads a thread's archived flag and its unfinished-archive
// mark.
func archiveStateOf(t *testing.T, s *Store, id string) (archived, cascade bool) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT archived, archive_cascade FROM threads WHERE id = ?`, id).Scan(&archived, &cascade); err != nil {
		t.Fatalf("read the archive state of %s: %v", id, err)
	}
	return archived, cascade
}

func requireArchiveState(t *testing.T, s *Store, step, id string, archived, cascade bool) {
	t.Helper()
	if gotArchived, gotCascade := archiveStateOf(t, s, id); gotArchived != archived || gotCascade != cascade {
		t.Fatalf("%s: %s archived %v cascade %v, want %v %v", step, id, gotArchived, gotCascade, archived, cascade)
	}
}

// Every write of the archive flag marks an archive whose walk has not run,
// a request to archive an archived thread included, and every unarchive
// clears the mark; a worker its owner's walk finds already archived is left
// unmarked, because no walk of its own follows.
func TestEveryArchiveWriterMarksAnUnfinishedArchive(t *testing.T) {
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
				requireArchiveState(t, s, "archived", "t", true, true)
				if _, err := s.db.Exec(`UPDATE threads SET archive_cascade = 0 WHERE id = 't'`); err != nil {
					t.Fatal(err)
				}
				if changed, err := archive(s, "t"); err != nil || changed {
					t.Fatalf("archive again: %v, %v; want no change", changed, err)
				}
				requireArchiveState(t, s, "archived again", "t", true, true)
				if changed, err := unarchive(s, "t"); err != nil || !changed {
					t.Fatalf("unarchive: %v, %v", changed, err)
				}
				requireArchiveState(t, s, "unarchived", "t", false, false)
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
		if _, err := s.db.Exec(`UPDATE threads SET archive_cascade = 0 WHERE id = 'done'`); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := s.ArchiveOwnedWorker("worker", "owner"); err != nil || changed {
			t.Fatalf("a worker of an open owner: %v, %v; want no change", changed, err)
		}
		requireArchiveState(t, s, "owner open", "worker", false, false)
		if _, _, err := s.ArchiveThread("owner"); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := s.ArchiveOwnedWorker("worker", "owner"); err != nil || !changed {
			t.Fatalf("archive the worker: %v, %v", changed, err)
		}
		requireArchiveState(t, s, "worker archived", "worker", true, true)
		if _, changed, err := s.ArchiveOwnedWorker("done", "owner"); err != nil || changed {
			t.Fatalf("an archived worker: %v, %v; want no change", changed, err)
		}
		requireArchiveState(t, s, "archived worker", "done", true, false)
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
		requireArchiveState(t, s, "handed off", "old", true, true)
		requireArchiveState(t, s, "successor", "next", false, false)
	})
}

// A whole-row thread write from a stale struct cannot archive, unarchive
// or end an unfinished archive: the flag has one writer.
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
	requireArchiveState(t, s, "open after UpdateThread", "open", false, false)
	requireArchiveState(t, s, "closed after UpdateThread", "closed", true, true)
}

// A deleted thread a fork still reads is kept as a holder, unarchived, and
// with no archive left to finish.
func TestAHolderHasNoUnfinishedArchive(t *testing.T) {
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
	requireArchiveState(t, s, "holder", "S", false, false)
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
		if _, err := s.OweRemoteArchives(id, true); err != nil {
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

// unansweredArchiveSeq reads the newest archive sent for token that may
// still land, 0 when none, and whether token owes anything.
func unansweredArchiveSeq(t *testing.T, s *Store, token string) (int64, bool) {
	t.Helper()
	var seq int64
	err := s.db.QueryRow(`SELECT unanswered_archive_seq FROM thread_owner_ops WHERE token = ?`, token).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return seq, true
}

// An archive sent and not answered is withdrawn by every reopen after it,
// whatever replaced its row since: a supersede, an archive owed again and
// never attempted, a delete. The mark goes once the computer answers the
// operation owed above it.
func TestASentArchiveIsSupersededByEveryLaterReopen(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "owner")
	seedRemoteSpawn(t, s, "r", "owner", "pc", "w")
	archive := func() {
		t.Helper()
		if _, _, err := s.ArchiveThread("owner"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OweRemoteArchives("owner", true); err != nil {
			t.Fatal(err)
		}
	}
	reopen := func() OwnerOp {
		t.Helper()
		if _, _, err := s.UnarchiveThread("owner"); err != nil {
			t.Fatal(err)
		}
		return ownerOps(t, s)["r"]
	}
	archive()
	if marked, err := s.MarkOwnerOpSent("r", 1); err != nil || !marked {
		t.Fatalf("mark sent: %v, %v", marked, err)
	}
	if op := reopen(); op.Op != OwnerOpSupersede || op.Seq != 2 {
		t.Fatalf("the first reopen owes %+v, want a supersede at 2", op)
	}
	archive()
	if op := ownerOps(t, s)["r"]; op.Op != OwnerOpArchive || op.Seq != 3 || op.Sent {
		t.Fatalf("archived again: %+v, want archive 3 not attempted", op)
	}
	if seq, _ := unansweredArchiveSeq(t, s, "r"); seq != 1 {
		t.Fatalf("archive 1 may still land, but the row says %d", seq)
	}
	if op := reopen(); op.Op != OwnerOpSupersede || op.Seq != 4 {
		t.Fatalf("the second reopen owes %+v, want a supersede at 4", op)
	}
	// A supersede's own attempt leaves the archive's mark.
	if marked, err := s.MarkOwnerOpSent("r", 4); err != nil || !marked {
		t.Fatal(err)
	}
	if seq, _ := unansweredArchiveSeq(t, s, "r"); seq != 1 {
		t.Fatalf("a supersede's attempt moved the mark to %d", seq)
	}
	// An answer to archive 1 itself, replaced since, changes nothing.
	if settled, err := s.SettleOwnerOp("r", 1); err != nil || settled {
		t.Fatalf("settle archive 1: %v, %v", settled, err)
	}
	if seq, owed := unansweredArchiveSeq(t, s, "r"); !owed || seq != 1 {
		t.Fatalf("the mark after archive 1's answer = %d (owed %v)", seq, owed)
	}
	// A delete replacing the row keeps the mark.
	archive()
	commitDelete(t, s, "op", "owner")
	if op := ownerOps(t, s)["r"]; op.Op != OwnerOpDelete {
		t.Fatalf("the delete owes %+v", op)
	}
	if seq, _ := unansweredArchiveSeq(t, s, "r"); seq != 1 {
		t.Fatalf("the delete dropped the mark: %d", seq)
	}
	// The computer's answer to the owed operation ends it.
	op := ownerOps(t, s)["r"]
	if settled, err := s.SettleOwnerOp("r", op.Seq); err != nil || !settled {
		t.Fatalf("settle the delete: %v, %v", settled, err)
	}
	if _, owed := unansweredArchiveSeq(t, s, "r"); owed {
		t.Fatal("the answered delete is still owed")
	}
}

// A paired computer's archive of its worker here holds only through the
// link it names, is taken once per sequence, walks again when its sequence
// is sent again while the worker is still archived, and marks the worker's
// own archive unfinished whether or not it flipped the row.
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
		requireArchiveState(t, s, name, call.id, false, false)
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
	requireArchiveState(t, s, "archived", "worker", true, true)
	if seq := receiptLink(t, s, "tok").OwnerOpSeq; seq != 2 {
		t.Fatalf("the link's seq = %d, want 2", seq)
	}
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 1); err != nil || changed || applied {
		t.Fatalf("an older seq: %v, %v, %v; want nothing done", changed, applied, err)
	}
	// The same seq again, while the worker is archived under it, walks
	// again: its walk ended and is unfinished once more.
	if _, err := s.db.Exec(`UPDATE threads SET archive_cascade = 0 WHERE id = 'worker'`); err != nil {
		t.Fatal(err)
	}
	if row, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 2); err != nil || changed || !applied || !row.Archived {
		t.Fatalf("a replay of seq 2: %+v, %v, %v, %v; want its walk again", row, changed, applied, err)
	}
	requireArchiveState(t, s, "replayed", "worker", true, true)
	if seq := receiptLink(t, s, "tok").OwnerOpSeq; seq != 2 {
		t.Fatalf("the replay moved the link's seq to %d", seq)
	}
	if _, _, _, err := s.ArchiveLinkedWorker("worker", "tok", "other", 1); !errors.Is(err, ErrWorkerNotOwned) {
		t.Fatalf("another device's replay: %v, want ErrWorkerNotOwned", err)
	}

	// The archive's walk ended; the person reopened nothing, and the owner
	// archives again under a newer sequence: the archive is unfinished
	// again with no flip.
	if _, err := s.db.Exec(`UPDATE threads SET archive_cascade = 0 WHERE id = 'worker'`); err != nil {
		t.Fatal(err)
	}
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 3); err != nil || changed || !applied {
		t.Fatalf("archive again: %v, %v, %v; want applied with no flip", changed, applied, err)
	}
	requireArchiveState(t, s, "archived again", "worker", true, true)

	// A supersede that arrived first makes an older archive a replay.
	if _, _, err := s.UnarchiveThread("worker"); err != nil {
		t.Fatal(err)
	}
	// Reopened since, the same seq again changes nothing.
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 3); err != nil || changed || applied {
		t.Fatalf("a replay after a reopen: %v, %v, %v; want nothing done", changed, applied, err)
	}
	requireArchiveState(t, s, "a replay after a reopen", "worker", false, false)
	if found, err := s.SupersedeLink("tok", "dev", "lead-tok", 5); err != nil || !found {
		t.Fatalf("supersede: %v, %v", found, err)
	}
	if _, changed, applied, err := s.ArchiveLinkedWorker("worker", "tok", "dev", 4); err != nil || changed || applied {
		t.Fatalf("an archive older than the supersede: %v, %v, %v; want nothing done", changed, applied, err)
	}
	requireArchiveState(t, s, "after the superseded archive", "worker", false, false)
}

// The boot resumes the archives whose walk did not finish, of threads this
// computer still holds, through the partial index.
func TestListUnfinishedArchives(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"done", "unfinished", "deleting", "moved", "open"} {
		mustCreateThread(t, s, id)
	}
	for _, id := range []string{"done", "unfinished", "deleting", "moved"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.OweRemoteArchives("done", true); err != nil {
		t.Fatal(err)
	}
	seedTransfer(t, s, "moved", "outgoing", "move", "complete", 128)
	commitDelete(t, s, "op", "deleting")
	ids, err := s.ListUnfinishedArchives()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"unfinished"}) {
		t.Fatalf("unfinished archives = %v, want [unfinished]", ids)
	}
	if got := explainWakeCarrierPlan(t, s.db, unfinishedArchivesSQL); !strings.Contains(got, "idx_threads_archive_cascade") {
		t.Errorf("unfinished archives plan = %q, want idx_threads_archive_cascade", got)
	}
}
