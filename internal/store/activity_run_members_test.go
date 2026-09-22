package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// On-demand members (docs/architecture/timeline-window-pages.md §3) and
// the two things a page's stub has to keep true about them: the run is
// re-derived from the database on every call, and the stub it comes back
// with describes the span the caller ends up holding.

func memberIDs(members ActivityRunMembers) []string {
	return collectIDs(members.Items)
}

func TestActivityRunMembersTrimFreshMatchesSmallerRead(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", "ptrcrcttrrp")
	cases := []ActivityRunMembersRequest{
		{RunFirstItemID: ids[1], Direction: ActivityRunMembersBefore},
		{RunFirstItemID: ids[1], Direction: ActivityRunMembersAfter},
		{RunFirstItemID: ids[1], LoadedFirstItemID: ids[7], LoadedLastItemID: ids[9], Direction: ActivityRunMembersBefore},
		{RunFirstItemID: ids[1], LoadedFirstItemID: ids[2], LoadedLastItemID: ids[3], Direction: ActivityRunMembersAfter},
		{RunFirstItemID: ids[1], LoadedFirstItemID: ids[7], LoadedLastItemID: ids[9], Direction: ActivityRunMembersAround, AroundItemID: ids[4]},
	}
	for _, req := range cases {
		largeReq := req
		largeReq.Limit = 5
		large, err := s.ListActivityRunMembers(context.Background(), "t", largeReq)
		if err != nil {
			t.Fatal(err)
		}
		smallReq := req
		smallReq.Limit = 2
		small, err := s.ListActivityRunMembers(context.Background(), "t", smallReq)
		if err != nil {
			t.Fatal(err)
		}
		from := -1
		for i, item := range large.Items {
			if len(small.Items) > 0 && item.ID == small.Items[0].ID {
				from = i
				break
			}
		}
		if from < 0 {
			t.Fatalf("%s: small answer %v outside large answer %v", req.Direction, memberIDs(small), memberIDs(large))
		}
		trimmed, err := large.TrimFresh(from, from+len(small.Items))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(trimmed.Items, small.Items) || !reflect.DeepEqual(trimmed.Stub, small.Stub) {
			t.Fatalf("%s: trimmed answer differs from smaller read\ntrimmed: %+v\nsmall: %+v", req.Direction, trimmed.Stub, small.Stub)
		}
	}
}

