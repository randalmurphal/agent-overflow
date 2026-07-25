package gitwatch

import (
	"log"
	"os"
	"path/filepath"
	"slices"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
)

// This file owns watch-root staleness: deciding, per drained event,
// whether the installed root set can have gone stale (inspectEvent),
// and recomputing/reinstalling it at the next refresh edge
// (maybeRebuildWatches). All state it touches (needsRebuild,
// forceReinstall) is run-goroutine-local.

// inspectEvent flags a watch-root rebuild when the event indicates the
// root set may be stale:
//   - a Create/Rename whose path IS a current root means that root's
//     directory was deleted and recreated — its notify watchpoint died
//     with the deletion and is never resurrected, so the roots must be
//     reinstalled even if recomputing yields an identical set
//     (forceReinstall);
//   - a .gitignore change anywhere moves the pruned-subtree boundaries;
//   - under a KindGitMeta root, an index / exclude / config write moves
//     boundaries too (`git add -f`, info/exclude edits, core.excludesFile
//     re-pointing). index.lock churn deliberately does not trigger: git
//     creates the lock speculatively even for reads, and a real index
//     update always ends in a rename onto "index";
//   - an event for a root's TriggerFile (the global ignore file, watched
//     via its parent directory) may change ignore rules;
//   - a directory appearing directly under a KindAncestor or
//     KindGitMeta root is covered by no existing root, so its future
//     contents would otherwise go unwatched.
//
// Deletions need no rebuild: notify drops watches on its own and stale
// extra roots are harmless — recreation is what must be caught, above.
func (w *workspaceWatcher) inspectEvent(ev notify.EventInfo, roots []gitops.WatchRoot) {
	if w.rootsFn == nil || w.forceReinstall {
		return // forceReinstall implies needsRebuild; nothing stronger to learn
	}
	path := ev.Path()
	// Only Create/Rename can introduce a directory the current watches
	// don't cover: a write can't mint one, and removals need no rebuild.
	dirBearing := ev.Event()&(notify.Create|notify.Rename) != 0

	if dirBearing && rootIndexByPath(roots, path) >= 0 {
		if info, err := os.Lstat(path); err == nil && info.IsDir() {
			w.needsRebuild = true
			w.forceReinstall = true
		}
		return
	}
	if w.needsRebuild {
		return
	}
	base := filepath.Base(path)
	if base == ".gitignore" {
		w.needsRebuild = true
		return
	}
	parent := filepath.Dir(path)
	for _, root := range roots {
		if root.Path != parent {
			continue
		}
		if root.TriggerFile != "" && base == root.TriggerFile {
			w.needsRebuild = true
			return
		}
		switch root.Kind {
		case gitops.KindGitMeta, gitops.KindAncestor:
			if root.Kind == gitops.KindGitMeta &&
				(base == "index" || base == "exclude" || base == "config") {
				w.needsRebuild = true
				return
			}
			if !dirBearing {
				return
			}
			if info, err := os.Lstat(path); err == nil && info.IsDir() {
				w.needsRebuild = true
			}
		}
		return
	}
}

func rootIndexByPath(roots []gitops.WatchRoot, path string) int {
	for i, root := range roots {
		if root.Path == path {
			return i
		}
	}
	return -1
}

// rebuildOutcome is maybeRebuildWatches' report to the refresh edge:
// whether the watch set was left alone, successfully reinstalled (the
// watches are proven live — any polling fallback is now redundant), or
// lost to a reinstall failure (escalate to polling).
type rebuildOutcome uint8

const (
	rebuildNone rebuildOutcome = iota
	rebuildReinstalled
	rebuildLostWatches
)

// maybeRebuildWatches recomputes and reinstalls the watch roots when a
// drained event flagged them stale.
//
// Failure to *recompute* keeps the existing (still installed) watches
// AND keeps needsRebuild set: the tree that needs (re)watching may
// never produce another event, so the retry has to ride whatever
// causes the next refresh edge rather than wait for a second trigger.
func (w *workspaceWatcher) maybeRebuildWatches() rebuildOutcome {
	if !w.needsRebuild || w.rootsFn == nil || w.ctx.Err() != nil {
		return rebuildNone
	}
	newRoots, err := w.rootsFn()
	if err != nil {
		log.Printf("gitwatch: recomputing watch roots for %s: %v (keeping existing watches; retrying at next refresh)", w.cwd, err)
		return rebuildNone
	}
	force := w.forceReinstall
	w.needsRebuild, w.forceReinstall = false, false
	if !force && slices.Equal(w.currentWatchRoots(), newRoots) {
		return rebuildNone
	}
	w.stopFn(w.eventsCh)
	if err := w.installFn(newRoots, w.eventsCh); err != nil {
		log.Printf("gitwatch: reinstalling watches for %s (%v); falling back to %s polling",
			w.cwd, err, pollFallbackInterval)
		return rebuildLostWatches
	}
	w.setWatchRoots(newRoots)
	return rebuildReinstalled
}

// drainNotify empties the queued burst, inspecting each event for
// rebuild triggers on the way (the flags are sticky until the next
// refresh edge and inspectEvent short-circuits once saturated, so a
// burst stays cheap). roots is the caller's snapshot — the run
// goroutine is the only writer, so one snapshot serves the whole drain.
//
// Every iteration also samples the queue depth: a (near-)full queue
// means notify may already have dropped events — possibly including a
// rebuild trigger, which rides individual events and would be lost for
// good — so recompute AND force-reinstall pessimistically. Sampling
// per dequeue (not once at drain start) matters because a producer
// outpacing the drain can fill the queue mid-drain; drops only ever
// happen at full capacity, and the drain sees every level in between.
// With pruning active the queue rarely fills, and a spurious forced
// reinstall just re-arms the same watches.
func (w *workspaceWatcher) drainNotify(roots []gitops.WatchRoot) {
	for {
		if w.rootsFn != nil && len(w.eventsCh) >= notifyChannelSize-1 {
			w.needsRebuild = true
			w.forceReinstall = true
		}
		select {
		case ev := <-w.eventsCh:
			w.inspectEvent(ev, roots)
		default:
			return
		}
	}
}
