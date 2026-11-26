package git

import (
	"context"
	"fmt"

	"agent-overflow/internal/forgeapi"
)

// The reads a ticket's Work makes of the forge
// (docs/architecture/forge-transport.md#ticket-work-search): the pull
// requests of a repository the forge's search finds by a ticket's key, and
// the pull requests of a checkout's branch in any state (ListPRsByHead).
// The forge's search is looser than the key rule, so the caller keeps only
// the rows whose title or branch names the key.

// PRFound is a pull or merge request one of these reads found.
type PRFound struct {
	Ref   PRReference
	URL   string
	Title string
	// State is open, closed or merged (NormalizePRState; GitLab's locked,
	// a merge in progress, reads as itself).
	State       string
	Draft       bool
	Author      PRListAuthor
	HeadRefName string
}

// prFound is the row of a forge's answer, its reference read from its URL
// as every pull request reference is (ParsePRURL).
func prFound(url, title, state string, draft bool, author PRListAuthor, head string) (PRFound, error) {
	ref, err := ParsePRURL(url)
	if err != nil {
		return PRFound{}, fmt.Errorf("the forge answered a pull request whose URL %q the app cannot address: %w", url, err)
	}
	return PRFound{Ref: ref, URL: url, Title: title, State: NormalizePRState(state), Draft: draft, Author: author, HeadRefName: head}, nil
}

// PRKeySearch is what SearchPRsByKey found. Each search reads one page of
// 100, since no ticket key is named by more pull requests of one
// repository; Full is set when a search had more, so the rows may not be
// all of them.
type PRKeySearch struct {
	Rows []PRFound
	Full bool
}

// SearchPRsByKey reads the pull requests of repo, in any state, that the
// forge's search finds for key: on GitHub those whose title holds it as a
// phrase or whose head branch begins with it, in one request; on GitLab
// those whose title holds it. key is a ticket key, letters, digits,
// underscores and dashes, so it cannot change the query around it.
func (c *Core) SearchPRsByKey(ctx context.Context, repo RepoReference, key string) (PRKeySearch, error) {
	if err := repo.Validate(); err != nil {
		return PRKeySearch{}, err
	}
	if !isSearchableKey(key) {
		return PRKeySearch{}, fmt.Errorf("search the pull requests of %s: %q is not a ticket key", repo.Key(), key)
	}
	return c.ForgeByID(repo.Forge).SearchPRsByKey(ctx, repo, key)
}

// ListPRsByHead returns the pull requests in any state whose head is the
// given branch of cwd's origin repository itself, and of its parent when
// the origin is a fork, as ListOpenPRs reads the open ones.
func (c *Core) ListPRsByHead(ctx context.Context, cwd, head string) ([]PRFound, error) {
	return c.forgeFor(cwd).ListPRsByHead(ctx, cwd, head)
}

func isSearchableKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (nullForge) SearchPRsByKey(context.Context, RepoReference, string) (PRKeySearch, error) {
	return PRKeySearch{}, ErrUnsupportedForge
}

func (nullForge) ListPRsByHead(context.Context, string, string) ([]PRFound, error) {
	return nil, ErrUnsupportedForge
}

// githubFoundPullFields is what a found pull request carries.
const githubFoundPullFields = `url number title state isDraft headRefName ` + githubActor

// githubPullsByKeyQuery is the two searches of a key in one request, at
// the cost of one: the title search matches whole tokens in order and
// the head search is a prefix match on the branch name (measured in
// docs/references/forge-api-measurements.md), and the two cannot be
// joined with OR in one query.
var githubPullsByKeyQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullsByKey($title: String!, $head: String!) {
  byTitle: search(type: ISSUE, query: $title, first: 100) { issueCount nodes { ... on PullRequest { `+githubFoundPullFields+` } } }
  byHead: search(type: ISSUE, query: $head, first: 100) { issueCount nodes { ... on PullRequest { `+githubFoundPullFields+` } } }
}`)

type githubFoundPullRaw struct {
	URL         string         `json:"url"`
	Number      int            `json:"number"`
	Title       string         `json:"title"`
	State       string         `json:"state"`
	IsDraft     bool           `json:"isDraft"`
	HeadRefName string         `json:"headRefName"`
	Author      githubActorRaw `json:"author"`
}

type githubFoundSearchRaw struct {
	IssueCount int                  `json:"issueCount"`
	Nodes      []githubFoundPullRaw `json:"nodes"`
}

// SearchPRsByKey searches repo's pull requests by title and by head
// branch in one GraphQL request. A pull request both find is a row of
// each; the caller lists a pull request once.
func (f *githubForge) SearchPRsByKey(ctx context.Context, repo RepoReference, key string) (PRKeySearch, error) {
	if _, _, err := splitGitHubProject(repo.Project()); err != nil {
		return PRKeySearch{}, err
	}
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return PRKeySearch{}, err
	}
	scope := "repo:" + repo.Project() + " is:pr "
	vars := map[string]any{"title": scope + `in:title "` + key + `"`, "head": scope + "head:" + key}
	var raw struct {
		ByTitle *githubFoundSearchRaw `json:"byTitle"`
		ByHead  *githubFoundSearchRaw `json:"byHead"`
	}
	if _, err := client.GraphQL(ctx, githubPullsByKeyQuery, vars, &raw); err != nil {
		return PRKeySearch{}, err
	}
	if raw.ByTitle == nil || raw.ByHead == nil {
		return PRKeySearch{}, fmt.Errorf("GitHub answered no search for %s on %s", repo.Project(), repo.Host)
	}
	var out PRKeySearch
	for _, search := range []*githubFoundSearchRaw{raw.ByTitle, raw.ByHead} {
		out.Full = out.Full || search.IssueCount > len(search.Nodes)
		for _, node := range search.Nodes {
			// A search result that is not a pull request has no number.
			if node.Number <= 0 {
				continue
			}
			row, err := prFound(node.URL, node.Title, node.State, node.IsDraft, PRListAuthor{Login: node.Author.Login, Name: node.Author.Name}, node.HeadRefName)
			if err != nil {
				return PRKeySearch{}, err
			}
			out.Rows = append(out.Rows, row)
		}
	}
	return out, nil
}
