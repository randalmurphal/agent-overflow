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
//   - a Create, Rename or Remove whose path IS a current root, with a
//     directory at that path now, means that root's directory was
//     deleted and recreated — its notify watchpoint died with the
//     deletion and is never resurrected, so the roots must be
//     reinstalled even if recomputing yields an identical set
//     (forceReinstall). The parent's watch reports the Create; the
//     root's own watch reports the Remove, which is the only report
//     when the recreate lands during an install, after the root's
//     registration and before its parent's (roots install deepest
//     first);
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
// A deletion that leaves nothing at the path needs no rebuild: notify
// drops watches on its own and stale extra roots are harmless —
// recreation is what must be caught, above.
func (w *workspaceWatcher) inspectEvent(ev notify.EventInfo, roots []gitops.WatchRoot) {
	if w.rootsFn == nil || w.forceReinstall {
		return // forceReinstall implies needsRebuild; nothing stronger to learn
	}
	path := ev.Path()
	// Only Create/Rename can introduce a directory the current watches
	// don't cover: a write can't mint one, and removals need no rebuild.
	dirBearing := ev.Event()&(notify.Create|notify.Rename) != 0

	if ev.Event()&(notify.Create|notify.Rename|notify.Remove) != 0 && rootIndexByPath(roots, path) >= 0 {
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
		case gitops.KindSubtree:
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

// maxRebuildReinstalls bounds the reinstalls one rebuild makes. When the
// roots recomputed after the last of them still differ from the installed
// set, or a reinstall is still forced, the watches stay as installed, the
// cap is logged, and the rebuild reports itself unfinished.
const maxRebuildReinstalls = 3

// maybeRebuildWatches recomputes the watch roots while needsRebuild is
// set, and reinstalls them when they differ from the installed set or a
// reinstall is forced. Every install leaves needsRebuild set, and forces
// another when a root was replaced during it (installWatches), so the
// roots are recomputed after it and reinstalled until a recompute matches
// what is installed: a directory created while the watches were out
// raised no event, and only that recompute finds it.
//
// unfinished reports that the rebuild ended with needsRebuild still set:
// a recompute failed, or the cap was hit. The watches it left (the ones
// already installed, or the ones a failed recompute found) stay. The
// caller has to run the rebuild again itself: the tree that needs
// (re)watching may never produce another event.
func (w *workspaceWatcher) maybeRebuildWatches() (outcome rebuildOutcome, unfinished bool) {
	if w.rootsFn == nil {
		return rebuildNone, false
	}
	outcome = rebuildNone
	for reinstalls := 0; w.needsRebuild && w.ctx.Err() == nil; reinstalls++ {
		newRoots, err := w.rootsFn()
		if err != nil {
			log.Printf("gitwatch: recomputing watch roots for %s: %v (keeping existing watches; retrying in %s)",
				w.cwd, err, w.pollInterval)
			return outcome, true
		}
		if !w.forceReinstall && slices.Equal(w.currentWatchRoots(), newRoots) {
			w.needsRebuild = false
			return outcome, false
		}
		if reinstalls == maxRebuildReinstalls {
			log.Printf("gitwatch: watch roots for %s still changing after %d reinstalls; keeping the installed watches and retrying in %s",
				w.cwd, maxRebuildReinstalls, w.pollInterval)
			return outcome, true
		}
		w.needsRebuild, w.forceReinstall = false, false
		w.stopFn(w.eventsCh)
		if err := w.installWatches(newRoots); err != nil {
			log.Printf("gitwatch: reinstalling watches for %s (%v); falling back to %s polling",
				w.cwd, err, w.pollInterval)
			return rebuildLostWatches, false
		}
		w.setWatchRoots(newRoots)
		outcome = rebuildReinstalled
	}
	return outcome, false
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
