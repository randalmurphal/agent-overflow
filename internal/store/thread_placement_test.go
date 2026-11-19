package store

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
)

// spawnLocal records a local spawn of target by caller, as the naming write
// leaves it, owned when own is set.
func spawnLocal(t *testing.T, s *Store, token, caller, target string, own bool) {
	t.Helper()
	if _, err := s.AcceptLocalThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: ThreadRequestSpawn, State: ThreadRequestUnconfirmed,
	}, ThreadRequestReceipt{Token: token, OwnerDeviceID: "device", Kind: ThreadRequestSpawn, SourceThreadID: caller}); err != nil {
		t.Fatalf("accept %s: %v", token, err)
	}
	if _, err := s.NameLocalThreadRequestTarget(token, target, own); err != nil {
		t.Fatalf("name %s: %v", token, err)
	}
}

func placementOf(t *testing.T, s *Store, id string) (string, string) {
	t.Helper()
	row, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return row.Placement, row.GroupID
}

// The schema refuses every combination of placement and group the sidebar
// could not render: grouped with no group, a group with any other
// placement, and a value outside the three.
func TestPlacementCheckRefusesInvalidCombinations(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "plain")
	mustCreateThread(t, s, "member")
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"member"}, group.ID); err != nil {
		t.Fatal(err)
	}
	for name, update := range map[string]struct {
		sql  string
		args []any
	}{
		"grouped without a group":  {`UPDATE threads SET placement = 'grouped' WHERE id = 'plain'`, nil},
		"a group while standalone": {`UPDATE threads SET group_id = ? WHERE id = 'plain'`, []any{group.ID}},
		"a group while owned":      {`UPDATE threads SET placement = 'owned' WHERE id = 'member'`, nil},
		"the retired nested value": {`UPDATE threads SET placement = 'nested' WHERE id = 'plain'`, nil},
		"ungrouped while grouped":  {`UPDATE threads SET group_id = NULL WHERE id = 'member'`, nil},
		"an unknown placement":     {`UPDATE threads SET placement = 'floating' WHERE id = 'plain'`, nil},
	} {
		if _, err := s.db.Exec(update.sql, update.args...); err == nil {
			t.Errorf("%s: the write was accepted", name)
		}
	}
	if got, groupID := placementOf(t, s, "member"); got != PlacementGrouped || groupID != group.ID {
		t.Errorf("member = %s in %q, want grouped in %s", got, groupID, group.ID)
	}
	if got, _ := placementOf(t, s, "plain"); got != PlacementStandalone {
		t.Errorf("plain = %s, want standalone", got)
	}
}

