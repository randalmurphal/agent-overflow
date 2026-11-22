package browser

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TryLock takes the lock if it is free and reports whether it did.
func (l *pageLock) TryLock() bool {
	select {
	case l.slot() <- struct{}{}:
		return true
	default:
		return false
	}
}

// A waiter on a held page lock stops at its context's end without the lock,
// one whose context already ended never takes even a free lock, and the
// lock passes to a waiter once released.
func TestPageLockWaitEndsWithItsContext(t *testing.T) {
	var l pageLock
	l.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.LockContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a wait on the held lock answered %v, want its deadline", err)
	}
	l.Unlock()
	if err := l.LockContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an ended context took the free lock: %v", err)
	}
	if !l.TryLock() {
		t.Fatal("the lock was held after a refused wait")
	}
	got := make(chan error, 1)
	go func() { got <- l.LockContext(context.Background()) }()
	l.Unlock()
	if err := <-got; err != nil {
		t.Fatalf("a waiter did not take the released lock: %v", err)
	}
	l.Unlock()
}

// An agent operation waiting for a page another operation holds stops
// waiting when its caller's context ends (lockPageForOperation).
func TestAnOperationStopsWaitingForAHeldPageWithItsContext(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = manager.Close() })
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := manager.Snapshot(ctx, access, PageID(page.ID))
		answered <- err
	}()
	select {
	case err := <-answered:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the snapshot answered %v, want its caller's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the snapshot still waits for the held page past its caller's deadline")
	}
}

// A suspend of a thread's pages and an asset bundle stop waiting for a page
// another operation holds when their caller's context ends.
func TestSuspendAndBundleStopWaitingForAHeldPageWithTheirContext(t *testing.T) {
	m, engine := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	driver := &assetDriver{assets: []AssetInfo{{URL: "https://example.test/a.css", Kind: "stylesheet", Name: "a.css"}}, body: "a"}
	engine.mu.Lock()
	engine.wrapDriver = func(inner pageDriver) pageDriver {
		driver.pageDriver = inner
		return driver
	}
	engine.mu.Unlock()
	page := mustOpen(t, m, access, "https://example.test/")
	listed, err := m.Assets(t.Context(), access, AssetOptions{PageID: PageID(page.ID), Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	inventory := listed.(AssetInventory)
	p, _, err := m.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, call := range map[string]func(context.Context) error{
		"suspend": func(ctx context.Context) error { return m.SuspendThread(ctx, access.ThreadID()) },
		"bundle": func(ctx context.Context) error {
			_, err := m.Assets(ctx, access, AssetOptions{PageID: PageID(page.ID), Action: "bundle", InventoryID: inventory.ID, Kinds: []string{"stylesheet"}})
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		answered := make(chan error, 1)
		go func() { answered <- call(ctx) }()
		select {
		case err := <-answered:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the %s answered %v, want its caller's deadline", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the %s still waits for the held page past its caller's deadline", name)
		}
		cancel()
	}
	if tab, ok := findTab(m, access, page.ID); !ok || tab.Suspended {
		t.Fatalf("the held page is %+v, want it live", tab)
	}
}
