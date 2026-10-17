package store

import (
	"errors"
	"testing"
)

// spawnLocal records a local spawn of target by caller, as the naming write
// leaves it, nested when nest is set.
func spawnLocal(t *testing.T, s *Store, token, caller, target string, nest bool) {
	t.Helper()
	if _, err := s.AcceptLocalThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: ThreadRequestSpawn, State: ThreadRequestUnconfirmed,
	}, ThreadRequestReceipt{Token: token, OwnerDeviceID: "device", Kind: ThreadRequestSpawn, SourceThreadID: caller}); err != nil {
		t.Fatalf("accept %s: %v", token, err)
	}
	if err := s.NameLocalThreadRequestTarget(token, target, nest); err != nil {
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
		"a group while nested":     {`UPDATE threads SET placement = 'nested' WHERE id = 'member'`, nil},
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
	if got, _ := placementOf(t, s, "child"); got != PlacementNested {
		t.Fatalf("spawned child = %s, want nested", got)
	}
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"agent"}, group.ID); !errors.Is(err, ErrStandingAgentGrouped) {
		t.Errorf("grouping the assistant = %v, want ErrStandingAgentGrouped", err)
	}
	moved, err := s.SetThreadGroup([]string{"child"}, group.ID)
	if err != nil || len(moved) != 1 || moved[0].Placement != PlacementGrouped || moved[0].GroupID != group.ID {
		t.Fatalf("grouping the child = %+v, %v", moved, err)
	}
	// An ungroup naming a row that was never grouped leaves its placement.
	if _, err := s.SetThreadGroup([]string{"child", "stranger"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, groupID := placementOf(t, s, "child"); got != PlacementStandalone || groupID != "" {
		t.Errorf("ungrouped child = %s in %q, want standalone", got, groupID)
	}
	if _, err := s.SetThreadPlacement([]string{"child"}, PlacementNested); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"child"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "child"); got != PlacementNested {
		t.Errorf("an ungroup of a nested child made it %s", got)
	}
	// Grouped to standalone or nested through SetThreadPlacement leaves the group.
	if _, err := s.SetThreadGroup([]string{"child"}, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadPlacement([]string{"child"}, PlacementNested); err != nil {
		t.Fatal(err)
	}
	if got, groupID := placementOf(t, s, "child"); got != PlacementNested || groupID != "" {
		t.Errorf("nested from a group = %s in %q", got, groupID)
	}
}

// SetThreadPlacement refuses what the schema cannot say and what nothing
// would nest under, and rolls the whole call back.
func TestSetThreadPlacementRefusals(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"agent", "child", "plain"} {
		mustCreateThread(t, s, id)
	}
	mustCreateAssistant(t, s, "agent", "/h/agent")
	spawnLocal(t, s, "tok", "agent", "child", false)
	if got, _ := placementOf(t, s, "child"); got != PlacementStandalone {
		t.Fatalf("a spawn named without nest = %s, want standalone", got)
	}
	for name, tc := range map[string]struct {
		ids       []string
		placement string
		want      error
	}{
		"nested with no link":        {[]string{"plain"}, PlacementNested, ErrThreadNotSpawned},
		"the assistant's own":        {[]string{"agent"}, PlacementStandalone, ErrStandingAgentPlaced},
		"grouped with no group":      {[]string{"child"}, PlacementGrouped, ErrPlacementNeedsGroup},
		"an unknown placement":       {[]string{"child"}, "floating", ErrInvalidPlacement},
		"a missing thread":           {[]string{"gone"}, PlacementStandalone, ErrThreadGone},
		"one refusal in a bulk call": {[]string{"child", "plain"}, PlacementNested, ErrThreadNotSpawned},
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
	rows, err := s.SetThreadPlacement([]string{"child"}, PlacementNested)
	if err != nil || rows[0].PinnedAt == nil {
		t.Fatalf("an unchanged placement = %+v, %v; want the pin kept", rows, err)
	}
	rows, err = s.SetThreadPlacement([]string{"child"}, PlacementStandalone)
	if err != nil || rows[0].PinnedAt != nil || rows[0].Placement != PlacementStandalone {
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
	grouped.Placement = PlacementNested
	if err := s.CreateThread(grouped); err != nil {
		t.Fatal(err)
	}
	plain := makeThread("plain", "claude")
	plain.Placement = PlacementNested
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

// The spawn's naming write nests its thread only when asked, and never
// pulls a thread the spawn put in a group out of it.
func TestNamingWriteNestsOnRequest(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"agent", "nested", "standalone", "grouped"} {
		mustCreateThread(t, s, id)
	}
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"grouped"}, group.ID); err != nil {
		t.Fatal(err)
	}
	spawnLocal(t, s, "tok-n", "agent", "nested", true)
	spawnLocal(t, s, "tok-s", "agent", "standalone", false)
	spawnLocal(t, s, "tok-g", "agent", "grouped", true)
	for id, want := range map[string]string{"nested": PlacementNested, "standalone": PlacementStandalone, "grouped": PlacementGrouped} {
		if got, _ := placementOf(t, s, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
}

// When the last link a nested thread has is deleted, the trigger leaves it
// standalone; a thread with another link stays nested.
func TestNestedThreadLosingItsLinkBecomesStandalone(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"spawner", "other", "child", "shared"} {
		mustCreateThread(t, s, id)
	}
	spawnLocal(t, s, "tok-child", "spawner", "child", true)
	spawnLocal(t, s, "tok-shared", "spawner", "shared", true)
	if _, err := s.db.Exec(`INSERT INTO thread_requests (token, caller_thread_id, kind, target_computer_id, target_thread_id, state, lineage_only, created_at, updated_at)
		VALUES ('tok-shared-old', 'other', 'spawn', '', 'shared', 'finished', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("spawner"); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "child"); got != PlacementStandalone {
		t.Errorf("child after its spawner's delete = %s, want standalone", got)
	}
	if got, _ := placementOf(t, s, "shared"); got != PlacementNested {
		t.Errorf("a thread another spawn row still names = %s, want nested", got)
	}
}

// A handoff nests the old thread under the agent as its past thread, and
// the assistant's delete, which takes the past-thread row, leaves it
// standalone.
func TestHandoffNestsThePastThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "old")
	mustCreateThread(t, s, "new")
	mustCreateAssistant(t, s, "old", "/h/agent")
	if _, err := s.HandOffAssistant("old", "new", 10); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "old"); got != PlacementNested {
		t.Fatalf("past thread = %s, want nested", got)
	}
	if got, _ := placementOf(t, s, "new"); got != PlacementStandalone {
		t.Errorf("successor = %s, want standalone", got)
	}
	if _, err := s.db.Exec(`DELETE FROM agent_past_threads WHERE thread_id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := placementOf(t, s, "old"); got != PlacementStandalone {
		t.Errorf("past thread without its link = %s, want standalone", got)
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
	nested := PlacementNested
	title := "Renamed"
	if _, err := s.ApplyThreadOrganize("plain", ThreadOrganizeWrite{Title: &title, Placement: &nested}); !errors.Is(err, ErrThreadNotSpawned) {
		t.Fatalf("nesting a plain thread = %v, want ErrThreadNotSpawned", err)
	}
	if row, err := s.GetThread("plain"); err != nil || row.Title == title {
		t.Errorf("a refused organize write renamed the thread: %+v, %v", row, err)
	}
}

// A deleted thread its forks still read is kept as a holder, out of its
// group and nested under nothing.
func TestRetiredHolderIsStandalone(t *testing.T) {
	for _, placement := range []string{PlacementGrouped, PlacementNested} {
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