// A thread an assistant spawned joins and leaves groups like any other:
// a move in makes it grouped, an ungroup leaves it standalone, and the
// assistant's own thread is still refused.
func TestSpawnedChildMovesBetweenPlacements(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"agent", "child", "stranger"} {
		mustCreateThread(t, s, id)
	}
	mustCreateAssistant(t, s, "agent", "/h/agent")
	spawnLocal(t, s, "tok", "agent", "child", true)
	if got, _ := placementOf(t, s, "child"); got != PlacementOwned {
		t.Fatalf("spawned child = %s, want owned", got)
	}
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"agent"}, group.ID); !errors.Is(err, ErrStandingAgentGrouped) {
		t.Errorf("grouping the assistant = %v, want ErrStandingAgentGrouped", err)
	}
	moved, err := s.SetThreadGroup([]string{"child"}, group.ID)
	if err != nil || len(moved) != 1 || moved[0].Thread.Placement != PlacementGrouped || moved[0].Thread.GroupID != group.ID {
		t.Fatalf("grouping the child = %+v, %v", moved, err)
	}
	// An ungroup naming a row that was never grouped leaves its placement.
	if _, err := s.SetThreadGroup([]string{"child", "stranger"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, groupID := placementOf(t, s, "child"); got != PlacementStandalone || groupID != "" {
		t.Errorf("ungrouped child = %s in %q, want standalone", got, groupID)
	}
	if _, err := s.SetThreadPlacement([]string{"child"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"child"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "child"); got != PlacementOwned {
		t.Errorf("an ungroup of an owned child made it %s", got)
	}
	// Grouped to standalone or owned through SetThreadPlacement leaves the group.
	if _, err := s.SetThreadGroup([]string{"child"}, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadPlacement([]string{"child"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	if got, groupID := placementOf(t, s, "child"); got != PlacementOwned || groupID != "" {
		t.Errorf("owned from a group = %s in %q", got, groupID)
	}
}

// SetThreadPlacement refuses what the schema cannot say and what nothing
// would own, and rolls the whole call back.
func TestSetThreadPlacementRefusals(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"agent", "child", "plain"} {
		mustCreateThread(t, s, id)
	}
	mustCreateAssistant(t, s, "agent", "/h/agent")
	spawnLocal(t, s, "tok", "agent", "child", false)
	if got, _ := placementOf(t, s, "child"); got != PlacementStandalone {
		t.Fatalf("a spawn named without own = %s, want standalone", got)
	}
	for name, tc := range map[string]struct {
		ids       []string
		placement string
		want      error
	}{
		"owned with no link":         {[]string{"plain"}, PlacementOwned, ErrThreadNotSpawned},
		"the assistant's own":        {[]string{"agent"}, PlacementStandalone, ErrStandingAgentPlaced},
		"grouped with no group":      {[]string{"child"}, PlacementGrouped, ErrPlacementNeedsGroup},
		"an unknown placement":       {[]string{"child"}, "floating", ErrInvalidPlacement},
		"a missing thread":           {[]string{"gone"}, PlacementStandalone, ErrThreadGone},
		"one refusal in a bulk call": {[]string{"child", "plain"}, PlacementOwned, ErrThreadNotSpawned},
	} {
		if _, err := s.SetThreadPlacement(tc.ids, tc.placement); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if got, _ := placementOf(t, s, "child"); got != PlacementStandalone {
		t.Errorf("a refused bulk call left child %s", got)
	}
}

// A placement change unpins the row, as a group move does; a call that
// changes nothing keeps the pin.
func TestPlacementChangeUnpins(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "child")
	spawnLocal(t, s, "tok", "agent", "child", true)
	if _, _, err := s.PinThread("child"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.SetThreadPlacement([]string{"child"}, PlacementOwned)
	if err != nil || rows[0].Thread.PinnedAt == nil {
		t.Fatalf("an unchanged placement = %+v, %v; want the pin kept", rows, err)
	}
	rows, err = s.SetThreadPlacement([]string{"child"}, PlacementStandalone)
	if err != nil || rows[0].Thread.PinnedAt != nil || rows[0].Thread.Placement != PlacementStandalone {
		t.Fatalf("a placement change = %+v, %v; want standalone and unpinned", rows, err)
	}
}

// Deleting a group leaves its members, archived ones included, standalone:
// the FK's SET NULL would otherwise break the placement CHECK.
func TestDeleteThreadGroupLeavesMembersStandalone(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "child")
	mustCreateThread(t, s, "archived")
	spawnLocal(t, s, "tok", "agent", "child", true)
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"child", "archived"}, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveThread("archived"); err != nil {
		t.Fatal(err)
	}
	members, err := s.DeleteThreadGroup(group.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("delete group = %+v, %v", members, err)
	}
	for _, id := range []string{"child", "archived"} {
		if got, groupID := placementOf(t, s, id); got != PlacementStandalone || groupID != "" {
			t.Errorf("%s after the delete = %s in %q, want standalone", id, got, groupID)
		}
	}
}

// A thread created into a group (a fork of a grouped thread) is inserted
// grouped; any other insert is standalone.
func TestInsertDerivesPlacementFromGroup(t *testing.T) {
	s := newTestStore(t)
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	grouped := makeThread("grouped", "claude")
	grouped.GroupID = group.ID
	grouped.Placement = PlacementOwned
	if err := s.CreateThread(grouped); err != nil {
		t.Fatal(err)
	}
	plain := makeThread("plain", "claude")
	plain.Placement = PlacementOwned
	if err := s.CreateThread(plain); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "grouped"); got != PlacementGrouped {
		t.Errorf("inserted with a group = %s, want grouped", got)
	}
	if got, _ := placementOf(t, s, "plain"); got != PlacementStandalone {
		t.Errorf("inserted without a group = %s, want standalone", got)
	}
}

// The spawn's naming write makes its thread owned only when asked, and
// never pulls a thread the spawn put in a group out of it.
func TestNamingWriteOwnsOnRequest(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"agent", "owned", "standalone", "grouped"} {
		mustCreateThread(t, s, id)
	}
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"grouped"}, group.ID); err != nil {
		t.Fatal(err)
	}
	spawnLocal(t, s, "tok-n", "agent", "owned", true)
	spawnLocal(t, s, "tok-s", "agent", "standalone", false)
	spawnLocal(t, s, "tok-g", "agent", "grouped", true)
	for id, want := range map[string]string{"owned": PlacementOwned, "standalone": PlacementStandalone, "grouped": PlacementGrouped} {
		if got, _ := placementOf(t, s, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
}

// When an owned thread's link is deleted, the trigger leaves it
// standalone, a thread a superseded spawn row still names included: that
// row owns nothing.
func TestOwnedThreadLosingItsLinkBecomesStandalone(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"spawner", "other", "child", "shared"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-child", "spawner", "child", true)
	spawnLocal(t, s, "tok-shared", "spawner", "shared", true)
	if _, err := s.db.Exec(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, lineage_only, superseded, created_at, updated_at)
		VALUES ('tok-shared-old', 'other', 'spawn', '', 'shared', 'finished', 1, 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("spawner"); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "child"); got != PlacementStandalone {
		t.Errorf("child after its spawner's delete = %s, want standalone", got)
	}
	if got, _ := placementOf(t, s, "shared"); got != PlacementStandalone {
		t.Errorf("a thread only a superseded spawn row still names = %s, want standalone", got)
	}
}

// A handoff leaves the old thread's placement alone: a past thread lives
// outside every project, under its assistant, and takes no placement.
func TestAHandOffLeavesThePastThreadUnplaced(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "old")
	mustCreateThread(t, s, "new")
	mustCreateAssistant(t, s, "old", "/h/agent")
	mustPlaceInHome(t, s, "new", "/h/agent")
	if _, err := s.HandOffAssistant("old", "new", 10); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "new"} {
		if got, _ := placementOf(t, s, id); got != PlacementStandalone {
			t.Errorf("%s = %s, want standalone", id, got)
		}
	}
}

