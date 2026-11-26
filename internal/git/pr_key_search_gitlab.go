package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"

	"agent-overflow/internal/forgeapi"
)

// SearchPRsByKey searches repo's merge requests in any state whose title
// holds key (GitLab's search matches the title and description, narrowed
// to the title by `in`) and its branches whose name holds key (the
// Branches API's search, a substring match ignoring case), the two at
// once, each up to keySearchMaxPages pages. GitLab's merge request list
// has no search of source branch names, so the caller looks up the
// branches it keeps.
func (f *gitlabForge) SearchPRsByKey(ctx context.Context, repo RepoReference, key string) (PRKeySearch, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return PRKeySearch{}, err
	}
	project := gitlabProjectPath(repo.Project())
	var out PRKeySearch
	var titleFull, branchesFull bool
	var titleErr, branchesErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		titleFull, titleErr = gitlabSearchPages(ctx, client, forgeapi.Request{
			Path:         project + "/merge_requests",
			Query:        url.Values{"state": {"all"}, "search": {key}, "in": {"title"}, "per_page": {strconv.Itoa(gitlabPageSize)}},
			MaxBodyBytes: gitlabMRListMaxBody,
		}, func(body []byte) error {
			var raw []gitlabHeadMRRaw
			if err := json.Unmarshal(body, &raw); err != nil {
				return fmt.Errorf("GitLab merge requests of %s naming %s: decode response: %w", repo.Project(), key, err)
			}
			for _, r := range raw {
				row, err := prFound(r.WebURL, r.Title, r.State, r.Draft, PRListAuthor{Login: r.Author.Username, Name: r.Author.Name}, r.SourceBranch)
				if err != nil {
					return err
				}
				out.Rows = append(out.Rows, row)
			}
			return nil
		})
	})
	wg.Go(func() {
		branchesFull, branchesErr = gitlabSearchPages(ctx, client, forgeapi.Request{
			Path:         project + "/repository/branches",
			Query:        url.Values{"search": {key}, "per_page": {strconv.Itoa(gitlabPageSize)}},
			MaxBodyBytes: gitlabMRListMaxBody,
		}, func(body []byte) error {
			var raw []struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(body, &raw); err != nil {
				return fmt.Errorf("GitLab branches of %s naming %s: decode response: %w", repo.Project(), key, err)
			}
			for _, b := range raw {
				out.Branches = append(out.Branches, b.Name)
			}
			return nil
		})
	})
	wg.Wait()
	if titleErr != nil {
		return PRKeySearch{}, titleErr
	}
	if branchesErr != nil {
		return PRKeySearch{}, branchesErr
	}
	out.Full = titleFull || branchesFull
	return out, nil
}

// gitlabSearchPages reads request's pages into visit, up to
// keySearchMaxPages, and reports whether a page was left unread.
func gitlabSearchPages(ctx context.Context, client *forgeapi.Client, request forgeapi.Request, visit func(body []byte) error) (bool, error) {
	pages, left := 0, false
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		if err := visit(resp.Body); err != nil {
			return false, err
		}
		pages++
		more := resp.Header.Get("X-Next-Page") != ""
		left = more && pages == keySearchMaxPages
		return more && !left, nil
	})
	return left, err
}
