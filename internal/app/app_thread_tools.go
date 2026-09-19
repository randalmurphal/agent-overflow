package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// threadToolsApp implements threadtools.App over this process's store,
// session manager and catalogs.
//
// It is an unexported adapter on purpose: an exported method on *App
// becomes a candidate RPC, and none of these belong on the wire. Every
// method here answers for THIS computer only; cross-computer reach is the
// Peer half, which this phase does not serve.
type threadToolsApp struct{ app *App }

func (a *App) threadToolsAdapter() threadtools.App { return threadToolsApp{app: a} }

// localThread reads one thread this computer owns, refusing a thread it
// gave away with the same not-found code a missing row gets. The code is
// what threadtools turns into the model-facing refusal.
func (t threadToolsApp) localThread(threadID string) (store.Thread, error) {
	thread, err := t.app.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, threadToolsNotFound(threadID)
	}
	if err := t.app.store.CheckThreadTransferAccess(threadID); err != nil {
		var moved *store.ThreadTransferError
		if errors.As(err, &moved) {
			return store.Thread{}, threadToolsNotFound(threadID)
		}
		return store.Thread{}, err
	}
	return thread, nil
}

func threadToolsNotFound(threadID string) error {
	return errorsx.Public(threadtools.CodeNotFound, fmt.Sprintf("No thread %s on this computer.", threadID), nil)
}

func (t threadToolsApp) Thread(_ context.Context, threadID string) (threadtools.Thread, error) {
	thread, err := t.localThread(threadID)
	if err != nil {
		return threadtools.Thread{}, err
	}
	return t.projectThread(thread), nil
}

// projectThread turns a store row into the tools' DTO, resolving the two
// names the model reads beside their ids.
func (t threadToolsApp) projectThread(thread store.Thread) threadtools.Thread {
	out := threadtools.Thread{
		ID:            thread.ID,
		Title:         thread.Title,
		ProjectID:     thread.ProjectID,
		Provider:      thread.Provider,
		Model:         thread.Model,
		Effort:        thread.ReasoningEffort,
		Mode:          thread.Mode,
		RuntimeMode:   thread.RuntimeMode,
		WorkspacePath: thread.WorkspacePath,
		Branch:        thread.Branch,
		GroupID:       thread.GroupID,
		Archived:      thread.Archived,
		Unread:        threadUnread(thread),
		LastActivity:  threadLastActivity(thread),

		HasIncompleteTurn:         thread.HasIncompleteTurn,
		HasFailedTurn:             thread.HasFailedTurn,
		HasActionableProposedPlan: thread.HasActionableProposedPlan,
		WorktreeSetupState:        thread.WorktreeSetupState,
	}
	if thread.WorktreePath != "" {
		out.WorkspacePath = thread.WorktreePath
	}
	if thread.ProjectID != "" {
		if project, err := t.app.store.GetProject(thread.ProjectID); err == nil {
			out.Project = project.Name
		}
	}
	if thread.GroupID != "" {
		if group, err := t.app.store.GetThreadGroup(thread.GroupID); err == nil {
			out.Group = group.Name
		}
	}
	out.Pin = threadPin(thread.PinnedAt, thread.PinGroup)
	return out
}

// threadPin names the pin tier the sidebar shows. A thread with no
// pinned_at is not pinned at all, whatever pin_group holds.
func threadPin(pinnedAt *int64, pinGroup *int) string {
	if pinnedAt == nil {
		return ""
	}
	if pinGroup != nil && *pinGroup == store.PinGroupBack {
		return threadtools.PinBack
	}
	return threadtools.PinFront
}

// threadUnread mirrors the frontend's hasUnread: the activity clock is the
// latest completed turn, and a row that was never read counts as read.
func threadUnread(thread store.Thread) bool {
	if thread.LatestTurnCompletedAt == nil || thread.LastReadAt == nil {
		return false
	}
	return *thread.LatestTurnCompletedAt > *thread.LastReadAt
}

func threadLastActivity(thread store.Thread) int64 {
	if thread.LatestTurnCompletedAt != nil && *thread.LatestTurnCompletedAt > 0 {
		return *thread.LatestTurnCompletedAt
	}
	return thread.UpdatedAt
}