// An organize write's placement runs after its group move and before its
// pin, and its refusals roll the whole write back.
func TestApplyThreadOrganizeWritesPlacement(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "child")
	mustCreateThread(t, s, "plain")
	spawnLocal(t, s, "tok", "agent", "child", true)
	standalone := PlacementStandalone
	result, err := s.ApplyThreadOrganize("child", ThreadOrganizeWrite{
		Placement: &standalone,
		Pin:       &ThreadPinWrite{Pinned: true},
	})
	if err != nil || !result.Changed || result.Thread.Placement != PlacementStandalone || result.Thread.PinnedAt == nil {
		t.Fatalf("organize = %+v, %v; want standalone and pinned", result, err)
	}
	owned := PlacementOwned
	title := "Renamed"
	if _, err := s.ApplyThreadOrganize("plain", ThreadOrganizeWrite{Title: &title, Placement: &owned}); !errors.Is(err, ErrThreadNotSpawned) {
		t.Fatalf("owning a plain thread = %v, want ErrThreadNotSpawned", err)
	}
	if row, err := s.GetThread("plain"); err != nil || row.Title == title {
		t.Errorf("a refused organize write renamed the thread: %+v, %v", row, err)
	}
}

// A deleted thread its forks still read is kept as a holder, out of its
// group and owned by nothing.
func TestRetiredHolderIsStandalone(t *testing.T) {
	for _, placement := range []string{PlacementGrouped, PlacementOwned} {
		t.Run(placement, func(t *testing.T) {
			s := readersFixture(t, 20, 1)
			mustCreateThread(t, s, "agent")
			if placement == PlacementGrouped {
				group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.SetThreadGroup([]string{"S"}, group.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				spawnLocal(t, s, "tok", "agent", "S", true)
			}
			if err := s.DeleteThread("S"); err != nil {
				t.Fatal(err)
			}
			var mode, got string
			if err := s.db.QueryRow(`SELECT mode, placement FROM threads WHERE id = 'S'`).Scan(&mode, &got); err != nil {
				t.Fatalf("the holder row: %v", err)
			}
			if got != PlacementStandalone {
				t.Errorf("holder (%s) placement = %s, want standalone", mode, got)
			}
		})
	}
}

// The naming write owns only under a source row that is there: a spawn
// whose caller was deleted while its thread was being created names the
// thread on the receipt and leaves it standalone, with no link to own it.
func TestNamingWriteWithoutItsSourceRowLeavesTheThreadStandalone(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateThread(t, s, "child")
	if _, err := s.AcceptLocalThreadRequest(ThreadRequest{
		Token: "tok", CallerThreadID: "agent", Kind: ThreadRequestSpawn, State: ThreadRequestUnconfirmed,
	}, ThreadRequestReceipt{Token: "tok", OwnerDeviceID: "device", Kind: ThreadRequestSpawn, SourceThreadID: "agent"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NameLocalThreadRequestTarget("tok", "child", true); err != nil {
		t.Fatalf("name after the caller's delete: %v", err)
	}
	if got, _ := placementOf(t, s, "child"); got != PlacementStandalone {
		t.Errorf("child with no source row = %s, want standalone", got)
	}
}

// An assistant's past thread, outside every project, takes no placement.
func TestAPastThreadTakesNoPlacement(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "old")
	mustCreateThread(t, s, "new")
	mustCreateAssistant(t, s, "old", "/h/agent")
	mustPlaceInHome(t, s, "new", "/h/agent")
	if _, err := s.HandOffAssistant("old", "new", 10); err != nil {
		t.Fatal(err)
	}
	for _, placement := range []string{PlacementStandalone, PlacementOwned} {
		if _, err := s.SetThreadPlacement([]string{"old"}, placement); !errors.Is(err, ErrPlacementOutsideProjects) {
			t.Errorf("%s for a past thread = %v, want ErrPlacementOutsideProjects", placement, err)
		}
	}
}

// An assistant's helper is owned from its spawn and stays owned when its
// spawn row goes: outside every project it has no sidebar row to return to,
// and its assistant's delete deletes it.
func TestAHelperStaysOwnedWithoutItsLink(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "agent")
	mustCreateAssistant(t, s, "agent", "/h/agent")
	homeThread(t, s, "helper", "agent")
	spawnLocal(t, s, "tok", "agent", "helper", true)
	if got, _ := placementOf(t, s, "helper"); got != PlacementOwned {
		t.Fatalf("spawned helper = %s, want owned", got)
	}
	if _, err := s.db.Exec(`DELETE FROM thread_requests WHERE token = 'tok'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "helper"); got != PlacementOwned {
		t.Errorf("helper without its spawn row = %s, want owned", got)
	}
}

// The sidebar's catalog reads leave an owned thread out, active or
// archived, and so do the project's thread counts; search and the by-project
// listing find it like any other thread. Placed standalone it is back.
func TestCatalogLeavesOwnedThreadsOut(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"spawner", "worker", "plain"} {
		mustCreateThread(t, s, id)
		if err := s.InsertItem(Item{ID: id + "-msg", ThreadID: id, Kind: "user_text", Role: "user", Summary: "needle " + id}); err != nil {
			t.Fatal(err)
		}
	}
	spawnLocal(t, s, "tok", "spawner", "worker", true)
	listed := func() []string {
		t.Helper()
		rows, err := s.ListThreadsWithItems()
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		slices.Sort(ids)
		return ids
	}
	count := func() int {
		t.Helper()
		projects, err := s.ListProjectsWithThreadCounts()
		if err != nil {
			t.Fatal(err)
		}
		for _, project := range projects {
			if project.Project.ID == defaultTestProjectID {
				return project.ThreadCount
			}
		}
		t.Fatalf("project %s is not listed", defaultTestProjectID)
		return 0
	}
	if got := listed(); !slices.Equal(got, []string{"plain", "spawner"}) {
		t.Errorf("catalog = %v, want the owned worker left out", got)
	}
	if got := count(); got != 2 {
		t.Errorf("project thread count = %d, want 2", got)
	}
	hits, err := s.SearchThreadMessages("needle worker", 10)
	if err != nil || !slices.ContainsFunc(hits, func(hit ThreadMessageHit) bool { return hit.ThreadID == "worker" }) {
		t.Errorf("search for the owned worker = %+v, %v; want it found", hits, err)
	}
	if byProject, err := s.ListThreadsByProject(defaultTestProjectID); err != nil || !slices.ContainsFunc(byProject, func(row Thread) bool { return row.ID == "worker" }) {
		t.Errorf("by-project listing = %v; want the owned worker", err)
	}
	for _, id := range []string{"worker", "plain"} {
		if _, _, err := s.ArchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	archived, err := s.ListArchivedThreads()
	if err != nil || len(archived) != 1 || archived[0].ID != "plain" {
		t.Errorf("archived list = %+v, %v; want only plain", archived, err)
	}
	for _, id := range []string{"worker", "plain"} {
		if _, _, err := s.UnarchiveThread(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if got := listed(); !slices.Equal(got, []string{"plain", "spawner", "worker"}) {
		t.Errorf("catalog after Show in Sidebar = %v, want the worker listed", got)
	}
	if got := count(); got != 3 {
		t.Errorf("project thread count after Show in Sidebar = %d, want 3", got)
	}
}

// ListOwnedWorkersToArchive names the unarchived owned threads whose spawn link
// the owner made: not a standalone or archived worker, not for a superseded
// spawn row's caller, even a later one, not a worker's own worker unless
// that worker is archived.
func TestListOwnedWorkersToArchive(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "other", "owned", "deep", "placed", "archived", "moved"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-owned", "owner", "owned", true)
	spawnLocal(t, s, "tok-deep", "owned", "deep", true)
	spawnLocal(t, s, "tok-placed", "owner", "placed", false)
	spawnLocal(t, s, "tok-archived", "owner", "archived", true)
	spawnLocal(t, s, "tok-moved", "owner", "moved", true)
	if _, err := s.db.Exec(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, lineage_only, superseded, created_at, updated_at)
		VALUES ('tok-moved-later', 'other', 'spawn', '', 'moved', 'finished', 1, 1, ?1, ?1)`, nowMillis()+60_000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveThread("archived"); err != nil {
		t.Fatal(err)
	}
	list := func(owner string) []OwnedWorker {
		t.Helper()
		got, err := s.ListOwnedWorkersToArchive(owner)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := list("owner"); !slices.Equal(got, []OwnedWorker{{"moved", "owner"}, {"owned", "owner"}}) {
		t.Errorf("owner's workers = %v; want [moved owned], stopping there", got)
	}
	if got := list("owned"); !slices.Equal(got, []OwnedWorker{{"deep", "owned"}}) {
		t.Errorf("a worker's workers = %v; want [deep]", got)
	}
	if got := list("other"); len(got) != 0 {
		t.Errorf("a superseded spawn's caller's workers = %v; want none", got)
	}
	// Through an archived worker the read goes on to the unarchived ones
	// beneath it, each fenced by its own owner; not beneath one moved away.
	if _, _, err := s.ArchiveThread("owned"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"under-archived", "under-placed", "under-under"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-under-archived", "archived", "under-archived", true)
	spawnLocal(t, s, "tok-under-placed", "placed", "under-placed", true)
	spawnLocal(t, s, "tok-under-under", "under-archived", "under-under", true)
	if got := list("owner"); !slices.Equal(got, []OwnedWorker{{"deep", "owned"}, {"moved", "owner"}, {"under-archived", "archived"}}) {
		t.Errorf("owner's workers through archived ones = %v; want deep, moved and under-archived, not beneath the placed one or the unarchived one", got)
	}
}

// ArchiveOwnedWorker archives a worker only while its owner is archived and
// still owns it, and reports any other worker unchanged.
func TestArchiveOwnedWorkerIsFencedByItsOwnersArchive(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"owner", "other", "owned", "placed", "moved"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-owned", "owner", "owned", true)
	spawnLocal(t, s, "tok-placed", "owner", "placed", false)
	spawnLocal(t, s, "tok-moved", "owner", "moved", true)
	// The spawn that reaches moved now names another caller, as a handoff
	// leaves it.
	if _, err := s.db.Exec(`UPDATE thread_requests SET caller_thread_id = 'other' WHERE token = 'tok-moved'`); err != nil {
		t.Fatal(err)
	}
	archived := func(id string) bool {
		t.Helper()
		row, err := s.GetThread(id)
		if err != nil {
			t.Fatal(err)
		}
		return row.Archived
	}
	if _, changed, err := s.ArchiveOwnedWorker("owned", "owner"); err != nil || changed || archived("owned") {
		t.Fatalf("worker of an unarchived owner = changed %v, %v; want left alone", changed, err)
	}
	if _, _, err := s.ArchiveThread("owner"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"placed", "moved"} {
		if _, changed, err := s.ArchiveOwnedWorker(id, "owner"); err != nil || changed || archived(id) {
			t.Errorf("%s, not the owner's = changed %v, %v; want left alone", id, changed, err)
		}
	}
	row, changed, err := s.ArchiveOwnedWorker("owned", "owner")
	if err != nil || !changed || !row.Archived {
		t.Fatalf("the owner's worker = %+v changed %v, %v; want archived", row, changed, err)
	}
	if _, changed, err := s.ArchiveOwnedWorker("owned", "owner"); err != nil || changed {
		t.Errorf("again = changed %v, %v; want unchanged", changed, err)
	}
	if _, _, err := s.ArchiveOwnedWorker("missing", "owner"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a missing worker = %v, want sql.ErrNoRows", err)
	}
}
