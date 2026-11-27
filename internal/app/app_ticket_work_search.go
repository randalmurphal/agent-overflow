package app

import (
	"cmp"
	"context"
	"log"
	"slices"
	"time"

	gitops "agent-overflow/internal/git"
)

// ticketSearchRepo is one forge repository a ticket's Work searches: the
// projects of Work's that clone it, in Work's order, and its outcome.
type ticketSearchRepo struct {
	repo     gitops.RepoReference
	projects []string
	paths    map[string]string
	found    gitops.PRKeySearch
	err      error
}

// ticketHeadLookup is one branch of the ticket a key search did not read
// whole, looked up by name in the checkout of a project that clones its
// repository.
type ticketHeadLookup struct {
	repo      *ticketSearchRepo
	projectID string
	branch    string
	found     []gitops.PRFound
	err       error
}

// searchTicketForge adds to read what the forge holds for the ticket's
// key in the repositories of origins, Work's origins whose pull request
// history cannot answer for the ticket (addTicketHistory), in two rounds
// of concurrent requests, at most prListReadConcurrency at a time:
//
//   - one key search per distinct repository Work's projects clone
//     (gitops.Core.SearchPRsByKey), however many projects clone it: the
//     pull requests whose title or head branch the forge matches, and the
//     repository's branches whose name holds the key;
//   - then, of a repository whose search answered, one lookup by name
//     (gitops.Core.ListPRsByHead) per branch of the ticket, the
//     repository's or a checkout's: the lookup owns which repositories
//     hold a branch's pull requests (a fork's parent too), and what Work
//     lists proves nothing, since a branch can have several.
//
// A found row or branch adds a pull request only when it names the key
// (namesTicketKey), since the forge's search is looser than the key rule;
// a found row of one listed is observed whatever it names, at the time
// it landed, after every stored observation Work read. The stored open rows of the pull requests found are
// observed too (observeListedRows).
// A repository's failure is its one ForgeProblem, beside the rows its
// search and lookups read before they failed; a failed search gets no
// lookups. A project whose origin
// names no forge repository is not searched.
func (a *App) searchTicketForge(ctx context.Context, read *ticketWorkRead, origins []ForgeProject) error {
	var repos []*ticketSearchRepo
	byKey := map[string]*ticketSearchRepo{}
	for _, origin := range origins {
		if origin.Error != "" {
			read.work.ForgeProblems = append(read.work.ForgeProblems, TicketForgeProblem{ProjectID: origin.ProjectID, Error: origin.Error, ErrorKind: forgeFailureForge})
			continue
		}
		key := origin.Repo.Identity()
		repo := byKey[key]
		if repo == nil {
			repo = &ticketSearchRepo{repo: origin.Repo, paths: map[string]string{}}
			byKey[key] = repo
			repos = append(repos, repo)
		}
		repo.projects = append(repo.projects, origin.ProjectID)
		repo.paths[origin.ProjectID] = origin.Path
	}

	ticketKey := read.work.Key
	runBounded(len(repos), prListReadConcurrency, func(i int) {
		repos[i].found, repos[i].err = a.gitCore().SearchPRsByKey(ctx, repos[i].repo, ticketKey)
	})
	var lookups []*ticketHeadLookup
	for _, repo := range repos {
		for _, found := range repo.found.Rows {
			read.addFound(found, repo.projects[0])
		}
		if repo.err != nil {
			continue
		}
		if repo.found.Full {
			read.work.SearchFull = append(read.work.SearchFull, repo.repo)
		}
		look := func(branch, projectID string) {
			if !namesTicketKey("", branch, ticketKey) ||
				slices.ContainsFunc(lookups, func(l *ticketHeadLookup) bool { return l.repo == repo && l.branch == branch }) {
				return
			}
			lookups = append(lookups, &ticketHeadLookup{repo: repo, projectID: projectID, branch: branch})
		}
		// A checkout's branch first, so its lookup runs in the project
		// that has it.
		for _, b := range read.branches {
			if repo.paths[b.ProjectID] != "" {
				look(b.Name, b.ProjectID)
			}
		}
		for _, branch := range repo.found.Branches {
			look(branch, repo.projects[0])
		}
	}
	runBounded(len(lookups), prListReadConcurrency, func(i int) {
		l := lookups[i]
		l.found, l.err = a.gitCore().ListPRsByHead(ctx, l.repo.paths[l.projectID], l.branch)
	})
	for _, l := range lookups {
		for _, found := range l.found {
			if read.addFound(found, l.projectID) {
				head := ticketHead{l.projectID, l.branch}
				if read.heads == nil {
					read.heads = map[ticketHead][]string{}
				}
				read.heads[head] = append(read.heads[head], found.Ref.Identity())
			}
		}
		if l.err != nil && l.repo.err == nil {
			l.repo.err = l.err
		}
	}

	for _, repo := range repos {
		if repo.err == nil {
			continue
		}
		fail := a.newRepoForgeFailure(repo.repo, repo.err)
		log.Printf("ticket work: search of %s for %s failed (id: %s): %v", repo.repo.Key(), ticketKey, fail.correlationID, repo.err)
		read.work.ForgeProblems = append(read.work.ForgeProblems, TicketForgeProblem{
			ProjectID: repo.projects[0], Repo: repo.repo, Error: fail.wire, ErrorKind: fail.kind, ErrorDetails: fail.detail,
			Reserve: fail.reserve, ResumeAt: fail.resumeAt,
		})
	}
	return read.observeListedRows()
}

// addFound observes a pull request the forge's search found, of
// projectID, now (observeFound), and reports whether it names the key.
func (r *ticketWorkRead) addFound(found gitops.PRFound, projectID string) bool {
	return r.observeFound(found, projectID, time.Now().UnixMilli(), "")
}

// observeFound observes found, observed at at (wall Unix ms), with its
// head repository when it is known, under projectID (observe), when it
// names the key or is listed, and reports whether it names the key.
func (r *ticketWorkRead) observeFound(found gitops.PRFound, projectID string, at int64, headRepo string) bool {
	names := namesTicketKey(found.Title, found.HeadRefName, r.work.Key)
	if !names && !r.listed(found.Ref.Identity()) {
		return false
	}
	row := TicketPullRequest{
		Ref: found.Ref, URL: found.URL, Title: found.Title, Author: cmp.Or(found.Author.Name, found.Author.Login),
		State: ticketPRState(found.State, found.Draft), Branch: found.HeadRefName, ProjectID: projectID,
	}
	r.observe(row, at)
	if headRepo != "" {
		if r.prHeads == nil {
			r.prHeads = map[string]string{}
		}
		r.prHeads[row.Ref.Identity()] = headRepo
	}
	return names
}

// ticketPRState is a found pull request's state as Work names it.
func ticketPRState(state string, draft bool) string {
	switch state {
	case "merged":
		return TicketPRMerged
	case "closed":
		return TicketPRClosed
	}
	if draft {
		return TicketPRDraft
	}
	return TicketPROpen
}
