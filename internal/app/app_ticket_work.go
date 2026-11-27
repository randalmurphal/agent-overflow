package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// TicketWork is what a ticket's Work lists besides its threads, which the
// client reads from its thread rows: the pull requests and branches whose
// name or title names the ticket's key (trackerapp.TicketKeys).
type TicketWork struct {
	Key          string              `json:"key"`
	PullRequests []TicketPullRequest `json:"pullRequests"`
	Branches     []TicketBranch      `json:"branches"`
	// Problems are the repositories whose branches could not be read,
	// each with why; the rest of the answer stands beside them.
	Problems []TicketWorkProblem `json:"problems"`
	// ForgeProblems are the repositories whose last pull request history
	// read failed (History), and those whose forge search failed
	// (SearchTicketWork), each with why in the Pull requests page's
	// terms; the rest of the answer stands beside them.
	ForgeProblems []TicketForgeProblem `json:"forgeProblems"`
	// SearchFull are the repositories whose search found more pull
	// requests than its one page holds, so the list may lack some.
	SearchFull []gitops.RepoReference `json:"searchFull"`
}

// TicketPullRequest is a pull request of a ticket's Work. An open one of
// the stored Pull requests list carries its title, author and CI. One
// the stored pull request history holds, or the forge's search found
// (SearchTicketWork), carries its title, author and state as the forge
// answered them, and no CI. One a thread linked to
// the ticket was last seen with, that none of those holds, records no
// title, author or CI.
type TicketPullRequest struct {
	Ref   gitops.PRReference `json:"ref"`
	URL   string             `json:"url"`
	Title string             `json:"title,omitempty"`
	// Author is the author's name, else their login, as the Pull
	// requests page names them.
	Author string `json:"author,omitempty"`
	// State is draft, open, merged or closed.
	State  string `json:"state"`
	CI     string `json:"ci,omitempty"`
	Branch string `json:"branch"`
	// ProjectID is the project the row was listed under: the stored
	// list's, the one whose branch the forge's lookup found it by, or the
	// linked thread's. Empty when that project was removed, or the thread
	// is not one Work lists.
	ProjectID string `json:"projectId,omitempty"`
	// ReviewProjectID is the project of the computer's whose checkout can
	// show the pull request in the app (reviewProject); empty when none
	// can, and the row opens on the forge.
	ReviewProjectID string `json:"reviewProjectId,omitempty"`
}

// Ticket pull request states.
const (
	TicketPRDraft  = "draft"
	TicketPROpen   = "open"
	TicketPRMerged = "merged"
	TicketPRClosed = "closed"
)

// TicketBranch is a branch of a ticket's Work, as the project's branch
// listing reads it (gitops.Core.ListBranches): where it is checked out,
// and ahead and behind its upstream.
type TicketBranch struct {
	ProjectID string `json:"projectId"`
	gitops.GitBranch
}

// TicketWorkProblem is a repository whose branches could not be read.
type TicketWorkProblem struct {
	ProjectID string `json:"projectId"`
	Error     string `json:"error"`
}

// TicketForgeProblem is a repository whose pull request history read or
// forge search failed, with the fields a Pull requests page project
// carries for a failed read (ProjectPRList): the caller-safe sentence,
// its kind, the rate limit's reserve and resume time, and the detail
// behind it. ProjectID is the first project of Work's that clones the
// repository, or of a fork's parent the fork's. Repo is zero for a
// project whose origin git could not read, whose Error is that failure's
// own text. History marks the last read of the repository's stored pull
// request history, which Work's rows of it come from, at FailedAt (Unix
// ms); otherwise the failure is this answer's search.
type TicketForgeProblem struct {
	ProjectID    string               `json:"projectId"`
	Repo         gitops.RepoReference `json:"repo"`
	Error        string               `json:"error"`
	ErrorKind    string               `json:"errorKind"`
	ErrorDetails *ForgeErrorDetail    `json:"errorDetails,omitempty"`
	Reserve      bool                 `json:"reserve,omitempty"`
	ResumeAt     string               `json:"resumeAt,omitempty"`
	History      bool                 `json:"history,omitempty"`
	FailedAt     int64                `json:"failedAt,omitempty"`
}

