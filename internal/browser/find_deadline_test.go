package browser

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A start on the shown suspended page waits for its restore only within the
// action's deadline: past it the start answers the deadline while the
// restore goes on, and the page comes back when the restore finishes.
func TestFindStartStopsWaitingForARestoreAtItsDeadline(t *testing.T) {
	var gate atomic.Pointer[chan struct{}]
	started := make(chan struct{}, 1)
	f := newFindFixtureWith(t, addr("alpha"), func(m *Manager) {
		m.findTimeout = 50 * time.Millisecond
		m.engine.(*fakeEngine).beforeNewPage = func() {
			if held := gate.Load(); held != nil {
				started <- struct{}{}
				<-*held
			}
		}
	})
	suspendTab(t, f.manager, f.access, f.pageID)
	held := make(chan struct{})
	gate.Store(&held)
	release := sync.OnceFunc(func() { gate.Store(nil); close(held) })
	defer release()
	answered := make(chan error, 1)
	go func() {
		_, err := f.manager.Find(context.Background(), f.access, PageID(f.pageID), FindStart, "alpha", f.mount, f.nextSeq())
		answered <- err
	}()
	receive(t, started, "the restore's page creation")
	select {
	case err := <-answered:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the start answered %v, want its deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start still waits for the restore past its deadline")
	}
	release()
	eventually(t, "the restore to bring the page back", func() bool {
		f.manager.mu.Lock()
		defer f.manager.mu.Unlock()
		live, _ := f.manager.livePageLocked(PageID(f.pageID))
		return live != nil
	})
	f.noCall(t)
}

// A find action waits for a live page held by another operation only within
// its deadline: past it the action answers the deadline while the page is
// still held, and its engine is never called.
func TestAFindStopsWaitingForAHeldPageAtItsDeadline(t *testing.T) {
	for _, kind := range []string{FindStart, FindNext, FindStop} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixtureWith(t, addr("alpha"), func(m *Manager) { m.findTimeout = 50 * time.Millisecond })
			if kind != FindStart {
				f.find(t, FindStart, "alpha")
				f.wantCall(t, "start alpha")
			}
			p, _, err := f.manager.lookupOwnedPage(f.access, PageID(f.pageID))
			if err != nil {
				t.Fatal(err)
			}
			p.mu.Lock()
			release := sync.OnceFunc(p.mu.Unlock)
			defer release()
			answered := make(chan error, 1)
			go func() {
				_, err := f.manager.Find(context.Background(), f.access, PageID(f.pageID), kind, "alpha", f.mount, f.seqFor(kind))
				answered <- err
			}()
			select {
			case err := <-answered:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("the %s answered %v, want its deadline", kind, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the %s still waits for the held page past its deadline", kind)
			}
			release()
			f.noCall(t)
		})
	}
}