// LiveState projects the triage live snapshot onto the four facts the
// state derivation reads. A thread with no session yields the zero value.
func (t threadToolsApp) LiveState(_ context.Context, threadID string) (threadtools.LiveState, error) {
	if t.app.triage == nil {
		return threadtools.LiveState{}, nil
	}
	snapshot := t.app.triage.LiveStateSnapshotForThread(threadID)
	return threadtools.LiveState{
		ActiveTurn:        snapshot.ActiveTurn != nil,
		PendingSends:      len(snapshot.QueueItems)+len(snapshot.FlushedItems)+len(snapshot.DeferredItems) > 0,
		PendingApprovals:  len(snapshot.Interactive.Approvals),
		PendingUserInputs: len(snapshot.Interactive.UserInputs),
	}, nil
}

// ResolveThreadRef matches a full id or a prefix against this computer's
// threads.
func (t threadToolsApp) ResolveThreadRef(_ context.Context, ref string) (threadtools.Resolution, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return threadtools.Resolution{}, nil
	}
	// A thread this computer handed away is a move, not a miss, and the
	// transfer record is the only place that says so.
	if err := t.app.store.CheckThreadTransferAccess(ref); err != nil {
		var moved *store.ThreadTransferError
		if errors.As(err, &moved) && moved.Moved {
			name, id := t.threadToolsBackendName(moved.BackendID)
			return threadtools.Resolution{MovedTo: id, MovedToName: name}, nil
		}
	}
	if thread, err := t.app.store.GetThread(ref); err == nil {
		if visible, owned := t.threadVisibleToTools(thread); visible && owned {
			return threadtools.Resolution{Matches: []threadtools.Candidate{{ThreadID: thread.ID, Title: thread.Title}}}, nil
		}
		return threadtools.Resolution{}, nil
	}
	candidates, err := t.prefixCandidates(ref)
	if err != nil {
		return threadtools.Resolution{}, err
	}
	return threadtools.Resolution{Matches: candidates}, nil
}

// prefixCandidates asks the store for the threads whose id starts with
// ref, in id order, and keeps the ones this caller may see.
//
// The store answers every mode, so the scratch rule is applied here; it
// can drop rows, which is why the lookup asks for more ids than the
// answer holds. The over-fetch is what bounds the walk: a prefix matching
// more than that is ambiguous several times over, and the refusal the
// candidates render says so with the rows it has.
func (t threadToolsApp) prefixCandidates(ref string) ([]threadtools.Candidate, error) {
	rows, err := t.app.store.ResolveThreadPrefix(ref, threadToolsPrefixScan)
	if err != nil {
		return nil, err
	}
	var matches []threadtools.Candidate
	for _, thread := range rows {
		visible, owned := t.threadVisibleToTools(thread)
		if !visible || !owned {
			continue
		}
		matches = append(matches, threadtools.Candidate{ThreadID: thread.ID, Title: thread.Title})
		if len(matches) == threadtools.MaxResolutionCandidates {
			break
		}
	}
	return matches, nil
}

// threadToolsPrefixScan is how many id-ordered rows one prefix lookup
// reads to fill MaxResolutionCandidates visible ones.
const threadToolsPrefixScan = 4 * threadtools.MaxResolutionCandidates

// threadVisibleToTools answers the two questions resolution asks of a row:
// hidden workflow threads are reachable, a scratch thread is not unless
// the tools themselves made it, and a thread this computer gave away is
// gone. owned is false for a thread whose transfer moved it.
func (t threadToolsApp) threadVisibleToTools(thread store.Thread) (visible, owned bool) {
	if err := t.app.store.CheckThreadTransferAccess(thread.ID); err != nil {
		return false, false
	}
	if thread.Mode == threadmode.ModeScratch {
		row, found, err := t.app.store.GetScratchThread(thread.ID)
		if err != nil || !found || row.RequestToken == "" {
			return false, true
		}
	}
	return true, true
}

// threadToolsBackendName names the computer a moved thread went to. An
// unknown backend id is still worth reporting: the id is what a later
// pairing will match.
func (t threadToolsApp) threadToolsBackendName(backendID string) (name, id string) {
	if backendID == "" {
		return "", ""
	}
	if t.app.backends != nil {
		if attached, err := t.app.backends.List(); err == nil {
			for _, row := range attached {
				if row.BackendID != backendID && row.ID != backendID {
					continue
				}
				label := row.Nickname
				if label == "" {
					label = row.Name
				}
				return label, row.ID
			}
		}
	}
	return "", backendID
}

