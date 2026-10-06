package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// Exercise every relative position of a two-message group and its surrounding
// content. Swaps, head insertion and holes must obey the same ordering contract
// despite SQLite's immediate unique-index checks and a failed first commit.
func TestUserPlacementAllRelativeOrders(t *testing.T) {
	// One store holds every case, each in its own thread.
	s := newTestStore(t)
	cases := 0
	var visit func([]string, []string)
	visit = func(order, remaining []string) {
		if len(remaining) != 0 {
			for i, id := range remaining {
				next := append([]string(nil), remaining[:i]...)
				next = append(next, remaining[i+1:]...)
				visit(append(append([]string(nil), order...), id), next)
			}
			return
		}
		for _, boundary := range []string{"", "p"} {
			for _, spacing := range []int{1, 3} {
				cases++
				thread := fmt.Sprintf("t%d", cases)
				t.Run(fmt.Sprintf("%v/boundary=%s/spacing=%d", order, boundary, spacing), func(t *testing.T) {
					seedContractThread(t, s, thread)
					for i, id := range order {
						kind := "assistant_text"
						if id == "a" || id == "b" {
							kind = "user_text"
						}
						if err := s.InsertItem(placementItem(thread, id, kind, i*spacing-2)); err != nil {
							t.Fatal(err)
						}
					}
					before, err := s.ListItems(thread)
					if err != nil {
						t.Fatal(err)
					}
					group := []Item{{ID: "a"}, {ID: "b"}}
					fail := func(string, int) (string, error) { return "", errors.New("injected stamp failure") }
					if _, err := s.PlaceUserItemsAfterBoundary(thread, 0, boundary, group, fail, 20); err == nil {
						t.Fatal("expected rollback")
					}
					unchanged, err := s.ListItems(thread)
					if err != nil || !reflect.DeepEqual(unchanged, before) {
						t.Fatalf("failed transaction changed history: %v", err)
					}
					want := []string{}
					if boundary == "" {
						want = append(want, "a", "b")
					}
					for _, id := range order {
						if id == "a" || id == "b" {
							continue
						}
						want = append(want, id)
						if id == boundary {
							want = append(want, "a", "b")
						}
					}
					for attempt := 0; attempt < 2; attempt++ {
						if _, err := s.PlaceUserItemsAfterBoundary(thread, 0, boundary, group, nil, 30); err != nil {
							t.Fatal(err)
						}
						if got := placementIDs(t, s, thread); !reflect.DeepEqual(got, want) {
							t.Fatalf("attempt %d: %v, want %v", attempt, got, want)
						}
					}
				})
			}
		}
	}
	visit(nil, []string{"p", "a", "b", "q"})
}