// GetTicketWork lists the pull requests and branches of the ticket issueID
// that connectionID reads, from what this computer stores and from git's
// refs, with no forge or tracker request: the stored Pull requests list
// (open pull requests), the stored pull request history (addTicketHistory)
// with each repository's failed read, the pull requests its linked
// threads were last seen with (merged and closed ones too), and the
// branches of the projects that list a board of the connection, hold one
// of those pull requests or hold a thread linked to the ticket.
// The ticket's key is the one its mirror holds, so the ticket must have
// been read on this computer, as an open ticket has.
//
//ao:scope git:operate
//ao:route home
func (a *App) GetTicketWork(ctx context.Context, connectionID, issueID string) (TicketWork, error) {
	if err := a.requireScope(ctx, transport.ScopeTrackerRead, "GetTicketWork reads the ticket's key"); err != nil {
		return TicketWork{}, err
	}
	read, err := a.readTicketWork(ctx, connectionID, issueID)
	if err != nil {
		return TicketWork{}, err
	}
	if _, err := a.addTicketHistory(&read); err != nil {
		return TicketWork{}, err
	}
	return a.answerTicketWork(ctx, &read)
}

// SearchTicketWork is GetTicketWork's answer with each pull request
// history of Work's repositories read again when its last read is
// forgeHistoryCatchUp old (catchUpTicketHistory), and the forge searched
// by the ticket's key in each repository whose history cannot answer for
// the ticket (addTicketHistory): once per such repository, and once per
// branch of the ticket in it (searchTicketForge). Each repository's
// failure is its ForgeProblem, and the rest of the answer stands beside
// them. A client
// shows GetTicketWork's answer first and this one when it lands, so a slow
// forge holds no row of the stored sources back. The budget runs from
// entry: every forge request runs within it, and no project's local read
// starts after it.
//
//ao:scope git:operate
//ao:route home
//ao:budget 2m15s
func (a *App) SearchTicketWork(ctx context.Context, connectionID, issueID string) (TicketWork, error) {
	if err := a.requireScope(ctx, transport.ScopeTrackerRead, "SearchTicketWork reads the ticket's key"); err != nil {
		return TicketWork{}, err
	}
	ctx, cancel := withinMethodBudget(forgeapi.WithInteractive(ctx), "SearchTicketWork", "Searching the forge for the ticket's pull requests")
	defer cancel()
	read, err := a.readTicketWork(ctx, connectionID, issueID)
	if err != nil {
		return TicketWork{}, budgetError(ctx, err)
	}
	if err := a.catchUpTicketHistory(ctx, &read); err != nil {
		return TicketWork{}, err
	}
	search, err := a.addTicketHistory(&read)
	if err != nil {
		return TicketWork{}, err
	}
	a.searchTicketForge(ctx, &read, search)
	return a.answerTicketWork(ctx, &read)
}

// ticketWorkRead is a ticket's Work as its reads gather it: the pull
// requests listed so far, the projects Work covers, the forge repository
// each one's origin names (Repo zero, and Error set, when git could not
// read it) and every branch of theirs that names the key.
// answerTicketWork folds a branch into the pull request whose head it is.
type ticketWorkRead struct {
	work TicketWork
	// created is the ticket's creation time (Unix ms, the tracker's
	// clock), 0 when its mirror holds none.
	created  int64
	projects []string
	origins  []ForgeProject
	branches []TicketBranch
	// recorded are the pull requests, by identity, that only a linked
	// thread's record lists.
	recorded map[string]bool
	// heads are the pull requests whose head is a branch of a project's
	// checkout, by the project and the branch: those a forge lookup by
	// the branch's name found in the checkout (searchTicketForge), and
	// the stored history's whose head repository is the project's origin
	// (addTicketHistory). They prove that a fork's branch is the head of
	// a pull request of its parent.
	heads map[ticketHead][]string
}

// ticketHead is a branch of a project's checkout that a head lookup ran
// for.
type ticketHead struct{ project, branch string }

