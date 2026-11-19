package threadlife

import (
	"errors"
	"testing"
)

// An action bound before a retirement is refused and ends refused; one bound
// after it binds a new lifetime and is admitted; another thread's action is
// untouched.
func TestARetirementRefusesOnlyTheActionsBoundBeforeIt(t *testing.T) {
	var r Registry
	stale := r.Bind("a")
	other := r.Bind("b")
	r.Retire("a")
	fresh := r.Bind("a")
	if err := stale.Admits("a"); !errors.Is(err, ErrRetired) {
		t.Fatalf("the stale action is admitted: %v", err)
	}
	if err := fresh.Admits("a"); err != nil {
		t.Fatalf("the fresh action is refused: %v", err)
	}
	if err := other.Admits("b"); err != nil {
		t.Fatalf("another thread's action is refused: %v", err)
	}
	if err := stale.End(); !errors.Is(err, ErrRetired) {
		t.Fatalf("the stale action ended with %v", err)
	}
	if err := fresh.End(); err != nil {
		t.Fatalf("the fresh action ended with %v", err)
	}
	if err := other.End(); err != nil {
		t.Fatalf("the other action ended with %v", err)
	}
}

// A binding admits only changes to its own thread, and a nil binding admits
// everything.
func TestABindingAdmitsOtherThreadsAndNilAdmitsAll(t *testing.T) {
	var r Registry
	b := r.Bind("a")
	r.Retire("a")
	if err := b.Admits("b"); err != nil {
		t.Fatalf("a change to another thread is refused: %v", err)
	}
	var none *Binding
	if err := none.Admits("a"); err != nil {
		t.Fatalf("no binding refuses: %v", err)
	}
}

// The registry keeps a lifetime only while an action is bound to it, and an
// End called twice releases once.
func TestTheRegistryKeepsOnlyBoundLifetimes(t *testing.T) {
	var r Registry
	first, second := r.Bind("a"), r.Bind("a")
	if err := first.End(); err != nil {
		t.Fatal(err)
	}
	if err := first.End(); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Fatalf("%d lifetimes, want the one still bound", r.Len())
	}
	if err := second.End(); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 0 {
		t.Fatalf("%d lifetimes with no action bound", r.Len())
	}
	r.Retire("a")
	r.Bind("b")
	r.Retire("b")
	if r.Len() != 0 {
		t.Fatalf("%d lifetimes after the retirement", r.Len())
	}
}
