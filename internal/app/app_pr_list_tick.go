package app

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// One tick of the PR list pump (app_pr_list.go): each forge project's
// list, read once per repository with at most prListReadConcurrency at a
// time; the focused rows' detail; attention; the user's pr_views. Reads
// run outside App.prUpdates.mu, and the results are folded in under it.

const (
	// prListReadConcurrency bounds the project list reads, and separately
	// the detail reads, one tick runs at once.
	prListReadConcurrency = 4
	// prListDetailMaxAge is how long a focused row's detail stands while
	// its list row does not change. GitHub's GraphQL budget is 5000 points
	// an hour, and reading 60 rows' detail every 45 seconds would spend
	// it: a detail is read again when the row changed (its update time,
	// head, comments, checks or merge status), when the user refreshes, or
	// once it is this old (a GitLab pipeline finishing moves no list
	// field).
	prListDetailMaxAge = 5 * time.Minute
)

// prListProjectState is one repository's last read: its rows, and the
// active failure (deduplicated like the PR pump's), with graced for a
// transient failure held back once while rows are on screen.
type prListProjectState struct {
	rows   []gitops.PRListRow
	read   bool
	fail   prForgeFailure
	graced bool
}

// prListDetailState is one focused row's last detail read, the list row
// it was read for (fingerprint) and when; err is the last read's failure.
type prListDetailState struct {
	detail      *PRListDetail
	fingerprint string
	readAt      time.Time
	err         prForgeFailure
}

func (a *App) listProjectPRs(ctx context.Context, repo gitops.RepoReference) ([]gitops.PRListRow, error) {
	if a.prUpdates.listFetchFn != nil {
		return a.prUpdates.listFetchFn(ctx, repo)
	}
	return a.gitCore().ListProjectPRs(ctx, repo)
}

func (a *App) readPRListDetail(ctx context.Context, pr gitops.PRReference) (gitops.PRDetail, error) {
	if a.prUpdates.listDetailFn != nil {
		return a.prUpdates.listDetailFn(ctx, pr)
	}
	return a.gitCore().ReadPRSummary(ctx, pr)
}

// prListFingerprint is what a detail read depends on in a list row.
func prListFingerprint(row gitops.PRListRow) string {
	encoded, _ := json.Marshal([]any{row.UpdatedAt, row.HeadSHA, row.Comments, row.ChecksState, row.MergeStatus, row.Draft, row.ReviewDecision})
	return string(encoded)
}

// rowKeysLocked is every PR key the pump's rows hold.
func (p *prListPump) rowKeysLocked() map[string]bool {
	keys := map[string]bool{}
	for _, state := range p.projects {
		for _, row := range state.rows {
			keys[row.Ref.Key()] = true
		}
	}
	return keys
}

