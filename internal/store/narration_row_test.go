package store

import (
	"context"
	"errors"
	"testing"
)

// A thinking block the provider marked as prose settles as assistant text
// under the same item id (triage narration.go). The settle is one
// UpsertUnsettledItem of the whole row with its payload. These tests cover
// what that write must do to every read of the row.

const narrationRowText = "quokka opens the note and the rest follows."

// seedNarrationRun seeds prose, two tools, a streaming thinking row with a
// payload, two tools and prose, and returns the ids with the thinking
// row's.
func seedNarrationRun(t *testing.T, s *Store, threadID string) (ids []string, thinkingID string) {
	t.Helper()
	ids = seedRunThread(t, s, threadID, "pttkttp")
	thinkingID = ids[3]
	if _, err := s.UpsertItem(Item{
		ID: thinkingID, ThreadID: threadID, TurnIndex: 0, Kind: "thinking", Role: "assistant",
		Status: "streaming", Summary: "the rest follows.", PayloadID: "thinking:" + thinkingID, UpdatedAt: 10,
	}, &Payload{ID: "thinking:" + thinkingID, Kind: "thinking", Meta: `{"preview":"quokka"}`, Data: []byte(narrationRowText), CreatedAt: 10}); err != nil {
		t.Fatalf("open thinking row: %v", err)
	}
	return ids, thinkingID
}

func settleAsNarration(t *testing.T, s *Store, threadID, itemID string) Item {
	t.Helper()
	row, err := s.UpsertUnsettledItem(Item{
		ID: itemID, ThreadID: threadID, TurnIndex: 0, Kind: "assistant_text", Role: "assistant",
		Status: "completed", Summary: narrationRowText, PayloadID: "thinking:" + itemID,
		Meta: `{"narration":true}`, UpdatedAt: 20,
	}, &Payload{ID: "thinking:" + itemID, Kind: "assistant_text", Meta: "{}", Data: []byte(narrationRowText), CreatedAt: 20}, nil)
	if err != nil {
		t.Fatalf("settle as narration: %v", err)
	}
	return row
}

func tailPage(t *testing.T, s *Store, threadID string) PagedItems {
	t.Helper()
	page, err := s.ListThreadSliceAround(context.Background(), threadID, "", 60, 30, TimelineSelection{})
	if err != nil {
		t.Fatalf("tail slice: %v", err)
	}
	return page
}

func narrationSearchHits(t *testing.T, s *Store, threadID string) []ThreadSearchHit {
	t.Helper()
	hits, err := s.SearchThreads("quokka", ThreadSearchFilter{ThreadIDs: []string{threadID}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return hits
}

func TestNarrationSettleMovesTheRowOutOfItsActivityRun(t *testing.T) {
	s := newTestStore(t)
	ids, thinkingID := seedNarrationRun(t, s, "t")

	before := tailPage(t, s, "t")
	if run := onlyRun(t, before); run.MemberCount != 5 || run.FirstItemID != ids[1] || run.LastItemID != ids[5] {
		t.Fatalf("run before the settle = %+v, want the five rows between the prose", run)
	}
	if hits := narrationSearchHits(t, s, "t"); len(hits) != 0 {
		t.Fatalf("a thinking row is indexed: %+v", hits)
	}

	row := settleAsNarration(t, s, "t", thinkingID)
	if row.Kind != "assistant_text" || row.Status != "completed" || row.Summary != narrationRowText ||
		row.ItemIndex != 3 || row.CreatedAt != 4 || row.PayloadKind != "assistant_text" || row.PayloadMeta != "{}" {
		t.Fatalf("settled row = %+v", row)
	}
	if data, err := s.GetPayloadData("t", row.PayloadID); err != nil || string(data) != narrationRowText {
		t.Fatalf("payload data = %q, %v", data, err)
	}

	after := tailPage(t, s, "t")
	assertPageAccounts(t, s, "t", after, ids)
	if len(after.Runs) != 2 {
		t.Fatalf("runs after the settle = %+v, want one on each side of the prose row", after.Runs)
	}
	for i, want := range [][2]string{{ids[1], ids[2]}, {ids[4], ids[5]}} {
		if run := after.Runs[i]; run.FirstItemID != want[0] || run.LastItemID != want[1] || run.MemberCount != 2 {
			t.Errorf("run %d = %s..%s of %d, want %s..%s of 2", i, run.FirstItemID, run.LastItemID, run.MemberCount, want[0], want[1])
		}
	}
	shipped := false
	for _, item := range after.Items {
		if item.ID == thinkingID {
			shipped = item.Kind == "assistant_text"
		}
	}
	if !shipped {
		t.Fatalf("the page did not ship the narration row as prose: %v", pageItemIDs(after))
	}

	// A caller still holding the old run is told it is stale; the run
	// that now starts after the prose row answers.
	_, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID: ids[1], LoadedFirstItemID: ids[1], LoadedLastItemID: ids[5],
		Direction: ActivityRunMembersBefore, Limit: 2,
	})
	if !errors.Is(err, ErrActivityRunStale) {
		t.Fatalf("members of the old run: err = %v, want ErrActivityRunStale", err)
	}
	_, err = s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID: thinkingID, Direction: ActivityRunMembersBefore, Limit: 2,
	})
	if !errors.Is(err, ErrActivityRunStale) {
		t.Fatalf("members of a run named by the prose row: err = %v, want ErrActivityRunStale", err)
	}
	members, err := s.ListActivityRunMembers(context.Background(), "t", ActivityRunMembersRequest{
		RunFirstItemID: ids[4], Direction: ActivityRunMembersAround, AroundItemID: ids[5], Limit: 2,
	})
	if err != nil || members.Stub.MemberCount != 2 || members.Stub.FirstItemID != ids[4] {
		t.Fatalf("members of the newer run = %+v, %v", members.Stub, err)
	}

	hits := narrationSearchHits(t, s, "t")
	if len(hits) != 1 || hits[0].ItemID != thinkingID || hits[0].Kind != ThreadSearchKindAssistant {
		t.Fatalf("search hits = %+v, want the narration row", hits)
	}
}

// A row another write ended keeps its kind, its text and its payload.
func TestNarrationSettleLeavesAnEndedRow(t *testing.T) {
	s := newTestStore(t)
	ids, thinkingID := seedNarrationRun(t, s, "t")
	status, summary := "errored", "the rest follows. [interrupted]"
	if _, err := s.UpdateItemFields("t", thinkingID, ItemPartialUpdate{Status: &status, Summary: &summary, IfStreaming: true}); err != nil {
		t.Fatalf("end the row: %v", err)
	}

	row := settleAsNarration(t, s, "t", thinkingID)
	if row.Kind != "thinking" || row.Status != status || row.Summary != summary || row.PayloadKind != "thinking" {
		t.Fatalf("the settle rewrote an ended row: %+v", row)
	}
	if meta, err := s.GetPayloadMeta("t", row.PayloadID); err != nil || meta.Kind != "thinking" || meta.Meta != `{"preview":"quokka"}` {
		t.Fatalf("the settle rewrote an ended row's payload: %+v, %v", meta, err)
	}
	page := tailPage(t, s, "t")
	if run := onlyRun(t, page); run.MemberCount != 5 || run.FirstItemID != ids[1] {
		t.Fatalf("run = %+v, want the row still a member", run)
	}
	if hits := narrationSearchHits(t, s, "t"); len(hits) != 0 {
		t.Fatalf("an ended thinking row is indexed: %+v", hits)
	}
}
