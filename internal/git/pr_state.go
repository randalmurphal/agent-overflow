package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// OpenPRObservation is one fresh answer of the open-PR lookup for a branch:
// the forge, or the local check that origin lacks the branch, was asked,
// not the cache. PR is the branch's open PR, nil when it has none.
type OpenPRObservation struct {
	Cwd    string
	Branch string
	PR     *GitPR
}

// SetOpenPRObserver installs fn to receive every fresh open-PR answer
// lookupOpenPR reads. A failed lookup is not an answer and is not
// reported. fn runs on the lookup's goroutine, inside a git status
// refresh, so it must hand the observation off rather than do work.
func (c *Core) SetOpenPRObserver(fn func(OpenPRObservation)) {
	if fn == nil {
		c.openPRObserver.Store(nil)
		return
	}
	c.openPRObserver.Store(&fn)
}

func (c *Core) observeOpenPR(cwd, branch string, pr *GitPR) {
	if fn := c.openPRObserver.Load(); fn != nil {
		(*fn)(OpenPRObservation{Cwd: cwd, Branch: branch, PR: pr})
	}
}

// RefreshOpenPR drops cwd's cached open-PR answers and asks the forge for
// its current branch's open PR, so the observer and the next status read
// learn of a PR nothing on disk announced (an agent's `gh pr create`),
// whether or not a status stream watches cwd. A failed lookup is cached
// like any other; there is nothing for the caller to do with it.
func (c *Core) RefreshOpenPR(ctx context.Context, cwd string) {
	c.InvalidatePRCache(cwd)
	branch := c.CurrentBranch(cwd)
	if branch == "" || c.DetectForge(cwd) == "" {
		return
	}
	c.lookupOpenPR(ctx, cwd, branch)
}

// TrackedPR names a pull request whose state ReadPRStates reads: its
// reference and, on GitHub, the GraphQL node id the open-PR lookup
// reported for it (GitPR.ID).
type TrackedPR struct {
	Ref    PRReference
	NodeID string
}

// PRState is a pull request's state as ReadPRStates read it. State is
// NormalizePRState's vocabulary; MergedAt is zero unless it merged.
type PRState struct {
	State    string
	MergedAt time.Time
}

// prStatesBatch is how many PRs one ReadPRStates request names: GitHub's
// nodes(ids:) limit and GitLab's per_page maximum.
const prStatesBatch = 100

// ReadPRStates reads the state of every PR in prs, keyed by number. The
// PRs must all name one repository; a PR the forge no longer resolves is
// absent from the answer. Each request names at most 100 PRs, so a
// repository costs one request per read in practice.
func (c *Core) ReadPRStates(ctx context.Context, prs []TrackedPR) (map[int]PRState, error) {
	if len(prs) == 0 {
		return map[int]PRState{}, nil
	}
	repo := prs[0].Ref
	for _, pr := range prs {
		if err := pr.Ref.Validate(); err != nil {
			return nil, err
		}
		if pr.Ref.Forge != repo.Forge || pr.Ref.Host != repo.Host || pr.Ref.Project() != repo.Project() {
			return nil, fmt.Errorf("PR states read names more than one repository: %s and %s", repo.Key(), pr.Ref.Key())
		}
	}
	return c.ForgeByID(repo.Forge).ReadPRStates(ctx, repo.Repository(), prs)
}

// githubPRStatesQuery reads pull requests by node id. A node that does not
// resolve answers null beside a NOT_FOUND error.
const githubPRStatesQuery = `query PRStates($ids: [ID!]!) {
  nodes(ids: $ids) { ... on PullRequest { id number state mergedAt } }
}`

func (f *githubForge) ReadPRStates(ctx context.Context, repo RepoReference, prs []TrackedPR) (map[int]PRState, error) {
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	numbers := make(map[string]int, len(prs))
	ids := make([]string, 0, len(prs))
	for _, pr := range prs {
		if strings.TrimSpace(pr.NodeID) == "" {
			return nil, fmt.Errorf("GitHub PR %s has no node id to read its state by", pr.Ref.Key())
		}
		if _, dup := numbers[pr.NodeID]; !dup {
			ids = append(ids, pr.NodeID)
		}
		numbers[pr.NodeID] = pr.Ref.Number
	}
	states := make(map[int]PRState, len(ids))
	for start := 0; start < len(ids); start += prStatesBatch {
		batch := ids[start:min(start+prStatesBatch, len(ids))]
		var raw struct {
			Nodes []*struct {
				ID       string  `json:"id"`
				Number   int     `json:"number"`
				State    string  `json:"state"`
				MergedAt *string `json:"mergedAt"`
			} `json:"nodes"`
		}
		if _, err := client.GraphQL(ctx, "PRStates", githubPRStatesQuery, map[string]any{"ids": batch}, &raw); err != nil {
			if !onlyNotFound(err) {
				return nil, err
			}
		}
		for _, node := range raw.Nodes {
			if node == nil {
				continue
			}
			number, ok := numbers[node.ID]
			if !ok || number != node.Number {
				continue
			}
			state := PRState{State: NormalizePRState(node.State)}
			if node.MergedAt != nil {
				if state.MergedAt, err = time.Parse(time.RFC3339, *node.MergedAt); err != nil {
					return nil, fmt.Errorf("GitHub PR %d mergedAt %q: %w", node.Number, *node.MergedAt, err)
				}
			}
			states[number] = state
		}
	}
	return states, nil
}

// onlyNotFound reports whether err is a GraphQL answer whose every problem
// is NOT_FOUND: nodes(ids:) answers an id that no longer resolves that way,
// beside the data of the ids that do.
func onlyNotFound(err error) bool {
	gql, ok := errors.AsType[*forgeapi.GraphQLError](err)
	if !ok || len(gql.Problems) == 0 {
		return false
	}
	for _, problem := range gql.Problems {
		if problem.Type != "NOT_FOUND" {
			return false
		}
	}
	return true
}

func (f *gitlabForge) ReadPRStates(ctx context.Context, repo RepoReference, prs []TrackedPR) (map[int]PRState, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	iids := make([]string, 0, len(prs))
	seen := make(map[int]struct{}, len(prs))
	for _, pr := range prs {
		if _, dup := seen[pr.Ref.Number]; dup {
			continue
		}
		seen[pr.Ref.Number] = struct{}{}
		iids = append(iids, strconv.Itoa(pr.Ref.Number))
	}
	states := make(map[int]PRState, len(iids))
	for start := 0; start < len(iids); start += prStatesBatch {
		batch := iids[start:min(start+prStatesBatch, len(iids))]
		var raw []struct {
			IID      int     `json:"iid"`
			State    string  `json:"state"`
			MergedAt *string `json:"merged_at"`
		}
		request := forgeapi.Request{
			Path:  gitlabProjectPath(repo.Project()) + "/merge_requests",
			Query: url.Values{"iids[]": batch, "state": {"all"}, "per_page": {strconv.Itoa(prStatesBatch)}},
		}
		if _, err := client.JSON(ctx, request, &raw); err != nil {
			return nil, err
		}
		for _, mr := range raw {
			if _, ok := seen[mr.IID]; !ok {
				continue
			}
			state := PRState{State: NormalizePRState(mr.State)}
			if mr.MergedAt != nil {
				if state.MergedAt, err = time.Parse(time.RFC3339Nano, *mr.MergedAt); err != nil {
					return nil, fmt.Errorf("GitLab MR !%d merged_at %q: %w", mr.IID, *mr.MergedAt, err)
				}
			}
			states[mr.IID] = state
		}
	}
	return states, nil
}
