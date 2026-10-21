package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// The PR list's stored copy (store.PRListProject, one row per forge
// repository): what a subscribe and the sidebar badge answer before any
// forge read, and what the boot prefetch fills so the badge is right
// before the page is ever opened. Every landed project read replaces its
// repository's row; a tick or the prefetch deletes the rows of
// repositories no project names. One writer owns the copy at a time
// (commitPRListStore): the latest started of the boot prefetch and the
// list pumps, a pump only while it runs, so a slower read of a writer
// that lost the copy never replaces a newer read or undoes a prune.

// storedPRLists reads every stored list, by repository key, with when
// each read landed. A row that does not decode is skipped and logged: it
// is a cache the next read replaces.
func (a *App) storedPRLists() (map[string]ProjectPRList, map[string]time.Time, error) {
	records, err := a.store.ListPRListProjects()
	if err != nil {
		return nil, nil, fmt.Errorf("read the stored lists: %w", err)
	}
	lists := make(map[string]ProjectPRList, len(records))
	readAt := make(map[string]time.Time, len(records))
	for key, record := range records {
		var list ProjectPRList
		if err := json.Unmarshal(record.ProjectJSON, &list); err != nil {
			log.Printf("pr list: the stored list of %s does not decode, ignoring it: %v", key, err)
			continue
		}
		lists[key] = list
		readAt[key] = time.UnixMilli(record.ReadAt)
	}
	return lists, readAt, nil
}

// storedPRListSnapshot assembles the snapshot of the current forge
// projects from the stored copy. It is Stale when a project's stored read
// is missing or older than the PR update interval. Seq and FetchedAt are
// the caller's.
func (a *App) storedPRListSnapshot(ctx context.Context) (PRListSnapshot, error) {
	projects, projectsErr := a.forgeProjects(ctx)
	if err := ctx.Err(); err != nil {
		return PRListSnapshot{}, err
	}
	lists, readAt, err := a.storedPRLists()
	if err != nil {
		return PRListSnapshot{}, fmt.Errorf("pr list: %w", err)
	}
	snapshot := assemblePRListSnapshot(projects, projectsErr, lists)
	snapshot.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	interval := a.prUpdatePollInterval()
	for _, project := range snapshot.Projects {
		if project.Repo == (gitops.RepoReference{}) {
			continue
		}
		at, ok := readAt[project.Repo.Key()]
		if !ok || time.Since(at) >= interval {
			snapshot.Stale = true
		}
	}
	return snapshot, nil
}

// GetPRListNeedsYou is the sidebar badge before the Pull requests page
// subscribes: the needs-you rows of the stored copy for the current
// forge projects, each PR once. It reads no forge; pulls:badge carries
// the count again whenever the copy is written.
//
//ao:scope git:operate
//ao:route selected
func (a *App) GetPRListNeedsYou(ctx context.Context) (int, error) {
	if a.shuttingDown.Load() {
		return 0, ErrShuttingDown
	}
	snapshot, err := a.storedPRListSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	return snapshot.NeedsYou, nil
}

// prListWriter is a writer of the stored copy: the generation it took
// when it started (App.prUpdates.listGen) and, for a pump, the pump.
type prListWriter struct {
	gen  uint64
	pump *prListPump
}

// commitPRListStore runs write, a change to the stored copy, if w still
// owns the copy (its generation is the latest, and a pump still runs),
// then emits pulls:badge with the stored count when write changed it. It
// reports whether write ran; a superseded or stopped writer's change is
// dropped. The store lock orders the writes with the badges they emit.
func (a *App) commitPRListStore(w prListWriter, write func() (bool, error)) (bool, error) {
	if a.prUpdates.listStoreHook != nil {
		a.prUpdates.listStoreHook("enter")
	}
	a.prUpdates.listStore.Lock()
	defer a.prUpdates.listStore.Unlock()
	a.prUpdates.mu.Lock()
	owner := a.prUpdates.listGen == w.gen && (w.pump == nil || a.prUpdates.list == w.pump)
	a.prUpdates.mu.Unlock()
	if !owner {
		return false, nil
	}
	if a.prUpdates.listStoreHook != nil {
		a.prUpdates.listStoreHook("owned")
	}
	changed, err := write()
	if err != nil || !changed {
		return true, err
	}
	// The badge counts what was committed, read back with the app's
	// context: a writer's own context may end with its last subscriber.
	snapshot, err := a.storedPRListSnapshot(a.lifeCtx())
	switch {
	case err != nil && a.lifeCtx().Err() != nil:
		// Shutting down: the write committed and no client is left to
		// tell.
		return true, nil
	case err != nil:
		return true, fmt.Errorf("count the badge: %w", err)
	}
	a.emitPRListBadge(snapshot.NeedsYou)
	return true, nil
}

