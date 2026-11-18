package store

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// seedRemoteSpawn records a spawn caller made on computer, naming target
// there ("" for one whose acceptance never answered).
func seedRemoteSpawn(t *testing.T, s *Store, token, caller, computer, target string) {
	t.Helper()
	state := ThreadRequestRunning
	if target == "" {
		state = ThreadRequestUnconfirmed
	}
	if err := s.InsertThreadRequest(ThreadRequest{Token: token, CallerThreadID: caller, Kind: ThreadRequestSpawn,
		TargetThreadID: target, TargetComputerID: computer, State: state, CreatedAt: 100, UpdatedAt: 100}); err != nil {
		t.Fatalf("seed remote spawn %s: %v", token, err)
	}
}

// ownerOps reads every owed operation by token.
func ownerOps(t *testing.T, s *Store) map[string]OwnerOp {
	t.Helper()
	ops, err := s.DueOwnerOps(1<<62, 100)
	if err != nil {
		t.Fatal(err)
	}
	byToken := map[string]OwnerOp{}
	for _, op := range ops {
		byToken[op.Token] = op
	}
	return byToken
}

func ownerOpSeqOf(t *testing.T, s *Store, token string) int64 {
	t.Helper()
	var seq int64
	if err := s.db.QueryRow(`SELECT owner_op_seq FROM thread_requests WHERE token = ?`, token).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func commitDelete(t *testing.T, s *Store, op string, ids ...string) DeleteCommit {
	t.Helper()
	if _, err := s.ClaimDeleteFamily(op, ids); err != nil {
		t.Fatalf("claim %v: %v", ids, err)
	}
	commit, err := s.CommitDeleteClaim(op)
	if err != nil {
		t.Fatalf("commit %v: %v", ids, err)
	}
	return commit
}

// A delete's commit owes, in its transaction, a delete for every remote
// spawn of every thread it committed, the spawn whose acceptance never
// answered included, a release for those of a thread given to another
// computer, and nothing for a thread outside it.
func TestCommitDeleteClaimOwesTheRemoteLinksOfWhatItCommitted(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"lead", "worker", "bystander", "moved"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-worker", "lead", "worker", true)
	seedTransfer(t, s, "moved", "outgoing", "move", "complete", 128)
	seedRemoteSpawn(t, s, "r-lead", "lead", "pc-a", "w1")
	seedRemoteSpawn(t, s, "r-lost", "lead", "pc-a", "")
	seedRemoteSpawn(t, s, "r-worker", "worker", "pc-b", "w2")
	seedRemoteSpawn(t, s, "r-bystander", "bystander", "pc-a", "w3")
	seedRemoteSpawn(t, s, "r-moved", "moved", "pc-a", "w4")
	if _, err := s.db.Exec(`UPDATE thread_requests SET made_in_thread_id = 'past' WHERE token = 'r-lead'`); err != nil {
		t.Fatal(err)
	}

	commit := commitDelete(t, s, "op", "lead", "moved")
	want := map[string]OwnerOp{
		"r-lead":   {Token: "r-lead", ComputerID: "pc-a", SourceThreadID: "past", Op: OwnerOpDelete, Seq: 1},
		"r-lost":   {Token: "r-lost", ComputerID: "pc-a", SourceThreadID: "lead", Op: OwnerOpDelete, Seq: 1},
		"r-worker": {Token: "r-worker", ComputerID: "pc-b", SourceThreadID: "worker", Op: OwnerOpDelete, Seq: 1},
		"r-moved":  {Token: "r-moved", ComputerID: "pc-a", SourceThreadID: "moved", Op: OwnerOpRelease, Seq: 1},
	}
	got := ownerOps(t, s)
	if len(got) != len(want) {
		t.Fatalf("owed %v, want %v", got, want)
	}
	for token, op := range want {
		row := got[token]
		if row.Token != op.Token || row.ComputerID != op.ComputerID || row.SourceThreadID != op.SourceThreadID ||
			row.Op != op.Op || row.Seq != op.Seq || row.Sent || row.NextAt != row.OwedAt || row.OwedAt == 0 {
			t.Errorf("%s owed %+v, want %+v unsent and due at once", token, row, op)
		}
		if seq := ownerOpSeqOf(t, s, token); seq != op.Seq {
			t.Errorf("%s spawn row seq = %d, want %d", token, seq, op.Seq)
		}
	}
	var owed []string
	for _, op := range commit.OwedOps {
		owed = append(owed, op.Token)
	}
	slices.Sort(owed)
	if !slices.Equal(owed, []string{"r-lead", "r-lost", "r-moved", "r-worker"}) {
		t.Fatalf("the commit reported owing %v", owed)
	}
	if seq := ownerOpSeqOf(t, s, "r-bystander"); seq != 0 {
		t.Fatalf("a thread outside the delete took seq %d", seq)
	}

	// The owed rows outlive the spawn rows, which go with their caller.
	for _, id := range []string{"worker", "lead", "moved"} {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := ownerOps(t, s); len(got) != len(want) {
		t.Fatalf("after the destroy %d owed, want %d", len(got), len(want))
	}
}

// A commit that fails owes nothing and commits nothing.
func TestCommitDeleteClaimOwesNothingWhenItRollsBack(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedRemoteSpawn(t, s, "r-lead", "lead", "pc", "w")
	if _, err := s.ClaimDeleteFamily("op", []string{"lead"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_owe BEFORE INSERT ON thread_owner_ops BEGIN SELECT RAISE(ABORT, 'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitDeleteClaim("op"); err == nil {
		t.Fatal("the commit went ahead without its owed operation")
	}
	if _, committed := deleteClaimOf(t, s, "lead"); committed {
		t.Fatal("the thread was committed without its owed operation")
	}
	if seq := ownerOpSeqOf(t, s, "r-lead"); seq != 0 || len(ownerOps(t, s)) != 0 {
		t.Fatalf("a rolled back commit left seq %d and %d owed", seq, len(ownerOps(t, s)))
	}
}

// A delete replaces an archive owed for the same link, sent or not, under
// a new sequence; nothing replaces a delete; and the delivery writes act
// only on the sequence they name.
func TestADeleteOverridesAnOwedArchive(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedRemoteSpawn(t, s, "r", "lead", "pc", "w")
	if _, _, err := s.ArchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	if owed, err := s.OweRemoteArchives("lead"); err != nil || len(owed) != 1 || owed[0].Op != OwnerOpArchive || owed[0].Seq != 1 {
		t.Fatalf("owe archives = %+v, %v", owed, err)
	}
	if marked, err := s.MarkOwnerOpSent("r", 1); err != nil || !marked {
		t.Fatalf("mark sent: %v, %v", marked, err)
	}
	commitDelete(t, s, "op", "lead")
	op := ownerOps(t, s)["r"]
	if op.Op != OwnerOpDelete || op.Seq != 2 || op.Sent || op.Attempts != 0 {
		t.Fatalf("after the delete owed %+v, want an unsent delete at seq 2", op)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oweRemoteOpsTx(tx, []string{"lead"}, OwnerOpArchive, 5000); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if op := ownerOps(t, s)["r"]; op.Op != OwnerOpDelete || op.Seq != 2 {
		t.Fatalf("an archive replaced the owed delete: %+v", op)
	}
	if seq := ownerOpSeqOf(t, s, "r"); seq != 2 {
		t.Fatalf("an archive that owed nothing took seq %d", seq)
	}

	for name, write := range map[string]func() (bool, error){
		"settle":     func() (bool, error) { return s.SettleOwnerOp("r", 1) },
		"mark":       func() (bool, error) { return s.MarkOwnerOpSent("r", 1) },
		"reschedule": func() (bool, error) { return s.RescheduleOwnerOp("r", 1, 9_000_000) },
	} {
		if changed, err := write(); err != nil || changed {
			t.Errorf("%s of the older seq changed the row: %v, %v", name, changed, err)
		}
	}
	if got := ownerOps(t, s)["r"]; got != op {
		t.Fatalf("older-seq writes changed %+v to %+v", op, got)
	}
	if settled, err := s.SettleOwnerOp("r", 2); err != nil || !settled {
		t.Fatalf("settle seq 2: %v, %v", settled, err)
	}
	if len(ownerOps(t, s)) != 0 {
		t.Fatal("a settled op is still owed")
	}
}

// OweRemoteArchives owes an archive for the remote spawns of the owner and
// of the archived owned threads beneath it, not of an open worker or one
// with a sidebar row, and only while the owner is archived.
func TestOweRemoteArchives(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "archived", "open", "placed", "free"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "l-archived", "owner", "archived", true)
	spawnLocal(t, s, "l-open", "owner", "open", true)
	spawnLocal(t, s, "l-placed", "owner", "placed", false)
	for _, id := range []string{"owner", "archived", "placed"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, seed := range []struct{ token, caller string }{
		{"r-owner", "owner"}, {"r-archived", "archived"}, {"r-open", "open"}, {"r-placed", "placed"}, {"r-free", "free"},
	} {
		seedRemoteSpawn(t, s, seed.token, seed.caller, "pc", "w-"+seed.token)
	}

	if owed, err := s.OweRemoteArchives("free"); err != nil || len(owed) != 0 {
		t.Fatalf("an open owner owed %+v, %v", owed, err)
	}
	owed, err := s.OweRemoteArchives("owner")
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for _, op := range owed {
		if op.Op != OwnerOpArchive {
			t.Errorf("%s owed %s, want archive", op.Token, op.Op)
		}
		tokens = append(tokens, op.Token)
	}
	slices.Sort(tokens)
	if !slices.Equal(tokens, []string{"r-archived", "r-owner"}) {
		t.Fatalf("owed archives for %v, want [r-archived r-owner]", tokens)
	}
}

// The due list reads the owed rows by their next attempt, oldest first, a
// page at a time, and the delivery writes move one along.
func TestOwnerOpsDueList(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	for _, token := range []string{"a", "b", "c"} {
		seedRemoteSpawn(t, s, token, "lead", "pc", "w-"+token)
	}
	if _, _, err := s.ArchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OweRemoteArchives("lead"); err != nil {
		t.Fatal(err)
	}
	now := ownerOps(t, s)["a"].NextAt
	for token, next := range map[string]int64{"a": now + 30, "b": now + 10, "c": now + 20} {
		if moved, err := s.RescheduleOwnerOp(token, 1, next); err != nil || !moved {
			t.Fatalf("reschedule %s: %v, %v", token, moved, err)
		}
	}
	due, err := s.DueOwnerOps(now+25, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{due[0].Token, due[1].Token}; len(due) != 2 || !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("due = %+v, want b then c", due)
	}
	if page, err := s.DueOwnerOps(now+100, 1); err != nil || len(page) != 1 || page[0].Token != "b" {
		t.Fatalf("a page of one = %+v, %v", page, err)
	}
	if _, err := s.DueOwnerOps(now, 0); err == nil {
		t.Fatal("an empty page was read")
	}
	if marked, err := s.MarkOwnerOpSent("b", 1); err != nil || !marked {
		t.Fatal(marked, err)
	}
	if b := ownerOps(t, s)["b"]; !b.Sent || b.Attempts != 1 {
		t.Fatalf("b after a send = %+v", b)
	}
}

// The schedule reports the earliest owed operation, and nothing once none
// is owed.
func TestNextThreadRequestWorkReportsOwnerOps(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedRemoteSpawn(t, s, "r", "lead", "pc", "w")
	if schedule, err := s.NextThreadRequestWork(nil); err != nil || schedule.OwnerOp.Set {
		t.Fatalf("schedule with nothing owed = %+v, %v", schedule.OwnerOp, err)
	}
	if _, _, err := s.ArchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OweRemoteArchives("lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RescheduleOwnerOp("r", 1, 4242); err != nil {
		t.Fatal(err)
	}
	if schedule, err := s.NextThreadRequestWork(nil); err != nil || schedule.OwnerOp != (ScheduledMoment{At: 4242, Set: true}) {
		t.Fatalf("schedule = %+v, %v; want the owed op at 4242", schedule.OwnerOp, err)
	}
}

// Forgetting a computer drops what this computer owes it, and only that.
func TestForgettingAComputerDropsItsOwedOps(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedRemoteSpawn(t, s, "gone", "lead", "pc-gone", "w1")
	seedRemoteSpawn(t, s, "kept", "lead", "pc-kept", "w2")
	if _, _, err := s.ArchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OweRemoteArchives("lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForgetRemoteSpawnsOnComputer("pc-gone"); err != nil {
		t.Fatal(err)
	}
	ops := ownerOps(t, s)
	if _, ok := ops["gone"]; ok || len(ops) != 1 {
		t.Fatalf("owed after forgetting pc-gone: %+v", ops)
	}
}

// RemoteSpawnLinkTo finds the spawn that named a thread on a computer, its
// lineage included, and nothing for another computer's thread of that id.
func TestRemoteSpawnLinkTo(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedRemoteSpawn(t, s, "r", "lead", "pc", "w")
	if _, err := s.db.Exec(`UPDATE thread_requests SET lineage_only = 1, state = 'finished' WHERE token = 'r'`); err != nil {
		t.Fatal(err)
	}
	row, found, err := s.RemoteSpawnLinkTo("pc", "w")
	if err != nil || !found || row.Token != "r" || row.CallerThreadID != "lead" {
		t.Fatalf("RemoteSpawnLinkTo = %+v, %v, %v", row, found, err)
	}
	if _, found, err := s.RemoteSpawnLinkTo("other", "w"); err != nil || found {
		t.Fatalf("another computer's thread found: %v, %v", found, err)
	}
}

// What this computer owes another is a promise outside the history a
// snapshot describes, so a restore keeps it.
func TestRestoreKeepsOwedOwnerOps(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	mustCreateThread(t, s, "lead")
	seedRemoteSpawn(t, s, "r", "lead", "pc", "w")
	if _, _, err := s.ArchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OweRemoteArchives("lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := ownerOps(t, s)["r"]; !ok {
		t.Fatal("a restore dropped an owed operation")
	}
}

// The reads of v158's indexes use them.
func TestRemoteOwnershipReadsUseTheirIndexes(t *testing.T) {
	s := newTestStore(t)
	for name, plan := range map[string]struct {
		query, index string
		args         []any
	}{
		"due list":        {dueOwnerOpsSQL(10), "idx_thread_owner_ops_due", []any{1}},
		"owner op moment": {`SELECT MIN(next_at) FROM thread_owner_ops`, "idx_thread_owner_ops_due", nil},
		"forget":          {forgetOwnerOpsSQL, "idx_thread_owner_ops_computer", []any{"pc"}},
		"remote spawn":    {remoteSpawnLinkToSQL, "idx_thread_requests_remote_spawn_target", []any{"pc", "w"}},
		"remote owner":    {spawnOwnerRemoteSQL, "idx_thread_request_receipts_spawn_link", []any{"w"}},
		"spawned on":      {`SELECT ` + spawnedByComputerSQL("?"), "idx_thread_request_receipts_spawn_link", []any{"w"}},
		"spawn link":      {`SELECT ` + spawnLinkSQL, "idx_thread_request_receipts_spawn_link", []any{"w"}},
		"stale links":     {`SELECT 1 FROM thread_request_receipts WHERE ` + staleSpawnLinkSQL, "idx_thread_request_receipts_owner_seen", []any{1, 1}},
	} {
		if got := explainWakeCarrierPlan(t, s.db, plan.query, plan.args...); !strings.Contains(got, plan.index) {
			t.Errorf("%s plan = %q, want %s", name, got, plan.index)
		}
	}
}
