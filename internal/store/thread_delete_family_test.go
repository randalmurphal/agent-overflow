package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// familyRow is what a test reads of a DeleteMember.
type familyRow struct {
	id, under string
	kind      DeleteMemberKind
	depth     int
}

func familyRows(t *testing.T, s *Store, ids ...string) []familyRow {
	t.Helper()
	members, err := s.ListDeleteFamily(ids)
	if err != nil {
		t.Fatal(err)
	}
	rows := []familyRow{}
	for _, member := range members {
		rows = append(rows, familyRow{member.ThreadID, member.Under, member.Kind, member.Depth})
	}
	return rows
}

// ListDeleteFamily lists what a delete takes, each thread once, a thread
// before the threads under it: the named thread, every owned thread down
// the lineage through archived ones with the owner that fences its claim,
// and for an assistant's current thread its past threads and helpers and
// their own workers. Not a placed spawn and not one whose link another
// thread holds. It sees a thread a delete claimed or committed, so a delete
// and the boot's resume of one read the same family.
func TestListDeleteFamily(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "other", "owned", "deep", "deeper", "placed", "archived", "moved", "marked", "past", "helper", "helper-work"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-owned", "owner", "owned", true)
	spawnLocal(t, s, "tok-deep", "owned", "deep", true)
	spawnLocal(t, s, "tok-deeper", "deep", "deeper", true)
	spawnLocal(t, s, "tok-placed", "owner", "placed", false)
	spawnLocal(t, s, "tok-archived", "owner", "archived", true)
	spawnLocal(t, s, "tok-moved", "owner", "moved", true)
	spawnLocal(t, s, "tok-marked", "owner", "marked", true)
	spawnLocal(t, s, "tok-helper-work", "helper", "helper-work", true)
	if _, err := s.db.Exec(`UPDATE thread_requests SET caller_thread_id = 'other' WHERE token = 'tok-moved'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"archived", "deep"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BeginThreadDelete("marked"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE threads SET worktree_path = '/wt/deeper' WHERE id = 'deeper'`); err != nil {
		t.Fatal(err)
	}
	members, err := s.ListDeleteFamily([]string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.ThreadID == "deeper" && (member.WorktreePath != "/wt/deeper" || member.ProjectID == "") {
			t.Errorf("deeper = %+v, want its project and worktree", member)
		}
		if member.Committed != (member.ThreadID == "marked") {
			t.Errorf("%s committed = %v", member.ThreadID, member.Committed)
		}
	}
	want := []familyRow{
		{"owner", "", DeleteNamed, 0},
		{"archived", "owner", DeleteWorker, 1},
		{"marked", "owner", DeleteWorker, 1},
		{"owned", "owner", DeleteWorker, 1},
		{"deep", "owned", DeleteWorker, 2},
		{"deeper", "deep", DeleteWorker, 3},
	}
	if got := familyRows(t, s, "owner"); !slices.Equal(got, want) {
		t.Errorf("owner's family = %v, want %v", got, want)
	}
	// A named thread under another named one lives under it.
	wantBoth := []familyRow{want[0], {"owned", "owner", DeleteNamed, 1}, want[1], want[2], want[4], want[5]}
	if got := familyRows(t, s, "owned", "owner"); !slices.Equal(got, wantBoth) {
		t.Errorf("the family of the owner and its worker = %v, want %v", got, wantBoth)
	}

	// The owner is an assistant's current thread: its past thread and its
	// helpers go with it, a helper it also spawned once, as a helper, and
	// the workers of a helper with no spawn link too.
	if _, err := s.db.Exec(`INSERT INTO agents (id, thread_id, home, created_at) VALUES ('agent', 'owner', '/home/agent', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO agent_helpers (thread_id, agent_id) VALUES ('owned', 'agent'), ('helper', 'agent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES ('past', 'agent', 5)`); err != nil {
		t.Fatal(err)
	}
	want = []familyRow{
		{"owner", "", DeleteNamed, 0},
		{"past", "owner", DeletePastThread, 1},
		{"helper", "owner", DeleteHelper, 1},
		{"owned", "owner", DeleteHelper, 1},
		{"archived", "owner", DeleteWorker, 1},
		{"marked", "owner", DeleteWorker, 1},
		{"deep", "owned", DeleteWorker, 2},
		{"helper-work", "helper", DeleteWorker, 2},
		{"deeper", "deep", DeleteWorker, 3},
	}
	if got := familyRows(t, s, "owner"); !slices.Equal(got, want) {
		t.Errorf("an assistant's family = %v, want %v", got, want)
	}
	if got := familyRows(t, s, "missing"); len(got) != 0 {
		t.Errorf("a missing thread's family = %v, want none", got)
	}
}

