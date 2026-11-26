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

// ticketHeadLookup is one branch of the ticket a key search left without
// a listed pull request, looked up by name in the checkout of a project
// that has it.
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
//     (gitops.Core.SearchPRsByKey), however many projects clone it;
//   - then, of a repository whose search answered, one lookup by name
//     (gitops.Core.ListPRsByHead) per branch of the ticket that no pull
//     request listed by then names, since a forge's search matches a key
//     in a branch name only at its start (GitHub) or not at all (GitLab).
//
// A found row counts only when its title or branch names the key
// (namesTicketKey), since the forge's search is looser than the key rule.
// A repository's failure is its one ForgeProblem. A project whose origin
// names no forge repository is not searched. It fails only when ctx ends
// before the repositories are known.
func (a *App) searchTicketForge(ctx context.Context, read *ticketWorkRead) error {
	var repos []*ticketSearchRepo
	byKey := map[string]*ticketSearchRepo{}
	for _, projectID := range read.projects {
		project, err := a.store.GetProject(projectID)
		if err != nil {
			// The branches' read failed the same way: it is in Problems.
			continue
		}
		row, ok, err := a.forgeProject(ctx, project)
		switch {
		case err != nil:
			return err
		case !ok:
			continue
		case row.Error != "":
			read.work.ForgeProblems = append(read.work.ForgeProblems, TicketForgeProblem{ProjectID: projectID, Error: row.Error, ErrorKind: forgeFailureForge})
			continue
		}
		key := row.Repo.Identity()
		repo := byKey[key]
		if repo == nil {
			repo = &ticketSearchRepo{repo: row.Repo, paths: map[string]string{}}
			byKey[key] = repo
			repos = append(repos, repo)
		}
		repo.projects = append(repo.projects, projectID)
		repo.paths[projectID] = project.Path
	}

	ticketKey := read.work.Key
	runBounded(len(repos), prListReadConcurrency, func(i int) {
		repos[i].found, repos[i].err = a.gitCore().SearchPRsByKey(ctx, repos[i].repo, ticketKey)
	})
	for _, repo := range repos {
		if repo.err != nil {
			continue
		}
		if repo.found.Full {
			read.work.SearchFull = append(read.work.SearchFull, repo.repo)
		}
		for _, found := range repo.found.Rows {
			read.addFound(found, repo.projects[0])
		}
	}

	var lookups []*ticketHeadLookup
	for _, repo := range repos {
		if repo.err != nil {
			continue
		}
		var names []string
		for _, b := range read.branches {
			if repo.paths[b.ProjectID] == "" || slices.Contains(names, b.Name) || branchNamedByPullRequest(b, read.work.PullRequests) {
				continue
			}
			names = append(names, b.Name)
			lookups = append(lookups, &ticketHeadLookup{repo: repo, projectID: b.ProjectID, branch: b.Name})
		}
	}
	runBounded(len(lookups), prListReadConcurrency, func(i int) {
		l := lookups[i]
		l.found, l.err = a.gitCore().ListPRsByHead(ctx, l.repo.paths[l.projectID], l.branch)
	})
	for _, l := range lookups {
		if l.err != nil {
			if l.repo.err == nil {
				l.repo.err = l.err
			}
			continue
		}
		for _, found := range l.found {
			read.addFound(found, l.projectID)
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
	return nil
}

// addFound lists a pull request the forge found for the key, of
// projectID, once: a row already listed stays when the forge answered its
// state (the stored row carries CI), and gives way to the found row when
// it is a linked thread's record or the forge answered another state,
// since the forge's answer is the newest read of it.
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
	switch {
	case at < 0:
		r.work.PullRequests = append(r.work.PullRequests, row)
	case r.recorded[id] || r.work.PullRequests[at].State != row.State:
		if listed := r.work.PullRequests[at]; listed.ProjectID != "" {
			row.ProjectID = listed.ProjectID
		}
		r.work.PullRequests[at] = row
		delete(r.recorded, id)
	}
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