// pollPRList runs one tick under ctx (interactive for a subscribe or a
// refresh, which also re-reads every focused detail). It reports the
// snapshot when it changed, whether a project is failing (the retry
// schedule), and the earliest rate limit release among held projects.
func (a *App) pollPRList(ctx context.Context, pump *prListPump, forceDetails bool) (PRListSnapshot, bool, bool, time.Time) {
	projects, projectsErr := a.forgeProjects(ctx)
	interactive := forgeapi.IsInteractive(ctx)

	// One read per repository, whatever number of projects (clones) name
	// it; a background read waits out its project's rate limit.
	repos := map[string]gitops.RepoReference{}
	var order []string
	current := map[string]bool{}
	a.prUpdates.mu.Lock()
	for _, project := range projects {
		if project.Error != "" {
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
	a.prUpdates.mu.Unlock()
	rows := make([][]gitops.PRListRow, len(order))
	errs := make([]error, len(order))
	runBounded(len(order), prListReadConcurrency, func(i int) {
		rows[i], errs[i] = a.listProjectPRs(ctx, repos[order[i]])
	})
	if ctx.Err() != nil {
		return PRListSnapshot{}, false, false, time.Time{}
	}

	a.prUpdates.mu.Lock()
	if pump.dead {
		a.prUpdates.mu.Unlock()
		return PRListSnapshot{}, false, false, time.Time{}
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
	for i, key := range order {
		state := pump.projects[key]
		if state == nil {
			state = &prListProjectState{}
			pump.projects[key] = state
		}
		if errs[i] == nil {
			state.rows, state.read, state.fail, state.graced = rows[i], true, prForgeFailure{}, false
			continue
		}
		fail := a.newRepoForgeFailure(repos[key], errs[i])
		if fail.kind == forgeFailureTransient && state.read && !state.fail.failing() && !state.graced {
			state.graced = true
			log.Printf("pr list: read of %s failed (id: %s), retrying before reporting it: %v", key, fail.correlationID, errs[i])
			continue
		}
		state.graced = false
		if state.fail.key != fail.key {
			state.fail = fail
			log.Printf("pr list: read of %s failed (id: %s): %v", key, fail.correlationID, errs[i])
		}
	}
	focus := a.prListFocusLocked(pump, forceDetails)
	a.prUpdates.mu.Unlock()

	a.readPRListDetails(ctx, pump, focus)
	return a.publishPRList(pump)
}

// pollPRListFocus reads the detail of focused rows that have none yet and
// republishes, without reading the lists.
func (a *App) pollPRListFocus(ctx context.Context, pump *prListPump) (PRListSnapshot, bool) {
	a.prUpdates.mu.Lock()
	var fresh []prListFocusRead
	for _, read := range a.prListFocusLocked(pump, false) {
		if pump.details[read.pr.Key()] == nil {
			fresh = append(fresh, read)
		}
	}
	a.prUpdates.mu.Unlock()
	if len(fresh) == 0 {
		return PRListSnapshot{}, false
	}
	a.readPRListDetails(ctx, pump, fresh)
	snapshot, changed, _, _ := a.publishPRList(pump)
	return snapshot, changed
}

type prListFocusRead struct {
	pr          gitops.PRReference
	fingerprint string
}

// prListFocusLocked lists the focused rows whose detail is due: never
// read, read for another list row, older than prListDetailMaxAge, or every
// one when forced. A detail no row is focused on any more is dropped.
func (a *App) prListFocusLocked(pump *prListPump, force bool) []prListFocusRead {
	focused := map[string]bool{}
	for _, handle := range a.prUpdates.listHandles {
		for key := range handle.focus {
			focused[key] = true
		}
	}
	now := time.Now()
	var due []prListFocusRead
	seen := map[string]bool{}
	for _, state := range pump.projects {
		for _, row := range state.rows {
			key := row.Ref.Key()
			if !focused[key] || seen[key] {
				continue
			}
			seen[key] = true
			fingerprint := prListFingerprint(row)
			last := pump.details[key]
			if force || last == nil || last.fingerprint != fingerprint || now.Sub(last.readAt) >= prListDetailMaxAge {
				due = append(due, prListFocusRead{pr: row.Ref, fingerprint: fingerprint})
			}
		}
	}
	for key := range pump.details {
		if !seen[key] {
			delete(pump.details, key)
		}
	}
	return due
}

// readPRListDetails reads each due row's detail, at most
// prListReadConcurrency at once, and folds the answers in.
func (a *App) readPRListDetails(ctx context.Context, pump *prListPump, due []prListFocusRead) {
	details := make([]gitops.PRDetail, len(due))
	errs := make([]error, len(due))
	runBounded(len(due), prListReadConcurrency, func(i int) {
		details[i], errs[i] = a.readPRListDetail(ctx, due[i].pr)
	})
	if ctx.Err() != nil {
		return
	}
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if pump.dead {
		return
	}
	now := time.Now()
	for i, read := range due {
		key := read.pr.Key()
		state := pump.details[key]
		if state == nil {
			state = &prListDetailState{}
			pump.details[key] = state
		}
		state.fingerprint, state.readAt = read.fingerprint, now
		if errs[i] != nil {
			if fail := a.newPRForgeFailure(read.pr, errs[i]); state.err.key != fail.key {
				state.err = fail
				log.Printf("pr list: detail of %s failed (id: %s): %v", key, fail.correlationID, errs[i])
			}
			continue
		}
		state.detail, state.err = prListDetailOf(details[i]), prForgeFailure{}
	}
}

// publishPRList builds the snapshot from the pump's state (the projects
// the last tick read) and the user's views, and stores and stamps it when
// it differs from the last one. It also reports whether a project is
// failing and the earliest rate limit release, for the retry schedule.
func (a *App) publishPRList(pump *prListPump) (PRListSnapshot, bool, bool, time.Time) {
	a.prUpdates.mu.Lock()
	rowKeys := pump.rowKeysLocked()
	keys := make([]string, 0, len(rowKeys))
	for key := range rowKeys {
		keys = append(keys, key)
	}
	a.prUpdates.mu.Unlock()
	views, viewsErr := a.store.ListPRViews(keys)
	if viewsErr != nil {
		log.Printf("pr list: read the PR views: %v", viewsErr)
	}

	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	if pump.dead {
		return PRListSnapshot{}, false, false, time.Time{}
	}
	projects, projectsErr := pump.forge, pump.forgeErr
	snapshot := PRListSnapshot{Projects: make([]ProjectPRList, 0, len(projects))}
	switch {
	case projectsErr != nil:
		snapshot.Error = "The projects could not be read: " + projectsErr.Error()
	case viewsErr != nil:
		snapshot.Error = "What you have seen of each pull request could not be read: " + viewsErr.Error()
	}
	failing := false
	var release time.Time
	// A repository two projects clone lists its PRs twice; the badge
	// counts each PR once.
	needsYou := map[string]bool{}
	for _, project := range projects {
		list := ProjectPRList{ProjectID: project.ProjectID, Name: project.Name, Repo: project.Repo, Rows: []PRListItem{}, Error: project.Error}
		if project.Error != "" {
			list.ErrorKind = forgeFailureForge
			snapshot.Projects = append(snapshot.Projects, list)
			continue
		}
		state := pump.projects[project.Repo.Key()]
		if state == nil {
			snapshot.Projects = append(snapshot.Projects, list)
			continue
		}
		if state.fail.failing() || state.graced {
			failing = failing || !state.fail.rateLimited()
			if state.fail.rateLimited() && (release.IsZero() || state.fail.release.Before(release)) {
				release = state.fail.release
			}
		}
		list.Error, list.ErrorKind, list.Reserve, list.ResumeAt = state.fail.wire, state.fail.kind, state.fail.reserve, state.fail.resumeAt
		for _, row := range state.rows {
			item := PRListItem{PRListRow: row}
			if detail := pump.details[row.Ref.Key()]; detail != nil {
				item.Detail, item.DetailError = detail.detail, detail.err.wire
			}
			item.State, item.Group = prAttention(row, item.Detail)
			if view, ok := views[row.Ref.Key()]; ok {
				item.Seen, item.Since = prSeenOf(view), prSinceOf(view, row)
			}
			if item.Group == PRGroupNeedsYou {
				needsYou[row.Ref.Key()] = true
			}
			list.Rows = append(list.Rows, item)
		}
		sortPRListItems(list.Rows)
		snapshot.Projects = append(snapshot.Projects, list)
	}
	snapshot.NeedsYou = len(needsYou)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("pr list: encode the snapshot: %v", err)
		return PRListSnapshot{}, false, failing, release
	}
	if string(encoded) == string(pump.encoded) {
		return PRListSnapshot{}, false, failing, release
	}
	pump.encoded = encoded
	pump.seq = a.nextPRUpdateSeqLocked()
	snapshot.Seq = pump.seq
	snapshot.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	pump.snapshot = snapshot
	return snapshot, true, failing, release
}

func prSeenOf(view store.PRView) *PRSeen {
	return &PRSeen{OpenedAt: view.OpenedAt, HeadSHA: view.HeadSHA, Comments: view.Comments, Commits: view.Commits}
}

func prSinceOf(view store.PRView, row gitops.PRListRow) *PRSince {
	since := &PRSince{HeadMoved: row.HeadSHA != "" && view.HeadSHA != row.HeadSHA}
	if view.Comments != nil {
		since.NewComments = max(row.Comments-*view.Comments, 0)
	}
	if view.Commits != nil {
		since.NewCommits = max(row.Commits-*view.Commits, 0)
	}
	return since
}
