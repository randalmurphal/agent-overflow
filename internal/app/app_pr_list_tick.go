package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// One tick of the PR list pump (app_pr_list.go): each forge project's
// read (git.Core.ListProjectPRs: the forge's fixed request set, never one
// per row), once per repository with at most prListReadConcurrency at a
// time. Each read is folded in, stored and published as it lands, in the
// pump goroutine, so frames go out in sequence order. Reads run outside
// App.prUpdates.mu.

// prListReadConcurrency bounds the project reads one tick runs at once.
// Each read is itself a few concurrent requests (3 to 7 on GitLab).
const prListReadConcurrency = 4

// prListProjectState is one repository's rows as last read: list is its
// repository-level ProjectPRList (rows, ReadAt, DetailError, and the
// project that named it), read whether it holds a read (landed or from the
// store). fail is the active failure (deduplicated like the PR pump's),
// graced a transient failure held back once while rows are on screen,
// detailFail the active detail failure.
type prListProjectState struct {
	list       ProjectPRList
	read       bool
	fail       prForgeFailure
	detailFail prForgeFailure
	graced     bool
}

func (a *App) listProjectPRs(ctx context.Context, repo gitops.RepoReference) ([]gitops.PRListRow, error) {
	if a.prUpdates.listFetchFn != nil {
		return a.prUpdates.listFetchFn(ctx, repo)
	}
	return a.gitCore().ListProjectPRs(ctx, repo)
}

// lastForgeRepo is the repository a tick before read for projectID, from
// the projects it read (forge), carried forward while its origin fails.
func lastForgeRepo(forge []ForgeProject, projectID string) (gitops.RepoReference, bool) {
	for _, project := range forge {
		if project.ProjectID == projectID && project.Repo != (gitops.RepoReference{}) {
			return project.Repo, true
		}
	}
	return gitops.RepoReference{}, false
}

// storedRepoOf is the repository of the stored list projectID wrote, for
// a project whose origin cannot be read now.
func storedRepoOf(lists map[string]ProjectPRList, projectID string) (gitops.RepoReference, bool) {
	for _, list := range lists {
		if list.ProjectID == projectID && list.Repo != (gitops.RepoReference{}) {
			return list.Repo, true
		}
	}
	return gitops.RepoReference{}, false
}

// prListRepoRead is one repository's landed read.
type prListRepoRead struct {
	key  string
	repo gitops.RepoReference
	rows []gitops.PRListRow
	err  error
}

// pollPRList runs one tick under ctx (interactive for a subscribe or a
// refresh). It reports whether a project is failing (the retry schedule)
// and the earliest rate limit release among held projects.
func (a *App) pollPRList(ctx context.Context, pump *prListPump) (bool, time.Time) {
	projects, projectsErr := a.forgeProjects(ctx)
	if ctx.Err() != nil {
		return false, time.Time{}
	}
	interactive := forgeapi.IsInteractive(ctx)
	a.prUpdates.mu.Lock()
	seeded := pump.seeded
	a.prUpdates.mu.Unlock()
	var stored map[string]ProjectPRList
	if !seeded {
		var err error
		if stored, _, err = a.storedPRLists(); err != nil {
			log.Printf("pr list: %v", err)
		}
	}

	// One read per repository, whatever number of projects (clones) name
	// it; a background read waits out its project's rate limit.
	repos := map[string]gitops.RepoReference{}
	var order []string
	current := map[string]bool{}
	projects = slices.Clone(projects)
	a.prUpdates.mu.Lock()
	if pump.dead {
		a.prUpdates.mu.Unlock()
		return false, time.Time{}
	}
	if !pump.seeded {
		pump.seeded = true
		for key, list := range stored {
			pump.projects[key] = &prListProjectState{list: list, read: true}
		}
	}
	for i, project := range projects {
		if project.Error != "" {
			// An origin this tick could not read keeps the repository read
			// for the project before, and so its rows, beside the error; it
			// is not read again until the origin reads.
			repo, ok := lastForgeRepo(pump.forge, project.ProjectID)
			if !ok {
				repo, ok = storedRepoOf(pump.listsLocked(), project.ProjectID)
			}
			if ok {
				projects[i].Repo = repo
				current[repo.Key()] = true
			}
			continue
		}
		key := project.Repo.Key()
		current[key] = true
		if _, seen := repos[key]; seen {
			continue
		}
		if state := pump.projects[key]; state != nil && !interactive && time.Now().Before(state.fail.release) {
			continue
		}
		repos[key] = project.Repo
		order = append(order, key)
	}
	pump.forge, pump.forgeErr = projects, projectsErr
	if projectsErr == nil {
		// A project removed, or whose origin moved, takes its rows along.
		for key := range pump.projects {
			if !current[key] {
				delete(pump.projects, key)
			}
		}
	}
	a.prUpdates.mu.Unlock()
	if projectsErr == nil {
		a.prunePRListStore(pump.gen, current)
	}

	results := make(chan prListRepoRead, len(order))
	round := gitops.WithPRListRound(ctx)
	go runBounded(len(order), prListReadConcurrency, func(i int) {
		read := prListRepoRead{key: order[i], repo: repos[order[i]]}
		read.rows, read.err = a.listProjectPRs(round, read.repo)
		results <- read
	})
	for range order {
		read := <-results
		if ctx.Err() == nil {
			a.landPRListRead(pump, read)
		}
	}
	if ctx.Err() != nil {
		return false, time.Time{}
	}
	a.prUpdates.mu.Lock()
	pump.landed = true
	a.prUpdates.mu.Unlock()
	_, failing, release := a.publishPRList(pump)
	return failing, release
}