// Catalog answers thread_options from the catalogs the app already keeps.
func (t threadToolsApp) Catalog(_ context.Context, q threadtools.CatalogQuery) (threadtools.Catalog, error) {
	catalog := threadtools.Catalog{Reachable: true, OS: threadToolsOS()}
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		if q.Provider != "" && q.Provider != name {
			continue
		}
		option, err := t.providerOption(name)
		if err != nil {
			// A provider whose catalog probe failed is still installed;
			// dropping it would read as "this computer has no Codex".
			// The entry stands with an empty model list, which is what
			// the renderer reports, and the cause is logged here.
			log.Printf("thread tools: model catalog for %s: %v", name, err)
			option = threadtools.ProviderOption{ID: name, Name: threadToolsProviderName(name), Models: []threadtools.ModelOption{}}
		}
		catalog.Providers = append(catalog.Providers, option)
	}
	projects, err := t.projectOptions(q.ProjectID)
	if err != nil {
		return threadtools.Catalog{}, err
	}
	catalog.Projects = projects
	return catalog, nil
}

func (t threadToolsApp) providerOption(name string) (threadtools.ProviderOption, error) {
	models, err := t.app.GetModelsForProvider(name)
	if err != nil {
		return threadtools.ProviderOption{}, err
	}
	option := threadtools.ProviderOption{ID: name, Name: threadToolsProviderName(name), Models: []threadtools.ModelOption{}}
	for _, model := range models.Models {
		entry := threadtools.ModelOption{Slug: model.Slug, Name: model.Name, Source: string(models.Provenance)}
		for _, effort := range model.ReasoningEfforts {
			entry.Efforts = append(entry.Efforts, effort.Slug)
			if effort.Default {
				entry.DefaultEffort = effort.Slug
			}
		}
		for _, window := range model.ContextWindows {
			if window.Default || entry.ContextWindow == 0 {
				entry.ContextWindow = window.Tokens
			}
		}
		option.Models = append(option.Models, entry)
	}
	if len(option.Models) > 0 {
		option.DefaultModel = option.Models[0].Slug
	}
	return option, nil
}

func threadToolsProviderName(name string) string {
	switch name {
	case string(provider.Claude):
		return "Claude Code"
	case string(provider.Codex):
		return "Codex"
	}
	return name
}

// projectOptions lists this computer's projects with the workspaces and
// groups a spawn can name. Workspaces come from the thread rows' own
// workspace and worktree paths, which is where this app records them:
// there is no worktree table, and asking git for a listing would be a
// subprocess per project on a read a model makes to see its choices.
func (t threadToolsApp) projectOptions(projectID string) ([]threadtools.ProjectOption, error) {
	projects, err := t.app.store.ListProjects()
	if err != nil {
		return nil, err
	}
	groups, err := t.app.store.ListThreadGroups()
	if err != nil {
		return nil, err
	}
	worktrees, err := t.app.store.ListThreadWorktreeWorkspaces()
	if err != nil {
		return nil, err
	}
	out := []threadtools.ProjectOption{}
	for _, project := range projects {
		if projectID != "" && project.ID != projectID {
			continue
		}
		if project.Archived {
			continue
		}
		option := threadtools.ProjectOption{ID: project.ID, Name: project.Name, Path: project.Path}
		option.Workspaces = []threadtools.WorkspaceOption{{Path: project.Path}}
		for _, worktree := range worktrees {
			if worktree.ProjectID != project.ID || worktree.Path == project.Path {
				continue
			}
			option.Workspaces = append(option.Workspaces, threadtools.WorkspaceOption{Path: worktree.Path, Branch: worktree.Branch, Worktree: true})
		}
		for _, group := range groups {
			if group.ProjectID != project.ID {
				continue
			}
			option.Groups = append(option.Groups, threadtools.GroupOption{ID: group.ID, Name: group.Name, Pin: threadPin(group.PinnedAt, group.PinGroup)})
		}
		out = append(out, option)
	}
	return out, nil
}