// readTicketWork is the read from what this computer stores and git's
// refs that both Work methods start from (GetTicketWork's sources).
func (a *App) readTicketWork(ctx context.Context, connectionID, issueID string) (ticketWorkRead, error) {
	svc, err := a.trackerService()
	if err != nil {
		return ticketWorkRead{}, err
	}
	conn, err := a.store.GetTrackerConnection(connectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ticketWorkRead{}, errorsx.Public("tracker_connection_missing", "this computer has no such tracker connection; it may have been removed", err)
	}
	if err != nil {
		return ticketWorkRead{}, err
	}
	key, created, err := a.mirroredTicket(connectionID, issueID)
	if err != nil {
		return ticketWorkRead{}, err
	}
	read := ticketWorkRead{
		created: created,
		work: TicketWork{Key: key, PullRequests: []TicketPullRequest{}, Branches: []TicketBranch{}, Problems: []TicketWorkProblem{},
			ForgeProblems: []TicketForgeProblem{}, SearchFull: []gitops.RepoReference{}},
	}

	open, err := a.storedTicketPullRequests(key)
	if err != nil {
		return ticketWorkRead{}, err
	}
	recorded, err := a.store.ListTicketThreadPullRequests(store.TicketRef{Kind: conn.Kind, Site: conn.Site, IssueID: issueID})
	if err != nil {
		return ticketWorkRead{}, err
	}
	// A thread linked to the ticket may run in a repository that lists no
	// board of the connection and has no pull request yet.
	linked, err := a.store.ListThreadsForTicket(store.TicketRef{Kind: conn.Kind, Site: conn.Site, IssueID: issueID})
	if err != nil {
		return ticketWorkRead{}, err
	}
	read.work.PullRequests = open
	for _, pr := range recorded {
		row, ok := recordedTicketPullRequest(pr)
		if ok && !slices.ContainsFunc(read.work.PullRequests, func(p TicketPullRequest) bool { return p.Ref.Identity() == row.Ref.Identity() }) {
			if at := slices.IndexFunc(linked, func(t store.Thread) bool { return t.ID == pr.ThreadID }); at >= 0 {
				row.ProjectID = linked[at].ProjectID
			}
			read.work.PullRequests = append(read.work.PullRequests, row)
			if read.recorded == nil {
				read.recorded = map[string]bool{}
			}
			read.recorded[row.Ref.Identity()] = true
		}
	}

	choices, err := svc.BoardChoices()
	if err != nil {
		return ticketWorkRead{}, trackerError(err)
	}
	for _, choice := range choices {
		if choice.Connection == connectionID && !choice.ProjectArchived && !slices.Contains(read.projects, choice.Project) {
			read.projects = append(read.projects, choice.Project)
		}
	}
	// A stored list names the project it was read under, which may have
	// been removed since (the list stays until the Pull requests page's
	// next read prunes it); its rows stay and name no project, and no
	// branches are read for it.
	removed := map[string]bool{}
	for i, pr := range read.work.PullRequests {
		if pr.ProjectID == "" || slices.Contains(read.projects, pr.ProjectID) {
			continue
		}
		if !removed[pr.ProjectID] {
			_, err := a.store.GetProject(pr.ProjectID)
			switch {
			case err == nil:
				read.projects = append(read.projects, pr.ProjectID)
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return ticketWorkRead{}, err
			}
			removed[pr.ProjectID] = true
		}
		read.work.PullRequests[i].ProjectID = ""
	}
	for _, t := range linked {
		if t.ProjectID != "" && !slices.Contains(read.projects, t.ProjectID) {
			read.projects = append(read.projects, t.ProjectID)
		}
	}
	// The method's deadline is checked before each project's read; a read
	// already running ends at git's own timeout.
	for _, projectID := range read.projects {
		if err := ctx.Err(); err != nil {
			return ticketWorkRead{}, err
		}
		project, err := a.store.GetProject(projectID)
		if err != nil {
			read.work.Problems = append(read.work.Problems, TicketWorkProblem{ProjectID: projectID, Error: err.Error()})
			continue
		}
		origin, ok, err := a.forgeProject(ctx, project)
		if err != nil {
			return ticketWorkRead{}, err
		}
		if ok {
			read.origins = append(read.origins, origin)
		}
		branches, err := a.ticketBranches(project, key)
		if err != nil {
			read.work.Problems = append(read.work.Problems, TicketWorkProblem{ProjectID: projectID, Error: err.Error()})
			continue
		}
		read.branches = append(read.branches, branches...)
	}
	return read, nil
}

