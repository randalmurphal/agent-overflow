package app

import (
	"log"
	"time"

	"agent-overflow/internal/store"
)

// repairStoredHistory is the sweep's history repair. It folds history the
// removed background sealing moved into "sealed:" import chunks back into
// ordinary rows, then deletes payload rows nothing references. It runs
// after the retention deletes, so it never repairs a thread the sweep is
// about to delete, and before reclaimStoreFreeSpace, so the pages it frees
// count toward this tick's freelist thresholds.
//
// Every write is a bounded store transaction followed by a passive
// checkpoint and the sweep's chunk pause. The store keeps no job state: a quit or crash leaves the remaining
// sealed chunks and orphans for the next sweep. Once nothing is left, a
// sweep pays one indexed probe and one read-only payload scan.
func (a *App) repairStoredHistory() {
	if a.store == nil || a.shuttingDown.Load() {
		return
	}
	ctx := a.lifeCtx()
	start := time.Now()

	// A cancelled lifetime is a quit, not a failure: every phase checks it
	// and the remaining work waits for the next launch.
	threads, err := a.store.SealedHistoryThreads(ctx)
	if err != nil && ctx.Err() == nil {
		log.Printf("app: history repair: list sealed threads: %v", err)
	}
	var folded store.UnsealStats
	repaired := 0
	for i, id := range threads {
		if ctx.Err() != nil || a.shuttingDown.Load() {
			break
		}
		if i > 0 {
			a.retentionPause()
		}
		stats, err := a.store.UnsealThreadHistory(ctx, id, a.retentionPause)
		folded.Chunks += stats.Chunks
		folded.Rows += stats.Rows
		folded.Payloads += stats.Payloads
		if stats.Chunks > 0 {
			repaired++
			log.Printf("app: history repair: thread %s: folded %d sealed rows and %d payloads from %d chunks",
				id, stats.Rows, stats.Payloads, stats.Chunks)
		}
		if err != nil {
			log.Printf("app: history repair: thread %s: %v", id, err)
		}
	}

	var detached int
	var orphans, pruned store.OrphanPayloadStats
	if ctx.Err() == nil && !a.shuttingDown.Load() {
		detached, err = a.store.ReleaseDetachedSealedChunks(ctx, a.retentionPause)
		if err != nil {
			log.Printf("app: history repair: release detached sealed chunks: %v", err)
		}
		orphans, err = a.store.CountOrphanPayloads(ctx)
		if err != nil {
			log.Printf("app: history repair: count orphan payloads: %v", err)
		}
	}
	if orphans.Payloads > 0 && ctx.Err() == nil && !a.shuttingDown.Load() {
		log.Printf("app: history repair: found %d orphan payloads (%d bytes)", orphans.Payloads, orphans.Bytes)
		pruned, err = a.store.PruneOrphanPayloads(ctx, a.retentionPause)
		if err != nil {
			log.Printf("app: history repair: prune orphan payloads: %v", err)
		}
	}

	if folded.Chunks+detached+pruned.Payloads == 0 {
		return
	}
	log.Printf("app: history repair: folded %d sealed rows from %d chunks in %d threads, released %d detached chunks, pruned %d orphan payloads (%d bytes) in %s",
		folded.Rows, folded.Chunks, repaired, detached, pruned.Payloads, pruned.Bytes, time.Since(start).Round(time.Millisecond))
}
