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
// repositories no project names.

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

// prefetchPRList reads every forge project with background admission, no
// pump and no subscriber, stores each landed read and emits pulls:badge
// after each write; it holds nothing after. It reads nothing when the
// stored copy's newest read is younger than the PR update interval or a
// list pump runs (its ticks keep the copy current), and reports how many
// repositories it stored.
func (a *App) prefetchPRList(ctx context.Context) (int, error) {
	newest, err := a.store.NewestPRListReadAt()
	if err != nil {
		return 0, fmt.Errorf("read the stored lists: %w", err)
	}
	if newest > 0 && time.Since(time.UnixMilli(newest)) < a.prUpdatePollInterval() {
		return 0, nil
	}
	if prListPumpRunning(a) {
		return 0, nil
	}
	projects, err := a.forgeProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("list forge projects: %w", err)
	}
	lists, _, err := a.storedPRLists()
	if err != nil {
		return 0, err
	}
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
		if _, seen := repos[key]; !seen {
			repos[key], names[key] = repo, project
			order = append(order, key)
		}
	}
	a.prunePRListStore(current)

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
		if err == nil {
			err = a.store.PutPRListProject(store.PRListProject{RepoKey: read.key, ProjectJSON: encoded, ReadAt: now.UnixMilli()})
		}
		if err != nil {
			log.Printf("pr list: store the prefetch of %s: %v", read.key, err)
			continue
		}
		stored++
		if snapshot, err := a.storedPRListSnapshot(ctx); err == nil {
			a.emitPRListBadge(snapshot.NeedsYou)
		} else if ctx.Err() == nil {
			log.Printf("pr list: count the badge after the prefetch of %s: %v", read.key, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return stored, err
	}
	return stored, nil
}

// prListPumpRunning reports whether a list pump is live.
func prListPumpRunning(a *App) bool {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	return a.prUpdates.list != nil && !a.prUpdates.list.dead
}
