package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
)

// The page's run window and the on-demand members that load past it
// (docs/architecture/timeline-window-pages.md §2-§3), from the app's side:
// the shape a caller asks for, the byte ceiling it sets, and what the
// response says about the span the caller ends up holding.

// railRunShape is one activity run of `members` rows, alternating launch
// and completion so the members carry the payload weight a real run's do.
func railRunShape(members int) []heavyThreadRow {
	rows := make([]heavyThreadRow, 0, members)
	for i := range members {
		if i%2 == 0 {
			rows = append(rows, heavyThreadRow{kind: "tool_call", inputBytes: 90})
			continue
		}
		rows = append(rows, heavyThreadRow{
			kind: "tool_completion", previewBytes: 600, patchBytes: 400, spanBytes: 200,
		})
	}
	return rows
}

// seedLongRunThread seeds prose, one run of `members` rail rows, then
// prose again, so the run has a boundary on both sides and the page has
// units either side of it to walk.
func seedLongRunThread(t *testing.T, app *App, members int) (store.Thread, []string) {
	t.Helper()
	thread, err := createTestThread(t, app, "claude", "/tmp/w-long-run", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	memberIDs := make([]string, 0, members)
	index := 0
	insert := func(id, kind, role string, summaryBytes int) {
		t.Helper()
		if err := app.store.InsertItem(store.Item{
			ID: id, ThreadID: thread.ID, TurnIndex: 0, ItemIndex: index,
			Kind: kind, Role: role, Status: "completed", ToolName: "Bash",
			Summary:   fixtureText(index, summaryBytes),
			CreatedAt: int64(index) * 1000, UpdatedAt: int64(index) * 1000,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		index++
	}
	for i := range 2 {
		insert(fmt.Sprintf("prose-before-%02d", i), "assistant_text", "assistant", 200)
	}
	for i := range members {
		id := fmt.Sprintf("member-%04d", i)
		insert(id, "tool_call", "assistant", 2000)
		memberIDs = append(memberIDs, id)
	}
	for i := range 2 {
		insert(fmt.Sprintf("prose-after-%02d", i), "assistant_text", "assistant", 200)
	}
	return thread, memberIDs
}

func pageMemberIDs(page store.PagedItems, prefix string) []string {
	ids := []string{}
	for _, item := range page.Items {
		if strings.HasPrefix(item.ID, prefix) {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// A jump into the middle of a 500-member run: the page ships the run
// window centered on the anchor, not the run's newest members, and the
// byte trim keeps the anchor whatever else it drops. Landing a jump on a
// row the page did not ship leaves the pane with nothing to scroll to.
func TestListThreadSliceAround_AnchorInsideALongRun(t *testing.T) {
	app := newTestAppWithStore(t)
	const members, runWindow = 500, 30
	thread, memberIDs := seedLongRunThread(t, app, members)
	anchor := memberIDs[250]

	page, err := app.ListThreadSliceAround(context.Background(), thread.ID, anchor, 200, TimelinePageOptions{PageShape: PageShape{RunWindowRows: runWindow}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround: %v", err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("page carries %d run stubs, want the one run", len(page.Runs))
	}
	stub := page.Runs[0]
	if stub.MemberCount != members {
		t.Errorf("stub counts %d members, want %d", stub.MemberCount, members)
	}
	shipped := pageMemberIDs(page, "member-")
	if len(shipped) != runWindow {
		t.Fatalf("page ships %d of the run's members, want the %d-row window",
			len(shipped), runWindow)
	}
	// Centered on the anchor: the store starts the window half a window
	// before it, so the anchor sits in the middle rather than at an edge.
	if shipped[0] != memberIDs[250-runWindow/2] || shipped[len(shipped)-1] != memberIDs[250+runWindow/2-1] {
		t.Errorf("run window is %s..%s, want it centered on %s",
			shipped[0], shipped[len(shipped)-1], anchor)
	}
	if stub.UnshippedBefore+stub.UnshippedAfter+len(shipped) != members {
		t.Errorf("stub accounts for %d+%d members beside %d shipped, want %d in all",
			stub.UnshippedBefore, stub.UnshippedAfter, len(shipped), members)
	}

	// The same page under a ceiling a fraction of the window's size. The
	// anchor survives; what it loses folds into the stub rather than out
	// of the page's range.
	trimmed, err := app.ListThreadSliceAround(context.Background(), thread.ID, anchor, 200,
		TimelinePageOptions{PageShape: PageShape{RunWindowRows: runWindow, MaxBytes: 8 << 10}})
	if err != nil {
		t.Fatalf("ListThreadSliceAround(trimmed): %v", err)
	}
	trimmedMembers := pageMemberIDs(trimmed, "member-")
	if len(trimmedMembers) >= runWindow {
		t.Fatalf("the byte ceiling admitted %d members; the fixture is not over budget",
			len(trimmedMembers))
	}
	if !containsItem(trimmed.Items, anchor) {
		t.Fatalf("anchor %s was trimmed off its own page; shipped %v", anchor, trimmedMembers)
	}
	if len(trimmed.Runs) != 1 {
		t.Fatalf("trimmed page carries %d run stubs, want the one run", len(trimmed.Runs))
	}
	trimmedStub := trimmed.Runs[0]
	if trimmedStub.UnshippedBefore+trimmedStub.UnshippedAfter+len(trimmedMembers) != members {
		t.Errorf("after the trim the stub accounts for %d+%d beside %d shipped, want %d in all",
			trimmedStub.UnshippedBefore, trimmedStub.UnshippedAfter, len(trimmedMembers), members)
	}
	if trimmedStub.LoadedFirstItemID != trimmedMembers[0] ||
		trimmedStub.LoadedLastItemID != trimmedMembers[len(trimmedMembers)-1] {
		t.Errorf("stub span %s..%s does not describe the rows the page shipped (%s..%s)",
			trimmedStub.LoadedFirstItemID, trimmedStub.LoadedLastItemID,
			trimmedMembers[0], trimmedMembers[len(trimmedMembers)-1])
	}
}

// --- on-demand members ------------------------------------------------

func memberItemIDs(members store.ActivityRunMembers) []string {
	ids := make([]string, 0, len(members.Items))
	for _, item := range members.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// The rows a members response carries are projected like a page's, and
// the preference rides the request the same way.
func TestListActivityRunMembers_ProjectsUnderTheCallersPreference(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, railRunShape(60))

	off, err := app.ListActivityRunMembers(context.Background(), thread.ID, ActivityRunMembersRequest{
		RunFirstItemID: "item-0000",
		Direction:      store.ActivityRunMembersBefore,
		Limit:          20,
	})
	if err != nil {
		t.Fatalf("ListActivityRunMembers: %v", err)
	}
	if len(off.Items) != 20 {
		t.Fatalf("response carries %d members, want the 20 asked for", len(off.Items))
	}
	if got := countPreviewPatches(off.Items); got != 0 {
		t.Errorf("%d preview patches rode to a client that renders none of them", got)
	}
	if off.Stub.MemberCount != 60 || off.Stub.UnshippedBefore != 40 {
		t.Errorf("stub = %d members with %d before the span, want 60 with 40",
			off.Stub.MemberCount, off.Stub.UnshippedBefore)
	}

	on, err := app.ListActivityRunMembers(context.Background(), thread.ID, ActivityRunMembersRequest{
		RunFirstItemID: "item-0000",
		Direction:      store.ActivityRunMembersBefore,
		Limit:          20,
		Shape:          PageShape{InlinePreviews: true},
	})
	if err != nil {
		t.Fatalf("ListActivityRunMembers(previews on): %v", err)
	}
	if countPreviewPatches(on.Items) == 0 {
		t.Fatal("a client that asked for inline previews received none")
	}
	if len(on.Items) != len(off.Items) {
		t.Errorf("member counts diverged: %d vs %d — the preference must change fields, not rows",
			len(on.Items), len(off.Items))
	}
}

// Over the caller's byte ceiling the response is fetched again at the
// limit that fits, so the stub still describes the span the caller ends
// up holding. A stub edited in the app would be arithmetic nobody
// derived from the database.
func TestListActivityRunMembers_ByteCeilingShrinksTheAnswerNotTheStub(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, railRunShape(60))

	full, err := app.ListActivityRunMembers(context.Background(), thread.ID, ActivityRunMembersRequest{
		RunFirstItemID: "item-0000",
		Direction:      store.ActivityRunMembersBefore,
		Limit:          20,
	})
	if err != nil {
		t.Fatalf("ListActivityRunMembers: %v", err)
	}
	bounded, err := app.ListActivityRunMembers(context.Background(), thread.ID, ActivityRunMembersRequest{
		RunFirstItemID: "item-0000",
		Direction:      store.ActivityRunMembersBefore,
		Limit:          20,
		Shape:          PageShape{MaxBytes: 6 << 10},
	})
	if err != nil {
		t.Fatalf("ListActivityRunMembers(bounded): %v", err)
	}
	if len(bounded.Items) == 0 || len(bounded.Items) >= len(full.Items) {
		t.Fatalf("ceiling admitted %d of %d members; the fixture is not over budget",
			len(bounded.Items), len(full.Items))
	}
	if spent := spentBytes(bounded.Items); spent > 6<<10 {
		t.Errorf("response is %d bytes, over the %d ceiling asked for", spent, 6<<10)
	}
	// "before" with no span held takes the run's newest members, so the
	// ones nearest the reader are what a smaller answer keeps.
	ids := memberItemIDs(bounded)
	wantLast := memberItemIDs(full)[len(full.Items)-1]
	if ids[len(ids)-1] != wantLast {
		t.Errorf("bounded answer ends at %s, want the run's newest member %s", ids[len(ids)-1], wantLast)
	}
	if bounded.Stub.LoadedFirstItemID != ids[0] || bounded.Stub.LoadedLastItemID != ids[len(ids)-1] {
		t.Errorf("stub span %s..%s does not describe the %s..%s it answered with",
			bounded.Stub.LoadedFirstItemID, bounded.Stub.LoadedLastItemID, ids[0], ids[len(ids)-1])
	}
	if bounded.Stub.UnshippedBefore != 60-len(ids) || bounded.Stub.UnshippedAfter != 0 {
		t.Errorf("stub counts %d/%d unshipped around a %d-row span, want %d/0",
			bounded.Stub.UnshippedBefore, bounded.Stub.UnshippedAfter, len(ids), 60-len(ids))
	}
}

// A jump into a run's unshipped region asks for members "around" the
// target. The byte ceiling must not be what loses it: the answer
// narrows around the same member the caller named.
func TestListActivityRunMembers_AroundKeepsTheTargetUnderTheCeiling(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, railRunShape(60))
	const target = "item-0020"

	bounded, err := app.ListActivityRunMembers(context.Background(), thread.ID, ActivityRunMembersRequest{
		RunFirstItemID:    "item-0000",
		LoadedFirstItemID: "item-0055",
		LoadedLastItemID:  "item-0059",
		Direction:         store.ActivityRunMembersAround,
		AroundItemID:      target,
		Limit:             20,
		Shape:             PageShape{MaxBytes: 6 << 10},
	})
	if err != nil {
		t.Fatalf("ListActivityRunMembers(around): %v", err)
	}
	ids := memberItemIDs(bounded)
	if len(ids) == 0 || len(ids) >= 20 {
		t.Fatalf("ceiling admitted %d of 20 members; the fixture is not over budget", len(ids))
	}
	if !containsItem(bounded.Items, target) {
		t.Fatalf("the jump target %s is not in the answer %v", target, ids)
	}
	if spent := spentBytes(bounded.Items); spent > 6<<10 {
		t.Errorf("response is %d bytes, over the %d ceiling asked for", spent, 6<<10)
	}
	// The span the caller was holding is replaced, so the stub describes
	// the new one and counts the old rows as unshipped again.
	if bounded.Stub.LoadedFirstItemID != ids[0] || bounded.Stub.LoadedLastItemID != ids[len(ids)-1] {
		t.Errorf("stub span %s..%s does not describe the rows returned (%s..%s)",
			bounded.Stub.LoadedFirstItemID, bounded.Stub.LoadedLastItemID, ids[0], ids[len(ids)-1])
	}
	if bounded.Stub.UnshippedBefore+bounded.Stub.UnshippedAfter+len(ids) != 60 {
		t.Errorf("stub accounts for %d+%d beside %d returned, want 60 in all",
			bounded.Stub.UnshippedBefore, bounded.Stub.UnshippedAfter, len(ids))
	}
}

// A caller whose picture of the run has gone stale gets a refusal it can
// show a person and act on, not a partial answer whose counts are wrong
// in a way nothing downstream could detect.
func TestListActivityRunMembers_ReportsAStaleRun(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := seedHeavyThread(t, app, railRunShape(20))

	_, err := app.ListActivityRunMembers(context.Background(), thread.ID, ActivityRunMembersRequest{
		RunFirstItemID: "item-0004",
		Direction:      store.ActivityRunMembersBefore,
		Limit:          5,
	})
	if err == nil {
		t.Fatal("a run that does not start at the id the caller named was answered anyway")
	}
	if !errors.Is(err, store.ErrActivityRunStale) {
		t.Errorf("error %v does not carry the store's diagnosis", err)
	}
	code, message, public := errorsx.PublicDetails(err)
	if !public || code != ActivityRunStaleCode {
		t.Errorf("error %q does not carry the public code %q the pane branches on (got %q)", err, ActivityRunStaleCode, code)
	}
	if message != activityRunStaleMessage {
		t.Errorf("public message %q is not the sentence a person is shown", message)
	}
}
