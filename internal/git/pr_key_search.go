package git

import (
	"context"
	"fmt"

	"agent-overflow/internal/forgeapi"
)

// The reads a ticket's Work makes of the forge
// (docs/architecture/forge-transport.md#ticket-work-search): the pull
// requests and branches of a repository the forge's search finds by a
// ticket's key, and the pull requests of a branch in any state
// (ListPRsByHead). The forge's search is looser than the key rule, so the
// caller keeps only the rows and branches that name the key.

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

// PRKeySearch is what SearchPRsByKey found: the pull requests its title
// and head searches matched and the names of the repository's branches
// that hold the key, each search read up to keySearchMaxPages pages of
// 100. Full is set when a search had more, so these may not be all. A
// search that failed answers what it read before the failure beside its
// error.
type PRKeySearch struct {
	Rows     []PRFound
	Branches []string
	Full     bool
}

// keySearchMaxPages bounds each of a key search's searches: its matches
// are looser than the key rule (WEB-1 matches WEB-1000), so an exact match
// can sit behind a page of loose ones, and a repository with more than
// this many is said to be searched in part.
const keySearchMaxPages = 5

// SearchPRsByKey reads the pull requests of repo, in any state, and the
// branches that the forge's search finds for key: on GitHub the pull
// requests whose title holds it as a phrase or whose head branch begins
// with it, and the branches whose name holds it, in one request a page;
// on GitLab the merge requests whose title holds it and the branches whose
// name holds it, in two. key is a ticket key, letters, digits,
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
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
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

// githubPullsByKeyQuery is the three searches of a key, a page each, in
// one request at the cost of one: the title search matches whole tokens
// in order, the head search is a prefix match on the branch name, and the
// refs search a substring match on the repository's branch names, each
// ignoring case (measured in docs/references/forge-api-measurements.md).
// The two pull request searches cannot be joined with OR in one query. A
// search with no page left is left out of the next request.
var githubPullsByKeyQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query PullsByKey($owner: String!, $name: String!, $title: String!, $head: String!, $key: String!, $wantTitle: Boolean!, $wantHead: Boolean!, $wantBranches: Boolean!, $titleAfter: String, $headAfter: String, $branchesAfter: String) {
  byTitle: search(type: ISSUE, query: $title, first: 100, after: $titleAfter) @include(if: $wantTitle) { pageInfo { hasNextPage endCursor } nodes { ... on PullRequest { `+githubFoundPullFields+` } } }
  byHead: search(type: ISSUE, query: $head, first: 100, after: $headAfter) @include(if: $wantHead) { pageInfo { hasNextPage endCursor } nodes { ... on PullRequest { `+githubFoundPullFields+` } } }
  repository(owner: $owner, name: $name) @include(if: $wantBranches) { refs(refPrefix: "refs/heads/", query: $key, first: 100, after: $branchesAfter) { pageInfo { hasNextPage endCursor } nodes { name } } }
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
	PageInfo githubPageInfo       `json:"pageInfo"`
	Nodes    []githubFoundPullRaw `json:"nodes"`
}

// githubKeySearchPart is one of a key search's searches as its pages are
// read: whether it has a page left and the cursor of that page, null
// before the first page and once none is left.
type githubKeySearchPart struct {
	want  bool
	after any
}

func (p *githubKeySearchPart) next(info githubPageInfo) {
	p.want, p.after = info.HasNextPage && info.EndCursor != "", nil
	if p.want {
		p.after = info.EndCursor
	}
}

// SearchPRsByKey reads repo's pull requests by title and by head branch
// and its branches by name, a page of each a request, while any has a
// page left, up to keySearchMaxPages requests. A pull request both find
// is a row of each; the caller lists a pull request once. A request that
// fails ends the search with the pages read before it.
func (f *githubForge) SearchPRsByKey(ctx context.Context, repo RepoReference, key string) (PRKeySearch, error) {
	owner, name, err := splitGitHubProject(repo.Project())
	if err != nil {
		return PRKeySearch{}, err
	}
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return PRKeySearch{}, err
	}
	scope := "repo:" + repo.Project() + " is:pr "
	title, head, branches := githubKeySearchPart{want: true}, githubKeySearchPart{want: true}, githubKeySearchPart{want: true}
	var out PRKeySearch
	for range keySearchMaxPages {
		vars := map[string]any{"owner": owner, "name": name, "title": scope + `in:title "` + key + `"`, "head": scope + "head:" + key, "key": key,
			"wantTitle": title.want, "wantHead": head.want, "wantBranches": branches.want,
			"titleAfter": title.after, "headAfter": head.after, "branchesAfter": branches.after}
		var raw struct {
			ByTitle    *githubFoundSearchRaw `json:"byTitle"`
			ByHead     *githubFoundSearchRaw `json:"byHead"`
			Repository *struct {
				Refs struct {
					PageInfo githubPageInfo `json:"pageInfo"`
					Nodes    []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"refs"`
			} `json:"repository"`
		}
		if _, err := client.GraphQL(ctx, githubPullsByKeyQuery, vars, &raw); err != nil {
			return out, err
		}
		for _, part := range []struct {
			read   *githubKeySearchPart
			answer *githubFoundSearchRaw
		}{{&title, raw.ByTitle}, {&head, raw.ByHead}} {
			if !part.read.want {
				continue
			}
			if part.answer == nil {
				return out, fmt.Errorf("GitHub answered no search for %s on %s", repo.Project(), repo.Host)
			}
			part.read.next(part.answer.PageInfo)
			for _, node := range part.answer.Nodes {
				// A search result that is not a pull request has no number.
				if node.Number <= 0 {
					continue
				}
				row, err := prFound(node.URL, node.Title, node.State, node.IsDraft, PRListAuthor{Login: node.Author.Login, Name: node.Author.Name}, node.HeadRefName)
				if err != nil {
					return out, err
				}
				out.Rows = append(out.Rows, row)
			}
		}
		if branches.want {
			if raw.Repository == nil {
				return out, fmt.Errorf("GitHub answered no repository %s on %s", repo.Project(), repo.Host)
			}
			branches.next(raw.Repository.Refs.PageInfo)
			for _, node := range raw.Repository.Refs.Nodes {
				out.Branches = append(out.Branches, node.Name)
			}
		}
		if !title.want && !head.want && !branches.want {
			break
		}
	}
	out.Full = title.want || head.want || branches.want
	return out, nil
}
