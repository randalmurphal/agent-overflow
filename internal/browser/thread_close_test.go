package browser

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// retiredLeft counts the pages of tb still closing and the teardowns of
// lifetimes still running.
func retiredLeft(m *Manager, tb *threadBrowser) (closing, teardowns int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.owner == tb {
				closing++
			}
		}
	}
	return closing, len(m.retiring)
}

// A thread close whose caller gives up while its pages close leaves none of
// them usable: the hold that detaches the lifetime moves every page of it to
// closing, so a navigation through the retired Access finds no page, and
// the teardown goes on without the caller. A retry waits for that same
// teardown and answers once every page is closed.
func TestACanceledThreadCloseLeavesNoUsablePage(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	// Holding a page's operation lock keeps its close's engine half waiting.
	held := livePage(t, m, access, ids[0])
	held.mu.Lock()
	release := sync.OnceFunc(held.mu.Unlock)
	t.Cleanup(release)
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.CloseThread(gone, access.ThreadID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a close whose caller gave up answered %v", err)
	}
	if pages := m.ownedPages(access.tb); len(pages) != 0 {
		t.Fatalf("%d pages of the closed lifetime are still live", len(pages))
	}
	if _, err := m.Open(t.Context(), access, addr("again"), OpenOptions{PageID: PageID(ids[1])}); err == nil {
		t.Fatal("the retired Access navigated a page of the closed lifetime")
	}
	if err := access.End(); !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("the retired Access ended with %v", err)
	}
	short, cancelShort := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelShort()
	if err := m.CloseThread(short, access.ThreadID()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a retry during the teardown answered %v, want it to wait for the teardown", err)
	}
	release()
	if err := m.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	if closing, teardowns := retiredLeft(m, access.tb); closing != 0 || teardowns != 0 {
		t.Fatalf("after the retry %d pages are closing and %d teardowns run", closing, teardowns)
	}
}

// The Manager's Close joins a thread teardown its caller stopped waiting
// for.
func TestCloseJoinsAThreadTeardown(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"))
	held := livePage(t, m, access, ids[0])
	held.mu.Lock()
	release := sync.OnceFunc(held.mu.Unlock)
	t.Cleanup(release)
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.CloseThread(gone, access.ThreadID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a close whose caller gave up answered %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close answered %v before the thread's teardown ended", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := await(t, closed, "the Manager's close"); err != nil {
		t.Fatal(err)
	}
	if closing, teardowns := retiredLeft(m, access.tb); closing != 0 || teardowns != 0 {
		t.Fatalf("after Close %d pages are closing and %d teardowns run", closing, teardowns)
	}
}