// landPRListRead folds one repository's read into the pump, stores a
// successful one and publishes.
func (a *App) landPRListRead(pump *prListPump, read prListRepoRead) {
	var detailErr *gitops.PRListDetailError
	listed := read.err == nil || errors.As(read.err, &detailErr)
	a.prUpdates.mu.Lock()
	if pump.dead {
		a.prUpdates.mu.Unlock()
		return
	}
	state := pump.projects[read.key]
	if state == nil {
		state = &prListProjectState{}
		pump.projects[read.key] = state
	}
	var record []byte
	readAt := time.Now()
	switch {
	case listed:
		if detailErr == nil {
			state.detailFail = prForgeFailure{}
		} else if fail := a.newRepoForgeFailure(read.repo, read.err); state.detailFail.key != fail.key {
			state.detailFail = fail
			log.Printf("pr list: detail of %s failed (id: %s): %v", read.key, fail.correlationID, read.err)
		}
		projectID, name := pump.projectNamingLocked(read.repo)
		state.list = prListReadOf(projectID, name, read.repo, read.rows, readAt, state.detailFail.wire)
		state.read, state.fail, state.graced = true, prForgeFailure{}, false
		encoded, err := json.Marshal(state.list)
		if err != nil {
			log.Printf("pr list: encode the read of %s: %v", read.key, err)
		} else {
			record = encoded
		}
	default:
		fail := a.newRepoForgeFailure(read.repo, read.err)
		switch {
		case fail.kind == forgeFailureTransient && state.read && !state.fail.failing() && !state.graced:
			state.graced = true
			log.Printf("pr list: read of %s failed (id: %s), retrying before reporting it: %v", read.key, fail.correlationID, read.err)
		case state.fail.key != fail.key:
			state.graced = false
			state.fail = fail
			log.Printf("pr list: read of %s failed (id: %s): %v", read.key, fail.correlationID, read.err)
		default:
			state.graced = false
		}
	}
	a.prUpdates.mu.Unlock()
	if record != nil {
		a.storePRListRead(pump.gen, store.PRListProject{RepoKey: read.key, ProjectJSON: record, ReadAt: readAt.UnixMilli()})
	}
	a.publishPRList(pump)
}

// prListReadOf is a repository's landed read as the list publishes and
// stores it: each row with its attention, sorted.
func prListReadOf(projectID, name string, repo gitops.RepoReference, rows []gitops.PRListRow, readAt time.Time, detailError string) ProjectPRList {
	list := ProjectPRList{
		ProjectID: projectID, Name: name, Repo: repo, Rows: make([]PRListItem, 0, len(rows)),
		ReadAt: readAt.UTC().Format(time.RFC3339), DetailError: detailError,
	}
	for _, row := range rows {
		list.Rows = append(list.Rows, prListItemOf(row))
	}
	sortPRListItems(list.Rows)
	return list
}

