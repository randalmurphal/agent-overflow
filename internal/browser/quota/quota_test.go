package quota

import (
	"errors"
	"sync"
	"testing"
)

// A reservation holds every pool or none: a pool without room refuses it
// and leaves the others as they were.
func TestAReservationHoldsEveryPoolOrNone(t *testing.T) {
	workspace, global := NewPool("workspace downloads", 10), NewPool("browser artifacts", 10)
	global.SetUsed(8)
	r, err := Reserve(5, workspace, global)
	var full *FullError
	if r != nil || !errors.As(err, &full) || full.Pool != "browser artifacts" {
		t.Fatalf("reserve = %v, %v; want the artifact pool's refusal", r, err)
	}
	if workspace.Used() != 0 || global.Used() != 8 {
		t.Fatalf("a refusal left %d and %d charged", workspace.Used(), global.Used())
	}
}

// A settle keeps the actual bytes, a release returns them, and only the
// first of either acts.
func TestAReservationEndsOnce(t *testing.T) {
	pool := NewPool("p", 100)
	r, err := Reserve(40, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Settle(5) || r.Release() || r.Settle(7) {
		t.Fatal("a reservation ended more than once")
	}
	if pool.Used() != 5 {
		t.Fatalf("%d charged, want the settled 5", pool.Used())
	}
	r, _ = Reserve(40, pool)
	if !r.Release() || pool.Used() != 5 {
		t.Fatalf("%d charged after a release, want 5", pool.Used())
	}
	var none *Reservation
	if none.Settle(1) || none.Release() {
		t.Fatal("a nil reservation acted")
	}
}

// Take moves a reservation to its new holder and leaves the old one none.
func TestTakeLeavesTheSourceEmpty(t *testing.T) {
	pool := NewPool("p", 100)
	held, _ := Reserve(10, pool)
	moved := Take(&held)
	if held != nil || moved == nil {
		t.Fatal("the reservation was not moved")
	}
	if held.Release() {
		t.Fatal("the emptied holder released the moved reservation")
	}
	if pool.Used() != 10 {
		t.Fatalf("%d charged, want the moved reservation's 10", pool.Used())
	}
}

// Concurrent reservations never charge past the limit, and refusals never
// leave bytes behind.
func TestConcurrentReservationsStayWithinTheLimit(t *testing.T) {
	a, b := NewPool("a", 100), NewPool("b", 100)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 2000 {
				if r, err := Reserve(30, a, b); err == nil {
					if a.Used() > 100 || b.Used() > 100 {
						t.Errorf("charged %d, %d past the limit", a.Used(), b.Used())
					}
					r.Release()
				}
			}
		})
	}
	wg.Wait()
	if a.Used() != 0 || b.Used() != 0 {
		t.Fatalf("%d and %d charged after every release", a.Used(), b.Used())
	}
}
