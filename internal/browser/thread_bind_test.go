package browser

import (
	"errors"
	"fmt"
	"testing"
)

// A bind whose lookup fails, as an app action's read of a thread with no
// row does, leaves no lifetime behind: distinct unknown ids keep nothing.
// A lifetime another lookup confirmed, before or after the failure, stays
// that action's, and so does one a tool call bound meanwhile.
func TestAFailedLookupLeavesNoLifetime(t *testing.T) {
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = m.Close() })
	missing := errors.New("no such thread")
	fails := func() (Caller, error) { return Caller{}, missing }
	for i := range 50 {
		if _, err := m.BindThread(ThreadID(fmt.Sprintf("gone-%d", i)), fails); !errors.Is(err, missing) {
			t.Fatalf("a failed lookup answered %v", err)
		}
	}
	workspace := t.TempDir()
	finds := func(id ThreadID) func() (Caller, error) {
		return func() (Caller, error) { return Caller{ThreadID: id, Workspace: workspace}, nil }
	}
	if _, err := m.BindThread("asked", finds("other")); err == nil {
		t.Fatal("a lookup naming another thread was bound")
	}
	m.mu.Lock()
	kept := len(m.threads)
	m.mu.Unlock()
	if kept != 0 {
		t.Fatalf("failed lookups left %d lifetimes", kept)
	}

	// held binds id with a lookup that waits for release and then answers
	// lookup's; it answers once the lookup is in flight.
	held := func(id ThreadID, lookup func() (Caller, error)) (release func() (Access, error)) {
		inFlight, proceed := make(chan struct{}), make(chan struct{})
		type bound struct {
			access Access
			err    error
		}
		done := make(chan bound, 1)
		go func() {
			access, err := m.BindThread(id, func() (Caller, error) {
				close(inFlight)
				<-proceed
				return lookup()
			})
			done <- bound{access, err}
		}()
		<-inFlight
		return func() (Access, error) {
			close(proceed)
			b := <-done
			return b.access, b.err
		}
	}
	confirmed := func(t *testing.T, id ThreadID, access Access, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if lifetimeOf(m, string(id)) != access.tb || access.End() != nil {
			t.Fatal("a confirmed lifetime left the registry with another lookup's failure")
		}
	}
	t.Run("the failure ends first", func(t *testing.T) {
		release := held("first", finds("first"))
		if _, err := m.BindThread("first", fails); !errors.Is(err, missing) {
			t.Fatal(err)
		}
		access, err := release()
		confirmed(t, "first", access, err)
	})
	t.Run("the failure ends last", func(t *testing.T) {
		release := held("last", fails)
		access, err := m.BindThread("last", finds("last"))
		if _, ferr := release(); !errors.Is(ferr, missing) {
			t.Fatal(ferr)
		}
		confirmed(t, "last", access, err)
	})
	t.Run("a tool call binds meanwhile", func(t *testing.T) {
		release := held("tool", fails)
		tool := m.Bind(Caller{ThreadID: "tool", Workspace: workspace})
		if _, err := release(); !errors.Is(err, missing) {
			t.Fatal(err)
		}
		confirmed(t, "tool", tool, nil)
	})
}