// A thread given to another computer and a holder are not reached as
// workers, and named, are taken alone: the workers a moved thread left
// here are not its delete's.
func TestADeleteFamilyStopsAtAThreadThatLeft(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "gone-away", "left-here", "copied", "copied-work"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-gone", "owner", "gone-away", true)
	spawnLocal(t, s, "tok-left", "gone-away", "left-here", true)
	spawnLocal(t, s, "tok-copied", "owner", "copied", true)
	spawnLocal(t, s, "tok-copied-work", "copied", "copied-work", true)
	seedTransfer(t, s, "gone-away", "outgoing", "move", "complete", 128)
	seedTransfer(t, s, "copied", "outgoing", "copy", "complete", 128)
	want := []familyRow{
		{"owner", "", DeleteNamed, 0},
		{"copied", "owner", DeleteWorker, 1},
		{"copied-work", "copied", DeleteWorker, 2},
	}
	if got := familyRows(t, s, "owner"); !slices.Equal(got, want) {
		t.Errorf("owner's family = %v, want %v", got, want)
	}
	if got := familyRows(t, s, "gone-away"); !slices.Equal(got, []familyRow{{"gone-away", "", DeleteNamed, 0}}) {
		t.Errorf("a moved thread's family = %v, want it alone", got)
	}
}

// The walk queues each thread once however many named threads reach it: a
// chain of workers seeded with every one of its threads, as a project
// removal seeds it, is n rows, where a walk keyed by path would hold
// n(n+1)/2.
func TestADeleteFamilyVisitsEachThreadOnce(t *testing.T) {
	s := newTestStore(t)
	const n = 120
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("chain-%03d", i)
		mustCreateThread(t, s, ids[i])
		if i > 0 {
			spawnLocal(t, s, "tok-"+ids[i], ids[i-1], ids[i], true)
		}
	}
	encoded := `["` + ids[0]
	for _, id := range ids[1:] {
		encoded += `","` + id
	}
	encoded += `"]`
	var queued int
	if err := s.db.QueryRow(deleteFamilyCTE+`SELECT COUNT(*) FROM family`, encoded).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != n {
		t.Fatalf("the walk queued %d rows for a chain of %d seeded with all of it, want %d", queued, n, n)
	}
	members, err := s.ListDeleteFamily(ids)
	if err != nil {
		t.Fatal(err)
	}
	for i, member := range members {
		if member.ThreadID != ids[i] || member.Depth != i || member.Kind != DeleteNamed {
			t.Fatalf("member %d = %+v, want %s at depth %d", i, member, ids[i], i)
		}
	}
}

