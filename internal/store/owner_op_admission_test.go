package store

import (
	"errors"
	"slices"
	"testing"
)

// A delete a paired computer's owner sends through its link claims the
// named worker only through that link, and takes what the worker owns here
// with it. A worker the link no longer owns, by another device or token, or
// one the person gave a sidebar row, refuses the claim with nothing
// claimed.
func TestALinkedDeleteClaimsItsWorkerThroughTheLink(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"worker", "child", "placed"} {
		mustCreateThread(t, s, id)
	}
	acceptLink(t, s, "tok", "dev", "worker", 1000)
	spawnLocal(t, s, "tok-child", "worker", "child", true)
	acceptLink(t, s, "tok-placed", "dev", "placed", 1000)
	if _, err := s.SetThreadPlacement([]string{"placed"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	claims := func() []string {
		t.Helper()
		ids, err := queryIDs(s.reader(), `SELECT id FROM threads WHERE delete_claim <> '' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	for name, claim := range map[string]struct {
		id    string
		fence LinkFence
	}{
		"another device":   {"worker", LinkFence{Token: "tok", DeviceID: "other"}},
		"another token":    {"worker", LinkFence{Token: "tok-placed", DeviceID: "dev"}},
		"a sidebar thread": {"placed", LinkFence{Token: "tok-placed", DeviceID: "dev"}},
	} {
		if _, err := s.ClaimLinkedDelete("op-"+name, claim.id, claim.fence); !errors.Is(err, ErrWorkerNotOwned) {
			t.Errorf("%s: claim = %v, want ErrWorkerNotOwned", name, err)
		}
		if got := claims(); len(got) != 0 {
			t.Fatalf("%s: a refused claim left %v claimed", name, got)
		}
	}
	members, err := s.ClaimLinkedDelete("op", "worker", LinkFence{Token: "tok", DeviceID: "dev"})
	if err != nil {
		t.Fatalf("the link's claim: %v", err)
	}
	var ids []string
	for _, member := range members {
		ids = append(ids, member.ThreadID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"child", "worker"}) || !slices.Equal(claims(), ids) {
		t.Fatalf("the link's claim took %v and claimed %v, want the worker and its child", ids, claims())
	}
}

// What a delete's confirmation reads of the threads it takes: the spawns
// they made on other computers, their acceptance unanswered or not. A spawn
// here is not one.
func TestListRemoteSpawnTokens(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"lead", "worker", "local-only", "alone"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-worker", "lead", "worker", true)
	seedRemoteSpawn(t, s, "r-lost", "worker", "pc-a", "")
	spawnLocal(t, s, "tok-local", "local-only", "alone", true)
	for _, tc := range []struct {
		ids  []string
		want bool
	}{
		{[]string{"lead", "worker"}, true},
		{[]string{"worker"}, true},
		{[]string{"lead"}, false},
		{[]string{"local-only", "alone"}, false},
		{nil, false},
	} {
		got, err := s.ListRemoteSpawnTokens(tc.ids)
		if err != nil || (len(got) == 1 && got[0] == "r-lost") != tc.want || (!tc.want && len(got) != 0) {
			t.Errorf("ListRemoteSpawnTokens(%v) = %v, %v; want r-lost %v", tc.ids, got, err, tc.want)
		}
	}
}

// OwesWorkerEnd says an archive or delete is owed to the spawn's worker
// while one is, and not for a supersede or once the archive is settled;
// it names the spawn's caller while that caller is archived.
func TestOwesWorkerEnd(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	seedRemoteSpawn(t, s, "r1", "lead", "pc-a", "worker-there")
	owes := func(want bool, wantCaller, when string) {
		t.Helper()
		if got, caller, err := s.OwesWorkerEnd("r1"); err != nil || got != want || caller != wantCaller {
			t.Fatalf("%s: OwesWorkerEnd = %v, %q, %v; want %v, %q", when, got, caller, err, want, wantCaller)
		}
	}
	owes(false, "", "before the archive")
	if _, _, err := s.ArchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	owes(false, "lead", "with the archive not yet owed")
	owed, err := s.OweRemoteArchives("lead")
	if err != nil || len(owed) != 1 {
		t.Fatalf("owe: %+v, %v", owed, err)
	}
	owes(true, "lead", "with the archive owed")
	if _, err := s.MarkOwnerOpSent("r1", owed[0].Seq); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UnarchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	owes(false, "", "with a supersede owed")
	if _, _, err := s.ArchiveThread("lead"); err != nil {
		t.Fatal(err)
	}
	owed, err = s.OweRemoteArchives("lead")
	if err != nil || len(owed) != 1 {
		t.Fatalf("owe again: %+v, %v", owed, err)
	}
	owes(true, "lead", "with the second archive owed")
	if _, err := s.SettleOwnerOp("r1", owed[0].Seq); err != nil {
		t.Fatal(err)
	}
	owes(false, "lead", "once the archive is settled")
}
