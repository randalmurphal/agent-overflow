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
				t.Run(fmt.Sprintf("%v/boundary=%s/spacing=%d", order, boundary, spacing), func(t *testing.T) {
					s := newTestStore(t)
					seedContractThread(t, s, "t")
					for i, id := range order {
						kind := "assistant_text"
						if id == "a" || id == "b" {
							kind = "user_text"
						}
						if err := s.InsertItem(placementItem("t", id, kind, i*spacing-2)); err != nil {
							t.Fatal(err)
						}
					}
					before, err := s.ListItems("t")
					if err != nil {
						t.Fatal(err)
					}
					group := []Item{{ID: "a"}, {ID: "b"}}
					fail := func(string, int) (string, error) { return "", errors.New("injected stamp failure") }
					if _, err := s.PlaceUserItemsAfterBoundary("t", 0, boundary, group, fail, 20); err == nil {
						t.Fatal("expected rollback")
					}
					unchanged, err := s.ListItems("t")
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
						if _, err := s.PlaceUserItemsAfterBoundary("t", 0, boundary, group, nil, 30); err != nil {
							t.Fatal(err)
						}
						if got := placementIDs(t, s, "t"); !reflect.DeepEqual(got, want) {
							t.Fatalf("attempt %d: %v, want %v", attempt, got, want)
						}
					}
				})
			}
		}
	}
	visit(nil, []string{"p", "a", "b", "q"})
}