// answerTicketWork is the Work answer: the pull requests listed, each
// with the project that can show it (reviewProjects), and the branches
// that are no listed pull request's head. A branch of project P is a pull
// request's head when their names match and the pull request is of P's
// origin's repository, or a lookup by the branch's name in P's checkout
// found it, as it finds a fork's in the parent. The project a row was
// listed under takes no part: it is where the list was read, not where
// the head is. So before the search answers, a fork's branch stands
// beside the parent's pull request it heads.
func (a *App) answerTicketWork(ctx context.Context, read *ticketWorkRead) (TicketWork, error) {
	if err := a.reviewProjects(ctx, read); err != nil {
		return TicketWork{}, err
	}
	origins := make(map[string]string, len(read.origins))
	for _, o := range read.origins {
		if o.Error == "" {
			origins[o.ProjectID] = o.Repo.Identity()
		}
	}
	work := read.work
	work.Branches = []TicketBranch{}
	for _, b := range read.branches {
		found := read.heads[ticketHead{b.ProjectID, b.Name}]
		heads := slices.ContainsFunc(work.PullRequests, func(pr TicketPullRequest) bool {
			return pr.Branch == b.Name && (origins[b.ProjectID] != "" && pr.Ref.Repository().Identity() == origins[b.ProjectID] ||
				slices.Contains(found, pr.Ref.Identity()))
		})
		if !heads {
			work.Branches = append(work.Branches, b)
		}
	}
	return work, nil
}

