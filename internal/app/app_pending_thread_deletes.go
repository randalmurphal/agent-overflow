package app

import (
	"context"
	"log"
	"sync"
)

// pendingThreadDeletes owns the walks that complete thread deletes: the
// boot walk over the deletes a crash or an error stopped partway, and the
// walks over the pointer-fork holders a write released.
type pendingThreadDeletes struct {
	once sync.Once
	wg   sync.WaitGroup

	mu sync.Mutex
	// collecting is true while a walk over released holders runs; again
	// asks it for one more walk. stopped refuses new walks once shutdown
	// joins them.
	collecting, again, stopped bool
}

// startPendingThreadDeletes completes, in the background, every delete
// that began and did not finish (store.ListPendingThreadDeletes). Such a
// thread is already gone to every listing, read and fork, so the walk
// waits for the first client's catalog reads (awaitFirstReadsSettled) and
// deletes through the same locked, paced path as the retention sweep.
// From then on a write that may release a holder (store.OnHoldersReleased)
// starts a walk over the released holders (collectReleasedHolders).
func (a *App) startPendingThreadDeletes() {
	if a.store == nil {
		return
	}
	a.pendingThreadDeletes.once.Do(func() {
		a.pendingThreadDeletes.wg.Add(1)
		go func() {
			defer a.pendingThreadDeletes.wg.Done()
			ctx := a.lifeCtx()
			if err := a.awaitFirstReadsSettled(ctx); err != nil {
				return
			}
			a.completeThreadDeletes(ctx, "pending thread deletes", a.store.ListPendingThreadDeletes)
		}()
		a.store.OnHoldersReleased(a.collectReleasedHolders)
	})
}

// collectReleasedHolders deletes, in the background, the holders no fork
// reads any more (store.ListReleasedHolders). A call while a walk runs
// makes that walk run once more rather than starting another. It does
// not block: the store calls it after a commit.
func (a *App) collectReleasedHolders() {
	p := &a.pendingThreadDeletes
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	if p.collecting {
		p.again = true
		return
	}
	p.collecting = true
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx := a.lifeCtx()
		for {
			a.completeThreadDeletes(ctx, "released holders", a.store.ListReleasedHolders)
			p.mu.Lock()
			if !p.again || p.stopped || ctx.Err() != nil {
				p.collecting, p.again = false, false
				p.mu.Unlock()
				return
			}
			p.again = false
			p.mu.Unlock()
		}
	}()
}

// completeThreadDeletes deletes each thread list names under its action
// lock. A thread that fails keeps its mark and is logged; the next boot
// retries it.
func (a *App) completeThreadDeletes(ctx context.Context, walk string, list func() ([]string, error)) {
	ids, err := list()
	if err != nil {
		log.Printf("app: %s: %v", walk, err)
		return
	}
	pause := a.maintenancePause(ctx)
	for _, id := range ids {
		unlock, err := a.threadLocks().LockCtx(ctx, id)
		if err != nil {
			return
		}
		err = a.deleteThreadTreePacedLocked(id, pause)
		unlock()
		if err != nil {
			log.Printf("app: %s: delete thread %s: %v", walk, id, err)
		}
	}
}

// waitPendingThreadDeletes stops new walks and joins the running ones.
// Shutdown cancels the app context first; a walk stops before the next
// thread and stops pausing inside the current one.
func (a *App) waitPendingThreadDeletes() {
	p := &a.pendingThreadDeletes
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	if a.store != nil {
		a.store.OnHoldersReleased(nil)
	}
	p.wg.Wait()
}
