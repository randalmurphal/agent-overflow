package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// TicketWork is what a ticket's Work lists besides its threads, which the
// client reads from its thread rows: the pull requests and branches whose
// name or title names the ticket's key (trackerapp.TicketKeys).
type TicketWork struct {
	// Key is the ticket's key; Kind and Site name its tracker site as a
	// thread's link names it, so the client finds the ticket's threads.
	Key          string              `json:"key"`
	Kind         string              `json:"kind"`
	Site         string              `json:"site"`
	PullRequests []TicketPullRequest `json:"pullRequests"`
	Branches     []TicketBranch      `json:"branches"`
	// Problems are the repositories whose branches could not be read,
	// each with why; the rest of the answer stands beside them.
	Problems []TicketWorkProblem `json:"problems"`
}

// TicketPullRequest is a pull request of a ticket's Work. An open one is
// a row of the stored Pull requests list, with its title, author and CI.
// A merged or closed one is the pull request a thread linked to the
// ticket was last seen with, which records no title, author or CI.
type TicketPullRequest struct {
	Ref    gitops.PRReference `json:"ref"`
	URL    string             `json:"url"`
	Title  string             `json:"title,omitempty"`
	Author string             `json:"author,omitempty"`
	// State is draft, open, merged or closed.
	State     string `json:"state"`
	CI        string `json:"ci,omitempty"`
	Branch    string `json:"branch"`
	ProjectID string `json:"projectId,omitempty"`
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

// GetTicketWork lists the pull requests and branches of the ticket issueID
// that connectionID reads, from what this computer stores and from git's
// refs, with no forge or tracker request: the stored Pull requests list
// (open pull requests), the pull requests its linked threads were last
// seen with (merged and closed ones too), and the branches of the projects
// that list a board of the connection or hold one of those pull requests.
// The ticket's key is the one its mirror holds, so the ticket must have
// been read on this computer, as an open ticket has.
//
//ao:scope git:operate
//ao:route home
func (a *App) GetTicketWork(ctx context.Context, connectionID, issueID string) (TicketWork, error) {
	if err := a.requireScope(ctx, transport.ScopeTrackerRead, "GetTicketWork reads the ticket's key"); err != nil {
		return TicketWork{}, err
	}
	svc, err := a.trackerService()
	if err != nil {
		return TicketWork{}, err
	}
	conn, err := a.store.GetTrackerConnection(connectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return TicketWork{}, errorsx.Public("tracker_connection_missing", "this computer has no such tracker connection; it may have been removed", err)
	}
	if err != nil {
		return TicketWork{}, err
	}
	key, err := a.mirroredTicketKey(connectionID, issueID)
	if err != nil {
		return TicketWork{}, err
	}
	work := TicketWork{Key: key, Kind: conn.Kind, Site: conn.Site, PullRequests: []TicketPullRequest{}, Branches: []TicketBranch{}, Problems: []TicketWorkProblem{}}

	open, err := a.storedTicketPullRequests(key)
	if err != nil {
		return TicketWork{}, err
	}
	recorded, err := a.store.ListTicketThreadPullRequests(store.TicketRef{Kind: conn.Kind, Site: conn.Site, IssueID: issueID})
	if err != nil {
		return TicketWork{}, err
	}
	work.PullRequests = open
	for _, pr := range recorded {
		row, ok := recordedTicketPullRequest(pr)
		if ok && !slices.ContainsFunc(work.PullRequests, func(p TicketPullRequest) bool { return p.Ref.Identity() == row.Ref.Identity() }) {
			work.PullRequests = append(work.PullRequests, row)
		}
	}

	choices, err := svc.BoardChoices()
	if err != nil {
		return TicketWork{}, trackerError(err)
	}
	var projects []string
	for _, choice := range choices {
		if choice.Connection == connectionID && !choice.ProjectArchived && !slices.Contains(projects, choice.Project) {
			projects = append(projects, choice.Project)
		}
	}
	for _, pr := range work.PullRequests {
		if pr.ProjectID != "" && !slices.Contains(projects, pr.ProjectID) {
			projects = append(projects, pr.ProjectID)
		}
	}
	for _, projectID := range projects {
		branches, err := a.ticketBranches(projectID, key, work.PullRequests)
		if err != nil {
			work.Problems = append(work.Problems, TicketWorkProblem{ProjectID: projectID, Error: err.Error()})
			continue
		}
		work.Branches = append(work.Branches, branches...)
	}
	return work, nil
}

// mirroredTicketKey is the key of issueID in connectionID's mirror.
func (a *App) mirroredTicketKey(connectionID, issueID string) (string, error) {
	root, ok, err := a.store.GetMirrorRoot(connectionID)
	if err != nil {
		return "", err
	}
	if ok {
		cards, err := a.store.ListMirrorCards(root, []string{issueID})
		if err != nil {
			return "", err
		}
		if len(cards) == 1 && cards[0].Key != "" {
			return cards[0].Key, nil
		}
	}
	return "", errorsx.Public("ticket_not_read", "this computer has not read the ticket yet: open it first", fmt.Errorf("issue %s of connection %s is not in its mirror", issueID, connectionID))
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
			if !slices.Contains(trackerapp.TicketKeys(row.Title+" "+row.HeadRefName), key) {
				continue
			}
			state := TicketPROpen
			if row.Draft {
				state = TicketPRDraft
			}
			out = append(out, TicketPullRequest{
				Ref: row.Ref, URL: row.URL, Title: row.Title, Author: row.Author.Login, State: state,
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

// ticketBranches lists the branches of projectID whose name names key,
// leaving out a branch a listed pull request of the project already names.
func (a *App) ticketBranches(projectID, key string, prs []TicketPullRequest) ([]TicketBranch, error) {
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	branches, err := a.gitCore().ListBranches(project.Path)
	if err != nil {
		return nil, err
	}
	var out []TicketBranch
	for _, b := range branches {
		if !slices.Contains(trackerapp.TicketKeys(b.Name), key) {
			continue
		}
		if slices.ContainsFunc(prs, func(pr TicketPullRequest) bool {
			return pr.Branch == b.Name && (pr.ProjectID == projectID || pr.ProjectID == "")
		}) {
			continue
		}
		out = append(out, TicketBranch{ProjectID: projectID, GitBranch: b})
	}
	return out, nil
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
