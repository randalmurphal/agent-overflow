package store

import (
	"errors"
	"reflect"
	"testing"

	"agent-overflow/internal/itemmeta"
)

func placementItem(thread, id, kind string, index int) Item {
	role := "assistant"
	if kind == "user_text" {
		role = "user"
	}
	return Item{ThreadID: thread, ID: id, TurnIndex: 0, ItemIndex: index, Kind: kind, Role: role, Status: "completed", Summary: id, Meta: `{"kept":true}`, CreatedAt: 10, UpdatedAt: 10}
}
func seedPlacement(t *testing.T, s *Store) {
	t.Helper()
	seedContractThread(t, s, "t")
	for _, row := range []Item{placementItem("t", "prefix", "assistant_text", 0), placementItem("t", "a", "user_text", 1), placementItem("t", "b", "user_text", 2), placementItem("t", "pre-echo", "assistant_text", 3)} {
		if err := s.InsertItem(row); err != nil {
			t.Fatal(err)
		}
	}
}
func placementIDs(t *testing.T, s *Store, thread string) []string {
	t.Helper()
	rows, err := s.ListItems(thread)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
func TestUserPlacementRetryFreezesBoundaryAndRebasesForkAndRevert(t *testing.T) {
	s := newTestStore(t)
	seedPlacement(t, s)
	boundary, err := s.CaptureUserPlacementBoundary("t", 0, []string{"a", "b"})
	if err != nil || boundary != "pre-echo" {
		t.Fatalf("boundary=%q err=%v", boundary, err)
	}
	group := []Item{{ID: "a"}, {ID: "b"}}
	stamp := func(meta string, index int) (string, error) {
		if index != 3 {
			t.Fatalf("resolved boundary=%d", index)
		}
		meta, err := itemmeta.MarkPromotedAtInterrupt(meta)
		if err != nil {
			return "", err
		}
		return itemmeta.MarkPromotedEchoBoundary(meta, index)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_second_place BEFORE UPDATE OF item_index ON items WHEN NEW.id = 'b' BEGIN SELECT RAISE(ABORT, 'late failure'); END`); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListItems("t")
	beforeStamp := historyStampOf(t, s, "t")
	if rows, err := s.PlaceUserItemsAfterBoundary("t", 0, boundary, group, stamp, 20); err == nil || rows != nil {
		t.Fatalf("failure returned rows=%+v err=%v", rows, err)
	}
	after, _ := s.ListItems("t")
	if !reflect.DeepEqual(before, after) || historyStampOf(t, s, "t") != beforeStamp {
		t.Fatal("partial placement escaped rollback")
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_second_place`); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItem(placementItem("t", "response", "assistant_text", 4)); err != nil {
		t.Fatal(err)
	}
	marker := placementItem("t", "later-anchor", "user_text", 5)
	marker.Meta = `{"promoted_at_interrupt":true,"promoted_echo_boundary":4}`
	if err := s.InsertItem(marker); err != nil {
		t.Fatal(err)
	}
	changed, err := s.PlaceUserItemsAfterBoundary("t", 0, boundary, group, stamp, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 4 || changed[0].ID != "a" || changed[1].ID != "b" {
		t.Fatalf("changed rows=%+v", changed)
	}
	want := []string{"prefix", "pre-echo", "a", "b", "response", "later-anchor"}
	if got := placementIDs(t, s, "t"); !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v want=%v", got, want)
	}
	anchor, _, err := s.GetThreadItem("t", "later-anchor")
	if err != nil {
		t.Fatal(err)
	}
	state, err := itemmeta.DecodePromotionState(anchor.Meta)
	if err != nil || state.EchoBoundary != 6 {
		t.Fatalf("rebased=%+v err=%v", state, err)
	}
	// Rebased later anchor still keeps its preceding response, for fork AND revert.
	mustCreateThread(t, s, "fork")
	mapping, err := s.CloneThreadHistoryBeforeItem("t", "fork", "later-anchor")
	if err != nil {
		t.Fatal(err)
	}
	forkWant := make([]string, 0, 5)
	for _, id := range want[:5] {
		forkWant = append(forkWant, mapping[id])
	}
	if got := placementIDs(t, s, "fork"); !reflect.DeepEqual(got, forkWant) {
		t.Fatalf("fork order=%v", got)
	}
	if _, _, err := s.DeleteConversationFromItem("t", "later-anchor"); err != nil {
		t.Fatal(err)
	}
	if got := placementIDs(t, s, "t"); !reflect.DeepEqual(got, want[:5]) {
		t.Fatalf("revert order=%v", got)
	}
}

func TestUserPlacementMissingPromptPrecedesResponseAndUsesCurrentPredecessor(t *testing.T) {
	for _, head := range []bool{false, true} {
		t.Run(map[bool]string{false: "middle", true: "head"}[head], func(t *testing.T) {
			s := newTestStore(t)
			seedContractThread(t, s, "t")
			boundary := ""
			if !head {
				if err := s.InsertItem(placementItem("t", "prefix", "assistant_text", 2)); err != nil {
					t.Fatal(err)
				}
				var err error
				boundary, err = s.CaptureUserPlacementBoundary("t", 0, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			response := placementItem("t", "response", "assistant_text", 3)
			if head {
				response.ItemIndex = 0
			}
			if err := s.InsertItem(response); err != nil {
				t.Fatal(err)
			}
			if !head {
				if _, err := s.db.Exec(`UPDATE items SET item_index = item_index + 10 WHERE thread_id = 't'`); err != nil {
					t.Fatal(err)
				}
			}
			prompt := placementItem("t", "new", "user_text", 999)
			rows, err := s.PlaceUserItemsAfterBoundary("t", 0, boundary, []Item{prompt}, func(meta string, index int) (string, error) {
				want := 0
				if !head {
					want = 12
				}
				if index != want {
					t.Fatalf("boundary index=%d want=%d", index, want)
				}
				return meta, nil
			}, 20)
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].ID != "new" {
				t.Fatalf("rows=%+v", rows)
			}
			want := []string{"new", "response"}
			if !head {
				want = append([]string{"prefix"}, want...)
			}
			if got := placementIDs(t, s, "t"); !reflect.DeepEqual(got, want) {
				t.Fatalf("order=%v", got)
			}
		})
	}
}

func TestUserPlacementRefusesInvalidGroupsAtomically(t *testing.T) {
	for _, problem := range []string{"duplicate", "missing-content", "wrong-turn", "wrong-thread", "nonuser", "boundary-missing", "boundary-moved", "transform", "suffix-write"} {
		t.Run(problem, func(t *testing.T) {
			s := newTestStore(t)
			seedPlacement(t, s)
			boundary := "prefix"
			group := []Item{{ID: "a"}, {ID: "b"}}
			var transform func(string, int) (string, error)
			switch problem {
			case "duplicate":
				group[1].ID = "a"
			case "missing-content":
				group[1].ID = "missing"
			case "wrong-turn":
				if _, err := s.db.Exec(`UPDATE items SET turn_index = 1 WHERE id = 'b'`); err != nil {
					t.Fatal(err)
				}
			case "wrong-thread":
				group[1] = placementItem("foreign", "missing", "user_text", 0)
			case "nonuser":
				group[1].ID = "pre-echo"
			case "boundary-missing":
				boundary = "missing"
			case "boundary-moved":
				boundary = "a"
			case "transform":
				transform = func(string, int) (string, error) { return "", errors.New("fail") }
			case "suffix-write":
				if _, err := s.db.Exec(`CREATE TRIGGER fail_suffix BEFORE UPDATE OF item_index ON items WHEN NEW.id = 'pre-echo' BEGIN SELECT RAISE(ABORT, 'suffix failure'); END`); err != nil {
					t.Fatal(err)
				}
				group = append(group, placementItem("t", "new", "user_text", 0))
			}
			before, _ := s.ListItems("t")
			stamp := historyStampOf(t, s, "t")
			if rows, err := s.PlaceUserItemsAfterBoundary("t", 0, boundary, group, transform, 20); err == nil || rows != nil {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			after, _ := s.ListItems("t")
			if !reflect.DeepEqual(before, after) || historyStampOf(t, s, "t") != stamp {
				t.Fatal("invalid placement changed history")
			}
		})
	}
}

func TestUserPlacementImportedSuffixAndBoundaryRebaseStayLocal(t *testing.T) {
	s := newTestStore(t)
	for _, thread := range []string{"a", "b"} {
		newImportTargetThread(t, s, thread)
		prefix := placementItem(thread, "prefix", "assistant_text", 0)
		response := placementItem(thread, "response", "assistant_text", 1)
		anchor := placementItem(thread, "anchor", "user_text", 2)
		anchor.Meta = `{"promoted_at_interrupt":true,"promoted_echo_boundary":1}`
		if err := s.ApplyImportBatch(thread, ImportBatch{Turns: []Turn{{ThreadID: thread, TurnID: thread + ":0", TurnIndex: 0, StartedAt: 10}}, Rows: []ImportRow{{Item: prefix}, {Item: response}, {Item: anchor}}}); err != nil {
			t.Fatal(err)
		}
	}
	untouched, _ := s.ListItems("b")
	stampB := historyStampOf(t, s, "b")
	prompt := placementItem("a", "prompt", "user_text", 0)
	// Transform failure occurs after suffix localization and boundary rebasing.
	if _, err := s.PlaceUserItemsAfterBoundary("a", 0, "prefix", []Item{prompt}, func(string, int) (string, error) { return "", errors.New("injected transform") }, 20); err == nil {
		t.Fatal("expected rollback")
	}
	var overlays int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = 'a'`).Scan(&overlays); err != nil {
		t.Fatal(err)
	}
	if overlays != 0 {
		t.Fatalf("failed insert localized %d rows", overlays)
	}
	rows, err := s.PlaceUserItemsAfterBoundary("a", 0, "prefix", []Item{prompt}, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("changed rows=%+v", rows)
	}
	anchor, _, err := s.GetThreadItem("a", "anchor")
	if err != nil {
		t.Fatal(err)
	}
	state, err := itemmeta.DecodePromotionState(anchor.Meta)
	if err != nil || state.EchoBoundary != 2 {
		t.Fatalf("anchor=%+v err=%v", state, err)
	}
	after, _ := s.ListItems("b")
	if !reflect.DeepEqual(untouched, after) || stampB != historyStampOf(t, s, "b") {
		t.Fatal("placement mutated shared source")
	}
}

func TestUserPlacementGroupSwapsAndExistingTailMoveDoNotCollide(t *testing.T) {
	s := newTestStore(t)
	seedPlacement(t, s)
	// b and a trade occupied slots before the suffix; a later retry stays stable.
	group := []Item{{ID: "b"}, {ID: "a"}}
	for n := 0; n < 2; n++ {
		if _, err := s.PlaceUserItemsAfterBoundary("t", 0, "prefix", group, nil, 20); err != nil {
			t.Fatal(err)
		}
		if got := placementIDs(t, s, "t"); !reflect.DeepEqual(got, []string{"prefix", "b", "a", "pre-echo"}) {
			t.Fatalf("swapped=%v", got)
		}
	}
	// A row currently after the displaced suffix must vacate its slot first.
	if err := s.InsertItem(placementItem("t", "tail-user", "user_text", 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceUserItemsAfterBoundary("t", 0, "prefix", []Item{{ID: "tail-user"}}, nil, 30); err != nil {
		t.Fatal(err)
	}
	if got := placementIDs(t, s, "t"); !reflect.DeepEqual(got, []string{"prefix", "tail-user", "b", "a", "pre-echo"}) {
		t.Fatalf("moved=%v", got)
	}
}

func TestUserPlacementHeadBeforeNegativeHistoryAndGapAvoidsSuffixWrites(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	for _, item := range []Item{placementItem("t", "negative", "assistant_text", -2), placementItem("t", "gap-end", "assistant_text", 10)} {
		if err := s.InsertItem(item); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.PlaceUserItemsAfterBoundary("t", 0, "", []Item{placementItem("t", "head", "user_text", 0)}, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ItemIndex != -2 {
		t.Fatalf("head index=%d", rows[0].ItemIndex)
	}
	if got := placementIDs(t, s, "t"); !reflect.DeepEqual(got, []string{"head", "negative", "gap-end"}) {
		t.Fatalf("head order=%v", got)
	}
	// The suffix's ample unused index space needs no move or emission.
	rows, err = s.PlaceUserItemsAfterBoundary("t", 0, "negative", []Item{placementItem("t", "in-gap", "user_text", 0)}, nil, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "in-gap" {
		t.Fatalf("unnecessary suffix work: %+v", rows)
	}
}