// reviewProjects names, for each listed pull request, the project of the
// computer whose checkout can show it in the app (ReviewProjectID): one
// whose origin is the pull request's repository, since a review fetches
// its refs from origin (gitops.Core.EnsurePRObjects), so a fork's
// checkout cannot show its parent's. An archived project counts: archiving
// keeps the checkout, and a review opens a project by its ID. The row's
// own project comes first, then the projects Work covers, whose origins
// the read has; the computer's other projects are read, in name order,
// only for a row those leave without one, and only until every such row
// has one or ctx ends, so a search that spent its budget keeps its answer
// and those rows open on the forge. None when no project can.
func (a *App) reviewProjects(ctx context.Context, read *ticketWorkRead) error {
	clones := map[string][]string{}
	for _, o := range read.origins {
		if o.Error == "" {
			clones[o.Repo.Identity()] = append(clones[o.Repo.Identity()], o.ProjectID)
		}
	}
	prs := read.work.PullRequests
	// The rows no project Work covers can show, by their repository.
	waiting := map[string][]int{}
	for i, pr := range prs {
		repo := pr.Ref.Repository().Identity()
		shows := clones[repo]
		switch {
		case slices.Contains(shows, pr.ProjectID):
			prs[i].ReviewProjectID = pr.ProjectID
		case len(shows) > 0:
			prs[i].ReviewProjectID = shows[0]
		default:
			waiting[repo] = append(waiting[repo], i)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	all, err := a.store.ListAllProjects()
	if err != nil {
		return fmt.Errorf("read the projects that may show the ticket's pull requests: %w", err)
	}
	for _, p := range all {
		if slices.Contains(read.projects, p.ID) {
			continue
		}
		// No read starts once ctx ended, and forgeProject fails only then.
		if ctx.Err() != nil {
			return nil
		}
		origin, ok, err := a.forgeProject(ctx, p)
		if err != nil {
			return nil
		}
		if !ok || origin.Error != "" {
			continue
		}
		rows, found := waiting[origin.Repo.Identity()]
		if !found {
			continue
		}
		for _, i := range rows {
			prs[i].ReviewProjectID = p.ID
		}
		delete(waiting, origin.Repo.Identity())
		if len(waiting) == 0 {
			return nil
		}
	}
	return nil
}

// mirroredTicket is the key and creation time (Unix ms) of issueID in
// connectionID's mirror.
func (a *App) mirroredTicket(connectionID, issueID string) (string, int64, error) {
	root, ok, err := a.store.GetMirrorRoot(connectionID)
	if err != nil {
		return "", 0, err
	}
	if ok {
		cards, err := a.store.ListMirrorCards(root, []string{issueID})
		if err != nil {
			return "", 0, err
		}
		if len(cards) == 1 && cards[0].Key != "" {
			return cards[0].Key, cards[0].CreatedAt, nil
		}
	}
	return "", 0, errorsx.Public("ticket_not_read", "this computer has not read the ticket yet: open it first", fmt.Errorf("issue %s of connection %s is not in its mirror", issueID, connectionID))
}

// storedPRList is the part of a stored repository read that a ticket's
// Work reads (ProjectPRList), so a match decodes no more of it.
type storedPRList struct {
	ProjectID string `json:"projectId"`
	Rows      []struct {
		Ref         gitops.PRReference  `json:"ref"`
		URL         string              `json:"url"`
		Title       string              `json:"title"`
		Draft       bool                `json:"draft"`
		Author      gitops.PRListAuthor `json:"author"`
		HeadRefName string              `json:"headRefName"`
		CI          string              `json:"ci"`
	} `json:"rows"`
}

// storedTicketPullRequests lists the stored open pull requests whose title
// or head branch names key. A repository's read is decoded only when its
// bytes hold the key in any case, so a ticket's open costs one scan of the
// stored lists and a decode of the repositories that name it.
func (a *App) storedTicketPullRequests(key string) ([]TicketPullRequest, error) {
	records, err := a.store.ListPRListProjects()
	if err != nil {
		return nil, fmt.Errorf("read the stored pull requests: %w", err)
	}
	keys := make([]string, 0, len(records))
	for repoKey := range records {
		keys = append(keys, repoKey)
	}
	slices.Sort(keys)
	var out []TicketPullRequest
	for _, repoKey := range keys {
		blob := records[repoKey].ProjectJSON
		if !containsASCIIFold(blob, key) {
			continue
		}
		var list storedPRList
		if err := json.Unmarshal(blob, &list); err != nil {
			return nil, fmt.Errorf("read the stored pull requests of %s: %w", repoKey, err)
		}
		for _, row := range list.Rows {
			if !namesTicketKey(row.Title, row.HeadRefName, key) {
				continue
			}
			state := TicketPROpen
			if row.Draft {
				state = TicketPRDraft
			}
			out = append(out, TicketPullRequest{
				Ref: row.Ref, URL: row.URL, Title: row.Title, Author: cmp.Or(row.Author.Name, row.Author.Login), State: state,
				CI: row.CI, Branch: row.HeadRefName, ProjectID: list.ProjectID,
			})
		}
	}
	return out, nil
}

// recordedTicketPullRequest is a linked thread's pull request as Work
// lists it: merged once the forge reported the merge, closed once the app
// handled it as closed, otherwise open (the stored list, read first,
// holds an open one with its title). A record whose URL names no pull
// request the app can address is left out.
func recordedTicketPullRequest(pr store.ThreadPullRequest) (TicketPullRequest, bool) {
	ref, err := gitops.ParsePRURL(pr.URL)
	if err != nil {
		return TicketPullRequest{}, false
	}
	state := TicketPROpen
	switch {
	case pr.MergedAt > 0:
		state = TicketPRMerged
	case pr.Outcome == store.PROutcomeClosed:
		state = TicketPRClosed
	}
	return TicketPullRequest{Ref: ref, URL: pr.URL, State: state, Branch: pr.Branch}, true
}

// ticketBranches lists the branches of project whose name names key.
func (a *App) ticketBranches(project store.Project, key string) ([]TicketBranch, error) {
	branches, err := a.gitCore().ListBranches(project.Path)
	if err != nil {
		return nil, err
	}
	var out []TicketBranch
	for _, b := range branches {
		if slices.Contains(trackerapp.TicketKeys(b.Name), key) {
			out = append(out, TicketBranch{ProjectID: project.ID, GitBranch: b})
		}
	}
	return out, nil
}

// namesTicketKey is the rule for whether a pull request is one of key's:
// its title or head branch names key (trackerapp.TicketKeys).
func namesTicketKey(title, branch, key string) bool {
	return slices.Contains(trackerapp.TicketKeys(title+" "+branch), key)
}

// containsASCIIFold reports whether b holds s, an ASCII string, in any
// ASCII case, without copying b.
func containsASCIIFold(b []byte, s string) bool {
	n := len(s)
	if n == 0 {
		return true
	}
	first := lowerASCII(s[0])
	for i := 0; i+n <= len(b); i++ {
		if lowerASCII(b[i]) != first {
			continue
		}
		j := 1
		for j < n && lowerASCII(b[i+j]) == lowerASCII(s[j]) {
			j++
		}
		if j == n {
			return true
		}
	}
	return false
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