// storePRListRead stores one repository's landed read for writer w,
// and reports whether it was stored. A failure is logged: the copy is a
// cache the next read replaces, and the page shows the read either way.
func (a *App) storePRListRead(w prListWriter, record store.PRListProject) bool {
	stored, err := a.commitPRListStore(w, func() (bool, error) {
		return true, a.store.PutPRListProject(record)
	})
	if err != nil {
		log.Printf("pr list: store the read of %s: %v", record.RepoKey, err)
	}
	return stored && err == nil
}

// prunePRListStore deletes, for writer w, the stored lists of
// repositories no project names (current).
func (a *App) prunePRListStore(w prListWriter, current map[string]bool) {
	keep := make([]string, 0, len(current))
	for key := range current {
		keep = append(keep, key)
	}
	if _, err := a.commitPRListStore(w, func() (bool, error) {
		deleted, err := a.store.DeletePRListProjectsExcept(keep)
		return deleted > 0, err
	}); err != nil {
		log.Printf("pr list: prune the stored lists: %v", err)
	}
}

// startPRListPrefetch reads every forge project once in the background
// after the first client's reads settled, so the stored copy, and with it
// the badge, is current before the page opens (prefetchPRList).
func (a *App) startPRListPrefetch() {
	a.prUpdates.wg.Go(func() {
		ctx := a.lifeCtx()
		if err := a.awaitFirstReadsSettled(ctx); err != nil {
			return
		}
		if _, err := a.prefetchPRList(ctx); err != nil && ctx.Err() == nil {
			log.Printf("pr list: boot prefetch: %v", err)
		}
	})
}

// prefetchPRList reads, with background admission, no pump and no
// subscriber, every forge repository whose stored read is missing or
// older than the PR update interval, stores each landed read and holds
// nothing after. It takes the stored copy from any writer before it and
// owns it until a list pump starts: it reads nothing when one runs, and
// drops what lands after one started. It reports how many repositories it
// stored.
func (a *App) prefetchPRList(ctx context.Context) (int, error) {
	// The prefetch takes its own generation: a pump that stopped before
	// it started loses the copy to it, and one that starts after takes it
	// back.
	a.prUpdates.mu.Lock()
	running := a.prUpdates.list != nil && !a.prUpdates.list.dead
	var writer prListWriter
	if !running {
		a.prUpdates.listGen++
		writer.gen = a.prUpdates.listGen
	}
	a.prUpdates.mu.Unlock()
	if running {
		return 0, nil
	}
	projects, err := a.forgeProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("list forge projects: %w", err)
	}
	lists, readAt, err := a.storedPRLists()
	if err != nil {
		return 0, err
	}
	interval := a.prUpdatePollInterval()
	repos := map[string]gitops.RepoReference{}
	names := map[string]ForgeProject{}
	current := map[string]bool{}
	var order []string
	for _, project := range projects {
		repo := project.Repo
		if project.Error != "" {
			// Its stored rows stay beside the origin's failure; the
			// repository is not read until the origin reads.
			if repo, ok := storedRepoOf(lists, project.ProjectID); ok {
				current[repo.Key()] = true
			}
			continue
		}
		key := repo.Key()
		current[key] = true
		if _, seen := repos[key]; seen {
			continue
		}
		repos[key], names[key] = repo, project
		if at, ok := readAt[key]; !ok || time.Since(at) >= interval {
			order = append(order, key)
		}
	}
	a.prunePRListStore(writer, current)
	if len(order) == 0 {
		return 0, nil
	}

	results := make(chan prListRepoRead, len(order))
	round := gitops.WithPRListRound(ctx)
	go runBounded(len(order), prListReadConcurrency, func(i int) {
		read := prListRepoRead{key: order[i], repo: repos[order[i]]}
		read.rows, read.err = a.listProjectPRs(round, read.repo)
		results <- read
	})
	stored := 0
	for range order {
		read := <-results
		var detailErr *gitops.PRListDetailError
		if ctx.Err() != nil {
			continue
		}
		if read.err != nil && !errors.As(read.err, &detailErr) {
			log.Printf("pr list: prefetch of %s failed: %v", read.key, read.err)
			continue
		}
		detail := ""
		if detailErr != nil {
			fail := a.newRepoForgeFailure(read.repo, read.err)
			detail = fail.wire
			log.Printf("pr list: prefetch detail of %s failed (id: %s): %v", read.key, fail.correlationID, read.err)
		}
		now := time.Now()
		project := names[read.key]
		encoded, err := json.Marshal(prListReadOf(project.ProjectID, project.Name, read.repo, read.rows, now, detail))
		if err != nil {
			log.Printf("pr list: encode the prefetch of %s: %v", read.key, err)
			continue
		}
		if a.storePRListRead(writer, store.PRListProject{RepoKey: read.key, ProjectJSON: encoded, ReadAt: now.UnixMilli()}) {
			stored++
		}
	}
	if err := ctx.Err(); err != nil {
		return stored, err
	}
	return stored, nil
}