// TestListActivityRunMembersWalksEachDirection covers the three
// directions plus the stub-only refresh over one run.
func TestListActivityRunMembersWalksEachDirection(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", runSpec(20))
	run := ids[1]

	// No span held: "before" takes the run's newest members, the same end
	// a page ships.
	newest, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID: run,
		Direction:      ActivityRunMembersBefore,
		Limit:          5,
	})
	if err != nil {
		t.Fatalf("before with no span: %v", err)
	}
	if got := memberIDs(newest); len(got) != 5 || got[0] != ids[16] || got[4] != ids[20] {
		t.Fatalf("before with no span = %v, want %s..%s", got, ids[16], ids[20])
	}
	if newest.Stub.UnshippedBefore != 15 || newest.Stub.UnshippedAfter != 0 {
		t.Errorf("stub unshipped = %d/%d, want 15/0",
			newest.Stub.UnshippedBefore, newest.Stub.UnshippedAfter)
	}

	// "before" an existing span extends it older: only the new rows ship,
	// and the stub describes the union.
	older, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID:    run,
		LoadedFirstItemID: newest.Stub.LoadedFirstItemID,
		LoadedLastItemID:  newest.Stub.LoadedLastItemID,
		Direction:         ActivityRunMembersBefore,
		Limit:             5,
	})
	if err != nil {
		t.Fatalf("before an existing span: %v", err)
	}
	if got := memberIDs(older); len(got) != 5 || got[0] != ids[11] || got[4] != ids[15] {
		t.Fatalf("before an existing span = %v, want only the new rows %s..%s", got, ids[11], ids[15])
	}
	if older.Stub.LoadedFirstItemID != ids[11] || older.Stub.UnshippedBefore != 10 {
		t.Errorf("stub = %s with %d before, want %s with 10",
			older.Stub.LoadedFirstItemID, older.Stub.UnshippedBefore, ids[11])
	}

	// "after" extends the other way.
	newer, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID:    run,
		LoadedFirstItemID: ids[5],
		LoadedLastItemID:  ids[6],
		Direction:         ActivityRunMembersAfter,
		Limit:             3,
	})
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if got := memberIDs(newer); len(got) != 3 || got[0] != ids[7] || got[2] != ids[9] {
		t.Fatalf("after = %v, want only the new rows %s..%s", got, ids[7], ids[9])
	}

	// "around" REPLACES the span: the rows the caller held become
	// unshipped and are described by the stub.
	around, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID:    run,
		LoadedFirstItemID: ids[19],
		LoadedLastItemID:  ids[20],
		Direction:         ActivityRunMembersAround,
		AroundItemID:      ids[6],
		Limit:             4,
	})
	if err != nil {
		t.Fatalf("around: %v", err)
	}
	if got := memberIDs(around); len(got) != 4 || got[0] != ids[4] || got[3] != ids[7] {
		t.Fatalf("around = %v, want %s..%s", got, ids[4], ids[7])
	}
	if around.Stub.UnshippedAfter != 13 {
		t.Errorf("stub unshipped after = %d, want the 13 members past %s",
			around.Stub.UnshippedAfter, ids[7])
	}

	// Limit 0 mounts nothing and re-describes the span the caller holds.
	refreshed, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID:    run,
		LoadedFirstItemID: ids[4],
		LoadedLastItemID:  ids[7],
		Direction:         ActivityRunMembersBefore,
		Limit:             0,
	})
	if err != nil {
		t.Fatalf("stub refresh: %v", err)
	}
	if len(refreshed.Items) != 0 {
		t.Errorf("stub refresh shipped %d rows, want none", len(refreshed.Items))
	}
	if refreshed.Stub.LoadedFirstItemID != ids[4] || refreshed.Stub.LoadedLastItemID != ids[7] {
		t.Errorf("refreshed span = %s..%s, want %s..%s",
			refreshed.Stub.LoadedFirstItemID, refreshed.Stub.LoadedLastItemID, ids[4], ids[7])
	}
	if refreshed.Stub.UnshippedBefore != 3 || refreshed.Stub.UnshippedAfter != 13 {
		t.Errorf("refreshed unshipped = %d/%d, want 3/13",
			refreshed.Stub.UnshippedBefore, refreshed.Stub.UnshippedAfter)
	}
}

