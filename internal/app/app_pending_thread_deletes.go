package app

import (
	"context"
	"log"
	"sync"
)

// pendingThreadDeletes owns the boot-time walk that completes the thread
// deletes a crash or an error stopped partway.
type pendingThreadDeletes struct {
	once sync.Once
	wg   sync.WaitGroup
}

// startPendingThreadDeletes completes, in the background, every delete
// that began and did not finish (store.ListPendingThreadDeletes). Such a
// thread is already gone to every listing, read and fork, so the walk
// waits for the first client's catalog reads (awaitFirstReadsSettled) and
// deletes through the same locked, paced path as the retention sweep.
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
			a.completePendingThreadDeletes(ctx)
		}()
	})
}

// completePendingThreadDeletes deletes each pending thread under its
// action lock. A thread that fails keeps its mark and is logged; the next
// boot retries it.
func (a *App) completePendingThreadDeletes(ctx context.Context) {
	ids, err := a.store.ListPendingThreadDeletes()
	if err != nil {
		log.Printf("app: pending thread deletes: %v", err)
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
			log.Printf("app: pending thread deletes: delete thread %s: %v", id, err)
		}
	}
}

// waitPendingThreadDeletes joins the walk. Shutdown cancels the app
// context first; the walk stops before the next thread and stops pausing
// inside the current one.
func (a *App) waitPendingThreadDeletes() { a.pendingThreadDeletes.wg.Wait() }