// SearchThreads answers the ranked search and the plain listing.
//
// Every filter but one is a store filter: provider, project, archived,
// since, spawned-by and the hidden-mode rules are SQL terms, so LIMIT and
// OFFSET count the rows this method returns and the cursor threadtools
// mints out of them continues the same page. The exception is `state`,
// which is derived from the live router and has no column at all.
func (t threadToolsApp) SearchThreads(ctx context.Context, q threadtools.SearchQuery) (threadtools.SearchPage, error) {
	indexing, err := t.app.store.SearchIndexing()
	if err != nil {
		return threadtools.SearchPage{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = threadtools.DefaultSearchLimit
	}
	page := threadtools.SearchPage{Indexing: indexing}
	if strings.TrimSpace(q.Query) == "" {
		rows, more, err := t.listThreads(ctx, q, limit)
		if err != nil {
			return threadtools.SearchPage{}, err
		}
		page.Rows, page.More = rows, more
		return page, nil
	}
	rows, more, err := t.searchIndex(ctx, q, limit)
	if err != nil {
		return threadtools.SearchPage{}, err
	}
	page.Rows, page.More = rows, more
	return page, nil
}

// threadSearchScanCap and threadSearchScanPage bound the walk a `state`
// filter forces: the live state of a thread is not a stored column, so
// the rows it rejects can only be found by reading them. A page that
// stops at the cap reports More, which is how the model learns to narrow
// the query rather than page forever; More is therefore true both when
// rows remain and when the scan gave up looking for them.
//
// Without a `state` filter neither applies: the store's own offset is the
// page boundary and nothing is scanned past it.
const (
	threadSearchScanCap  = 4000
	threadSearchScanPage = 200
)

// threadToolsFilter renders the store filter both halves share. Kinds is
// a search-only field; the listing ignores it.
func (t threadToolsApp) threadToolsFilter(q threadtools.SearchQuery) (store.ThreadSearchFilter, error) {
	scratch, err := t.callerScratchThreads(q.SpawnedBy)
	if err != nil {
		return store.ThreadSearchFilter{}, err
	}
	filter := store.ThreadSearchFilter{
		ProjectID:        q.ProjectID,
		Provider:         q.Provider,
		Archived:         q.Archived,
		SinceUnixMs:      q.SinceUnixMs,
		SpawnedBy:        q.SpawnedBy,
		ScratchThreadIDs: scratch,
	}
	if q.ThreadID != "" {
		filter.ThreadIDs = []string{q.ThreadID}
	}
	if q.Kind != "" {
		filter.Kinds = []string{q.Kind}
	}
	return filter, nil
}

// threadToolsPage assembles one page from a filtered store listing.
//
// With no `state` filter the store's LIMIT and OFFSET are the page: the
// offset the caller passed counts the rows it received, and one extra row
// answers More without reading a second page.
//
// A `state` filter has no column to match, so the only way to find the
// rows it rejects is to read them. The walk then pages the store itself, skips
// the caller's offset over the rows that SURVIVED the filter, and gives
// up at threadSearchScanCap with More set. A page that drops a row for
// any other reason returns short rather than reaching past its offset,
// so the next page re-reads that row instead of skipping the one behind
// it.
func threadToolsPage[Row any](
	ctx context.Context,
	q threadtools.SearchQuery,
	limit int,
	filter store.ThreadSearchFilter,
	fetch func(store.ThreadSearchFilter) ([]Row, error),
	project func(Row) (threadtools.Hit, bool, error),
) ([]threadtools.Hit, bool, error) {
	if q.State == "" {
		filter.Limit, filter.Offset = limit+1, q.Offset
		source, err := fetch(filter)
		if err != nil {
			return nil, false, err
		}
		more := len(source) > limit
		if more {
			source = source[:limit]
		}
		rows := make([]threadtools.Hit, 0, len(source))
		for _, row := range source {
			hit, keep, err := project(row)
			if err != nil {
				return nil, false, err
			}
			if keep {
				rows = append(rows, hit)
			}
		}
		return rows, more, nil
	}

	filter.Limit = threadSearchScanPage
	rows := make([]threadtools.Hit, 0, limit)
	skipped, scanned := 0, 0
	for offset := 0; scanned < threadSearchScanCap; offset += filter.Limit {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		filter.Offset = offset
		source, err := fetch(filter)
		if err != nil {
			return nil, false, err
		}
		if len(source) == 0 {
			return rows, false, nil
		}
		scanned += len(source)
		for _, row := range source {
			hit, keep, err := project(row)
			if err != nil {
				return nil, false, err
			}
			if !keep {
				continue
			}
			if skipped < q.Offset {
				skipped++
				continue
			}
			if len(rows) == limit {
				return rows, true, nil
			}
			rows = append(rows, hit)
		}
		if len(source) < filter.Limit {
			return rows, false, nil
		}
	}
	return rows, true, nil
}

func (t threadToolsApp) searchIndex(ctx context.Context, q threadtools.SearchQuery, limit int) ([]threadtools.Hit, bool, error) {
	filter, err := t.threadToolsFilter(q)
	if err != nil {
		return nil, false, err
	}
	// One thread is resolved once however many of its items match.
	cache := map[string]*threadtools.Hit{}
	return threadToolsPage(ctx, q, limit, filter,
		func(filter store.ThreadSearchFilter) ([]store.ThreadSearchHit, error) { return t.searchHits(q, filter) },
		func(hit store.ThreadSearchHit) (threadtools.Hit, bool, error) {
			row, keep, err := t.searchRow(ctx, cache, hit.ThreadID, q)
			if err != nil || !keep {
				return threadtools.Hit{}, false, err
			}
			out := *row
			out.ItemID, out.Snippet = hit.ItemID, hit.Snippet
			return out, true, nil
		})
}

// searchHits runs one index page and turns a rejected query into the
// public refusal the model can act on.
func (t threadToolsApp) searchHits(q threadtools.SearchQuery, filter store.ThreadSearchFilter) ([]store.ThreadSearchHit, error) {
	hits, err := t.app.store.SearchThreads(q.Query, filter)
	if err != nil {
		return nil, errorsx.Public(threadtools.CodeInvalidRequest,
			"That search query is not valid. Use plain words, \"a phrase\" or word prefixes like build*.", err)
	}
	return hits, nil
}

// searchRow resolves one thread once per call and answers whether the
// adapter-side filter keeps it.
func (t threadToolsApp) searchRow(ctx context.Context, cache map[string]*threadtools.Hit, threadID string, q threadtools.SearchQuery) (*threadtools.Hit, bool, error) {
	if row, ok := cache[threadID]; ok {
		return row, row != nil, nil
	}
	thread, err := t.app.store.GetThread(threadID)
	if err != nil {
		cache[threadID] = nil
		return nil, false, nil
	}
	row, keep, err := t.threadHit(ctx, thread, q)
	if err != nil {
		return nil, false, err
	}
	if !keep {
		cache[threadID] = nil
		return nil, false, nil
	}
	cache[threadID] = &row
	return &row, true, nil
}

// threadHit projects one thread row and applies the two rules the store
// filter cannot: a thread mid-transfer, which is this computer's row
// until the handover settles but is not readable through the tools, and
// the derived live state.
func (t threadToolsApp) threadHit(ctx context.Context, thread store.Thread, q threadtools.SearchQuery) (threadtools.Hit, bool, error) {
	if visible, owned := t.threadVisibleToTools(thread); !visible || !owned {
		return threadtools.Hit{}, false, nil
	}
	projected := t.projectThread(thread)
	live, err := t.LiveState(ctx, thread.ID)
	if err != nil {
		return threadtools.Hit{}, false, err
	}
	if q.State != "" && threadtools.State(projected, live) != q.State {
		return threadtools.Hit{}, false, nil
	}
	return threadtools.Hit{Thread: projected, Live: live}, true, nil
}

// listThreads answers a query-less thread_search: this computer's threads
// by last activity, newest first.
func (t threadToolsApp) listThreads(ctx context.Context, q threadtools.SearchQuery, limit int) ([]threadtools.Hit, bool, error) {
	filter, err := t.threadToolsFilter(q)
	if err != nil {
		return nil, false, err
	}
	return threadToolsPage(ctx, q, limit, filter,
		t.app.store.ListThreadsByActivity,
		func(thread store.Thread) (threadtools.Hit, bool, error) { return t.threadHit(ctx, thread, q) })
}

// callerScratchThreads names the scratch threads a caller may see: the
// ones a thread_ask minted, which carry a request token. A scratch thread
// a person made with /side-chat has none and stays hidden.
func (t threadToolsApp) callerScratchThreads(string) ([]string, error) {
	rows, err := t.app.store.ListScratchThreads()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, row := range rows {
		if row.RequestToken != "" {
			ids = append(ids, row.ThreadID)
		}
	}
	return ids, nil
}