// TestListActivityRunMembersRefusesAStaleRun: every way the caller's
// picture of the run can have gone stale is an error the pane follows
// with a window reload, never a partial answer whose counts would be
// wrong in a way nothing downstream could detect.
func TestListActivityRunMembersRefusesAStaleRun(t *testing.T) {
	s := newTestStore(t)
	ids := seedRunThread(t, s, "t", runSpec(6))
	run := ids[1]

	cases := []struct {
		name string
		req  ActivityRunMembersRequest
	}{
		{
			name: "run id is not a row of this thread",
			req:  ActivityRunMembersRequest{RunFirstItemID: "ghost", Direction: ActivityRunMembersBefore, Limit: 2},
		},
		{
			name: "run id is a prose row",
			req:  ActivityRunMembersRequest{RunFirstItemID: ids[0], Direction: ActivityRunMembersBefore, Limit: 2},
		},
		{
			name: "run does not start there",
			req:  ActivityRunMembersRequest{RunFirstItemID: ids[3], Direction: ActivityRunMembersBefore, Limit: 2},
		},
		{
			name: "loaded span names a row outside the run",
			req: ActivityRunMembersRequest{
				RunFirstItemID:    run,
				LoadedFirstItemID: ids[0],
				LoadedLastItemID:  ids[2],
				Direction:         ActivityRunMembersBefore,
				Limit:             2,
			},
		},
		{
			name: "loaded span is inverted",
			req: ActivityRunMembersRequest{
				RunFirstItemID:    run,
				LoadedFirstItemID: ids[4],
				LoadedLastItemID:  ids[2],
				Direction:         ActivityRunMembersBefore,
				Limit:             2,
			},
		},
		{
			name: "around names a row outside the run",
			req: ActivityRunMembersRequest{
				RunFirstItemID: run,
				Direction:      ActivityRunMembersAround,
				AroundItemID:   ids[0],
				Limit:          2,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.ListActivityRunMembers(context.Background(), "t", tc.req); !errors.Is(err, ErrActivityRunStale) {
				t.Fatalf("err = %v, want ErrActivityRunStale", err)
			}
		})
	}

	t.Run("unknown direction", func(t *testing.T) {
		_, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
			RunFirstItemID: run, Direction: "sideways", Limit: 2,
		})
		if err == nil || !strings.Contains(err.Error(), "sideways") {
			t.Fatalf("err = %v, want a direction error", err)
		}
	})

	t.Run("limit outside the cap", func(t *testing.T) {
		for _, limit := range []int{-1, maxActivityRunMemberLimit + 1} {
			_, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
				RunFirstItemID: run, Direction: ActivityRunMembersBefore, Limit: limit,
			})
			if err == nil {
				t.Fatalf("limit %d: expected an error", limit)
			}
		}
	})
}

// TestHeldWindowFoldsRunStubs is the §5 round trip: a client that holds a
// page's shipped rows and folds its stubs describes the same range the
// server re-derives, so a reopen after an unrelated write costs no page —
// and a write to a member the page never shipped still costs one.
func TestHeldWindowFoldsRunStubs(t *testing.T) {
	s := newTestStore(t)
	seedRunThread(t, s, "t", runSpec(30))

	page, err := s.ListThreadSliceAround(context.Background(), "t", "", 200, 10, TimelineSelection{})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	stub := onlyRun(t, page)
	if stub.UnshippedBefore == 0 {
		t.Fatal("this case needs a run the page did not ship whole")
	}
	held := heldWindowFromPage(t, page)
	if held.Count != 32 {
		t.Fatalf("folded count = %d, want the 32 rows in the range", held.Count)
	}

	// A write the window cannot render moves the thread stamp and nothing
	// else; the folded window still IS the read.
	stale := historyStampOf(t, s, "t")
	notification := Item{
		ID: "plan-note", ThreadID: "t", TurnIndex: 1, ItemIndex: 0,
		Kind: "notification", Role: "system", ToolName: "plan_update",
		Status: "completed", Summary: "plan", CreatedAt: 999,
	}
	if err := s.InsertItem(notification); err != nil {
		t.Fatalf("insert plan_update notification: %v", err)
	}
	got, err := s.SyncThreadWindow(context.Background(), "t", "", 200, 10, stale, &held, TimelineSelection{})
	if err != nil {
		t.Fatalf("sync a folded window: %v", err)
	}
	if got.Status != SyncFresh || got.Page != nil {
		t.Fatalf("folded window: status = %q page=%v, want a page-less fresh",
			got.Status, got.Page != nil)
	}

	// A write to an UNSHIPPED member changes that member's rev, which the
	// stub's digest folded in. The window must stop verifying.
	summary := "edited"
	if _, err := s.UpdateItemFields("t", stub.FirstItemID, ItemPartialUpdate{Summary: &summary}); err != nil {
		t.Fatalf("edit an unshipped member: %v", err)
	}
	stale = historyStampOf(t, s, "t")
	stale.Rev--
	got, err = s.SyncThreadWindow(context.Background(), "t", "", 200, 10, stale, &held, TimelineSelection{})
	if err != nil {
		t.Fatalf("sync after editing an unshipped member: %v", err)
	}
	if got.Status == SyncFresh {
		t.Fatal("an edited unshipped member left the window verifying as fresh")
	}
	if got.Page == nil {
		t.Fatal("the refused window got no page")
	}
}
