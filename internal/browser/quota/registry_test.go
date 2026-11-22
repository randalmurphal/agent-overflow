package quota

import (
	"errors"
	"testing"
)

// A key keeps its one pool while a reservation against it lives, though
// every hold let go: a hold taken again meets the bytes the reservation
// holds, and refuses past them.
func TestAKeysPoolOutlivesItsHoldsWhileAReservationLives(t *testing.T) {
	registry := NewRegistry("workspace downloads", 10)
	first := registry.Hold("a")
	r, err := Reserve(8, first.Pool())
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	second := registry.Hold("a")
	if second.Pool() != first.Pool() {
		t.Fatal("the key has a second pool while a reservation holds the first")
	}
	if _, err := Reserve(8, second.Pool()); err == nil {
		t.Fatal("a reservation past the bytes the first holds was admitted")
	}
	second.Release()
	r.Settle(3)
	if len(registry.pools) != 0 {
		t.Fatalf("%d pools remain once nothing holds them", len(registry.pools))
	}
	if third := registry.Hold("a"); third.Pool() == first.Pool() || third.Pool().Used() != 0 {
		t.Fatal("a key nothing held kept its pool")
	}
}

// Each key has a pool of its own, and a hold releases once.
func TestKeysHaveTheirOwnPools(t *testing.T) {
	registry := NewRegistry("p", 10)
	a, b := registry.Hold("a"), registry.Hold("b")
	if a.Pool() == b.Pool() {
		t.Fatal("two keys share a pool")
	}
	again := registry.Hold("a")
	if !a.Release() || a.Release() {
		t.Fatal("a hold did not release exactly once")
	}
	if len(registry.pools) != 2 {
		t.Fatalf("%d pools, want a's, still held, and b's", len(registry.pools))
	}
	again.Release()
	b.Release()
	if len(registry.pools) != 0 {
		t.Fatalf("%d pools remain", len(registry.pools))
	}
}

// A reservation against a pool its registry forgot registers it again when
// its key has no pool, and is refused when the key has a new one, so a key
// never has two.
func TestAReservationNeverMakesAKeysSecondPool(t *testing.T) {
	registry := NewRegistry("p", 10)
	stale := registry.Hold("a")
	stale.Release()
	r, err := Reserve(1, stale.Pool())
	if err != nil {
		t.Fatalf("a forgotten pool whose key is free refused: %v", err)
	}
	if got := registry.Hold("a"); got.Pool() != stale.Pool() {
		t.Fatal("the reservation did not keep its pool registered")
	}
	r.Release()

	other := NewRegistry("p", 10)
	old := other.Hold("a")
	old.Release()
	replacement := other.Hold("a")
	if _, err := Reserve(1, old.Pool()); !errors.Is(err, errReplaced) {
		t.Fatalf("a reservation against a replaced pool answered %v", err)
	}
	if replacement.Pool().Used() != 0 || old.Pool().Used() != 0 {
		t.Fatal("a refused reservation charged a pool")
	}
}
