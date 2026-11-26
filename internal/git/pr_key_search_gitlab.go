package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"agent-overflow/internal/forgeapi"
)

// SearchPRsByKey searches repo's merge requests in any state whose title
// holds key: GitLab's search matches the title and description, narrowed
// to the title by `in`. GitLab has no search of source branch names.
func (f *gitlabForge) SearchPRsByKey(ctx context.Context, repo RepoReference, key string) (PRKeySearch, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return PRKeySearch{}, err
	}
	resp, err := client.Do(ctx, forgeapi.Request{
		Path:         gitlabProjectPath(repo.Project()) + "/merge_requests",
		Query:        url.Values{"state": {"all"}, "search": {key}, "in": {"title"}, "per_page": {strconv.Itoa(gitlabPageSize)}},
		MaxBodyBytes: gitlabMRListMaxBody,
	})
	if err != nil {
		return PRKeySearch{}, err
	}
	var raw []gitlabHeadMRRaw
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return PRKeySearch{}, fmt.Errorf("GitLab merge requests of %s naming %s: decode response: %w", repo.Project(), key, err)
	}
	out := PRKeySearch{Full: resp.Header.Get("X-Next-Page") != ""}
	for _, r := range raw {
		row, err := prFound(r.WebURL, r.Title, r.State, r.Draft, PRListAuthor{Login: r.Author.Username, Name: r.Author.Name}, r.SourceBranch)
		if err != nil {
			return PRKeySearch{}, err
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}