// projectNamingLocked is the first project of the tick that names repo.
func (p *prListPump) projectNamingLocked(repo gitops.RepoReference) (string, string) {
	for _, project := range p.forge {
		if project.Repo == repo {
			return project.ProjectID, project.Name
		}
	}
	return "", ""
}

// listsLocked is each repository's list with its failure, by key.
func (p *prListPump) listsLocked() map[string]ProjectPRList {
	lists := make(map[string]ProjectPRList, len(p.projects))
	for key, state := range p.projects {
		list := state.list
		if list.Repo == (gitops.RepoReference{}) {
			list.Rows = []PRListItem{}
		}
		list.Error, list.ErrorKind, list.Reserve, list.ResumeAt = state.fail.wire, state.fail.kind, state.fail.reserve, state.fail.resumeAt
		list.ErrorDetails = state.fail.detail
		lists[key] = list
	}
	return lists
}

// publishPRList builds the snapshot from the pump's state and, when it
// differs from the last one, stamps, keeps and emits it. It reports the
// snapshot, whether a project is failing and the earliest rate limit
// release, for the retry schedule.
func (a *App) publishPRList(pump *prListPump) (PRListSnapshot, bool, time.Time) {
	a.prUpdates.mu.Lock()
	if pump.dead {
		a.prUpdates.mu.Unlock()
		return PRListSnapshot{}, false, time.Time{}
	}
	failing := false
	var release time.Time
	for _, state := range pump.projects {
		if state.fail.failing() || state.graced {
			failing = failing || !state.fail.rateLimited()
			if state.fail.rateLimited() && (release.IsZero() || state.fail.release.Before(release)) {
				release = state.fail.release
			}
		}
	}
	snapshot := assemblePRListSnapshot(pump.forge, pump.forgeErr, pump.listsLocked())
	snapshot.Stale = !pump.landed
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		a.prUpdates.mu.Unlock()
		log.Printf("pr list: encode the snapshot: %v", err)
		return snapshot, failing, release
	}
	if string(encoded) == string(pump.encoded) {
		snapshot = pump.snapshot
		a.prUpdates.mu.Unlock()
		return snapshot, failing, release
	}
	pump.encoded = encoded
	pump.seq = a.nextPRUpdateSeqLocked()
	snapshot.Seq = pump.seq
	snapshot.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	pump.snapshot = snapshot
	a.prUpdates.mu.Unlock()
	a.emitPumpEvent(&pump.pumpCore, eventchan.PullsUpdated, snapshot)
	return snapshot, failing, release
}

// assemblePRListSnapshot is the snapshot of projects from each
// repository's list (lists, by key): every project in the projects'
// order with its repository's rows, the projects' own read failure as
// Error, and the needs-you count, each PR once. A project whose origin
// could not be read shows that failure beside the rows its repository
// had, found by its Repo or, without one, the list it last wrote.
func assemblePRListSnapshot(projects []ForgeProject, projectsErr error, lists map[string]ProjectPRList) PRListSnapshot {
	snapshot := PRListSnapshot{Projects: make([]ProjectPRList, 0, len(projects))}
	if projectsErr != nil {
		snapshot.Error = "The projects could not be read: " + projectsErr.Error()
	}
	// A repository two projects clone lists its PRs twice; the badge
	// counts each PR once.
	needsYou := map[string]bool{}
	for _, project := range projects {
		repo := project.Repo
		if repo == (gitops.RepoReference{}) {
			repo, _ = storedRepoOf(lists, project.ProjectID)
		}
		list, ok := ProjectPRList{}, false
		if repo != (gitops.RepoReference{}) {
			list, ok = lists[repo.Key()]
		}
		if !ok {
			list = ProjectPRList{Repo: repo}
		}
		list.ProjectID, list.Name = project.ProjectID, project.Name
		if list.Rows == nil {
			list.Rows = []PRListItem{}
		}
		if project.Error != "" {
			// The origin's failure, beside the rows read before it.
			list.Error, list.ErrorKind, list.ErrorDetails, list.Reserve, list.ResumeAt = project.Error, forgeFailureForge, nil, false, ""
		}
		for _, row := range list.Rows {
			if row.Group == PRGroupNeedsYou {
				needsYou[row.Ref.Key()] = true
			}
		}
		snapshot.Projects = append(snapshot.Projects, list)
	}
	snapshot.NeedsYou = len(needsYou)
	return snapshot
}