// lineageThreadSQL is owned_threads' rule with the threads a delete took
// kept: for every transfer state, a thread not being deleted is reached by
// the walk exactly when owned_threads lists it.
func TestLineageThreadsAreOwnedThreadsAndTheirDeletes(t *testing.T) {
	s := newTestStore(t)
	index := 0
	for _, direction := range []string{"incoming", "outgoing"} {
		for _, kind := range []string{"move", "copy"} {
			for _, phase := range []string{"preparing", "prepared", "committed", "complete", "canceled"} {
				index++
				id := fmt.Sprintf("t-%02d", index)
				mustCreateThread(t, s, id)
				seedTransfer(t, s, id, direction, kind, phase, 128)
			}
		}
	}
	mustCreateThread(t, s, "plain")
	mustCreateThread(t, s, "holder")
	if _, err := s.db.Exec(`UPDATE threads SET mode = 'holder' WHERE id = 'holder'`); err != nil {
		t.Fatal(err)
	}
	owned, err := queryIDs(s.db, `SELECT id FROM owned_threads ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := queryIDs(s.db, `SELECT id FROM threads WHERE deleting = 0 AND `+lineageThreadSQL("threads")+` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(owned, lineage) {
		t.Fatalf("owned_threads = %v, the walk's threads = %v", owned, lineage)
	}
	if len(owned) < 2 || len(owned) == index+2 {
		t.Fatalf("owned_threads = %v: the fixture proves nothing", owned)
	}
	if _, err := s.db.Exec(`UPDATE threads SET deleting = 1, delete_claim = 'op' WHERE id = 'plain'`); err != nil {
		t.Fatal(err)
	}
	if got, err := queryIDs(s.db, `SELECT id FROM threads WHERE id = 'plain' AND `+lineageThreadSQL("threads")); err != nil || len(got) != 1 {
		t.Errorf("a committed thread to the walk = %v, %v; want reached", got, err)
	}
}

func deleteClaimOf(t *testing.T, s *Store, id string) (claim string, committed, removeWorktrees bool) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT delete_claim, deleting, delete_worktrees FROM threads WHERE id = ?`, id).Scan(&claim, &committed, &removeWorktrees); err != nil {
		t.Fatal(err)
	}
	return claim, committed, removeWorktrees
}

func familyIDs(members []DeleteMember) []string {
	ids := []string{}
	for _, member := range members {
		ids = append(ids, member.ThreadID)
	}
	return ids
}

// A delete claims its whole family in one transaction and refuses, changing
// nothing, a family another delete holds a claim in. Claiming again claims
// what joined the family since. A claimed thread is still listed, and runs
// and moves nothing; a release frees every claim at once. A commit marks
// the claimed threads deleting with the delete's worktree policy, and a
// later delete joins a committed thread instead of refusing it. The boot
// takes back the claims no delete committed.
func TestADeleteClaimsItsFamilyInOneTransaction(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "owned", "deep", "placed"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-owned", "owner", "owned", true)
	spawnLocal(t, s, "tok-deep", "owned", "deep", true)
	spawnLocal(t, s, "tok-placed", "owner", "placed", false)

	members, err := s.ClaimDeleteFamily("op-1", []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	if got := familyIDs(members); !slices.Equal(got, []string{"owner", "owned", "deep"}) {
		t.Fatalf("claimed = %v", got)
	}
	for _, id := range []string{"owner", "owned", "deep"} {
		if claim, committed, _ := deleteClaimOf(t, s, id); claim != "op-1" || committed {
			t.Errorf("%s = claim %q committed %v, want op-1's claim", id, claim, committed)
		}
	}
	if claim, _, _ := deleteClaimOf(t, s, "placed"); claim != "" {
		t.Errorf("the placed spawn was claimed by %q", claim)
	}
	if ids := catalogIDsOf(t, s); !slices.Contains(ids, "owner") {
		t.Errorf("catalog = %v, want the claimed thread still listed", ids)
	}

	// Another delete of any thread in the family is refused, with nothing
	// claimed.
	var claimed *DeleteClaimedError
	if _, err := s.ClaimDeleteFamily("op-2", []string{"owned"}); !errors.As(err, &claimed) || claimed.ThreadID != "owned" || !errors.Is(err, ErrThreadDeleting) {
		t.Fatalf("a second delete = %v, want the claimed thread named", err)
	}
	if ids, err := queryIDs(s.db, `SELECT id FROM threads WHERE delete_claim = 'op-2'`); err != nil || len(ids) != 0 {
		t.Errorf("the refused delete claimed %v, %v", ids, err)
	}

	// The fence: a claimed thread runs nothing and moves nowhere, and no
	// thread is given back to it.
	row, err := s.GetThread("owned")
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateThreadGroup(defaultTestProjectID, "g")
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"execution": s.CheckThreadExecutionAccess(row),
		"placement": func() error { _, err := s.SetThreadPlacement([]string{"owned"}, PlacementStandalone); return err }(),
		"group":     func() error { _, err := s.SetThreadGroup([]string{"owner"}, group.ID); return err }(),
		"transfer": func() error {
			_, err := s.CreateThreadTransfer(transferRequest("owner", "copy", "outgoing"))
			return err
		}(),
		"owned under a claimed owner": func() error {
			_, err := s.SetThreadPlacement([]string{"placed"}, PlacementOwned)
			return err
		}(),
	} {
		if !errors.Is(err, ErrThreadDeleting) {
			t.Errorf("%s on a claimed thread = %v, want ErrThreadDeleting", name, err)
		}
	}
	if err := s.CheckThreadDeleteAccess(row); err != nil {
		t.Errorf("the delete's own access = %v, want it admitted", err)
	}

	// A worker its owner spawned since joins the claim when it is read again.
	mustCreateThread(t, s, "late")
	spawnLocal(t, s, "tok-late", "owner", "late", true)
	if members, err = s.ClaimDeleteFamily("op-1", []string{"owner"}); err != nil {
		t.Fatal(err)
	}
	if got := familyIDs(members); !slices.Equal(got, []string{"owner", "late", "owned", "deep"}) {
		t.Fatalf("claimed again = %v", got)
	}

	if err := s.ReleaseDeleteClaim("op-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner", "owned", "deep", "late"} {
		if claim, _, _ := deleteClaimOf(t, s, id); claim != "" {
			t.Errorf("%s after the release = claim %q", id, claim)
		}
	}
	if row, err = s.GetThread("owned"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckThreadExecutionAccess(row); err != nil {
		t.Errorf("execution after the release = %v", err)
	}

	if _, err := s.ClaimDeleteFamily("op-2", []string{"owned"}); err != nil {
		t.Fatal(err)
	}
	committed, err := s.CommitDeleteClaim("op-2", true)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(committed)
	if !slices.Equal(committed, []string{"deep", "owned"}) {
		t.Fatalf("committed = %v", committed)
	}
	if _, committed, remove := deleteClaimOf(t, s, "owned"); !committed || !remove {
		t.Errorf("owned = committed %v remove worktrees %v, want both", committed, remove)
	}
	if ids := catalogIDsOf(t, s); slices.Contains(ids, "owned") {
		t.Errorf("catalog = %v, want the committed thread gone", ids)
	}
	// A later delete of the owner joins the committed threads.
	if members, err = s.ClaimDeleteFamily("op-3", []string{"owner"}); err != nil {
		t.Fatalf("a delete over a committed thread: %v", err)
	}
	for _, member := range members {
		if want := member.ThreadID == "owned" || member.ThreadID == "deep"; member.Committed != want {
			t.Errorf("%s committed = %v", member.ThreadID, member.Committed)
		}
		if member.Committed && (member.Claim != "op-2" || !member.RemoveWorktrees) {
			t.Errorf("%s = %+v, want op-2's commit kept", member.ThreadID, member)
		}
	}
	if released, err := s.ReleaseUndecidedDeleteClaims(); err != nil || released != 2 {
		t.Fatalf("the boot's release = %d, %v; want the owner and late", released, err)
	}
	for id, want := range map[string]string{"owner": "", "late": "", "owned": "op-2", "deep": "op-2"} {
		if claim, _, _ := deleteClaimOf(t, s, id); claim != want {
			t.Errorf("%s after the boot's release = claim %q, want %q", id, claim, want)
		}
	}
}

// A worker's claim takes its owner as its fence: it holds only while that
// owner still owns the worker.
func TestAWorkersClaimIsFencedByItsOwner(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "other", "owned", "placed"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-owned", "owner", "owned", true)
	spawnLocal(t, s, "tok-placed", "owner", "placed", false)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for id, owner := range map[string]string{"placed": "owner", "owned": "other"} {
		if err := claimThreadDeleteTx(tx, "op", id, owner); !errors.Is(err, ErrWorkerNotOwned) {
			t.Errorf("claim %s for %s = %v, want ErrWorkerNotOwned", id, owner, err)
		}
	}
	if err := claimThreadDeleteTx(tx, "op", "missing", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("claim a missing thread = %v, want sql.ErrNoRows", err)
	}
	if err := claimThreadDeleteTx(tx, "op", "owned", "owner"); err != nil {
		t.Errorf("claim the owner's worker = %v", err)
	}
	if err := claimThreadDeleteTx(tx, "op-2", "owned", "owner"); !errors.Is(err, ErrThreadDeleting) {
		t.Errorf("claim a claimed worker for another delete = %v, want ErrThreadDeleting", err)
	}
}

func catalogIDsOf(t *testing.T, s *Store) []string {
	t.Helper()
	ids, err := queryIDs(s.db, `SELECT id FROM owned_threads ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}
