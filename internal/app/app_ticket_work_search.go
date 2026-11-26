package app

import (
	"cmp"
	"context"
	"log"
	"slices"

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
// key, in two rounds of concurrent requests, at most
// prListReadConcurrency at a time:
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
// A found row or branch counts only when it names the key
// (namesTicketKey), since the forge's search is looser than the key rule.
// A repository's failure is its one ForgeProblem, beside the rows its
// search and lookups read before they failed; a failed search gets no
// lookups. A project whose origin
// names no forge repository is not searched.
func (a *App) searchTicketForge(ctx context.Context, read *ticketWorkRead) {
	var repos []*ticketSearchRepo
	byKey := map[string]*ticketSearchRepo{}
	for _, origin := range read.origins {
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
		repo.paths[origin.ProjectID] = origin.project.Path
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
			read.addFound(found, l.projectID)
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
}

// addFound lists a pull request the forge found for the key, of
// projectID, once. The forge's row is the newest read of it: it replaces a
// row listed already, which keeps only what the forge's row lacks, the
// stored list's CI and the project it was listed under.
func (r *ticketWorkRead) addFound(found gitops.PRFound, projectID string) {
	if !namesTicketKey(found.Title, found.HeadRefName, r.work.Key) {
		return
	}
	row := TicketPullRequest{
		Ref: found.Ref, URL: found.URL, Title: found.Title, Author: cmp.Or(found.Author.Name, found.Author.Login),
		State: ticketPRState(found.State, found.Draft), Branch: found.HeadRefName, ProjectID: projectID,
	}
	id := row.Ref.Identity()
	at := slices.IndexFunc(r.work.PullRequests, func(p TicketPullRequest) bool { return p.Ref.Identity() == id })
	if at < 0 {
		r.work.PullRequests = append(r.work.PullRequests, row)
		return
	}
	listed := r.work.PullRequests[at]
	row.CI = listed.CI
	if listed.ProjectID != "" {
		row.ProjectID = listed.ProjectID
	}
	r.work.PullRequests[at] = row
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
