package appupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// releaseFeed is the release host half of targetableProvider: the listing,
// one release by tag, and a small sidecar such as SHASUMS256. Matching,
// checksum parsing and verification stay in targetableProvider and
// verifiedProvider, so every feed answers "which asset is this host's" the
// same way.
//
// The latest check and the artifact download are not here. On GitHub they
// belong to the stock provider (targetableProvider.inner); the GitLab feed
// serves them through gitlabProvider.
type releaseFeed interface {
	// name labels the feed in errors: "github" or "gitlab".
	name() string
	// list returns up to releaseListPageSize releases, newest first.
	list(ctx context.Context) ([]apiRelease, error)
	// byTag returns the release published under tag. The tag is validated
	// by the caller.
	byTag(ctx context.Context, tag string) (apiRelease, error)
	// readSidecar returns the bytes of a small release asset, read through
	// at most limit bytes.
	readSidecar(ctx context.Context, asset apiAsset, limit int64) ([]byte, error)
}

// githubFeed reads the GitHub releases API with no credential.
type githubFeed struct {
	repo       string // "owner/repo"
	baseURL    string // GitHub API base (overridable for tests)
	httpClient *http.Client
}

func newGitHubFeed(repo, baseURL string, client *http.Client) *githubFeed {
	if baseURL == "" {
		baseURL = defaultGitHubAPIBase
	}
	return &githubFeed{repo: repo, baseURL: baseURL, httpClient: client}
}

func (f *githubFeed) name() string { return "github" }

func (f *githubFeed) list(ctx context.Context) ([]apiRelease, error) {
	var raw []apiRelease
	endpoint := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", f.baseURL, f.repo, releaseListPageSize)
	if err := f.getJSON(ctx, endpoint, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (f *githubFeed) byTag(ctx context.Context, tag string) (apiRelease, error) {
	var rel apiRelease
	endpoint := f.baseURL + "/repos/" + f.repo + "/releases/tags/" + url.PathEscape(tag)
	err := f.getJSON(ctx, endpoint, &rel)
	return rel, err
}

func (f *githubFeed) readSidecar(ctx context.Context, asset apiAsset, limit int64) ([]byte, error) {
	return f.getRaw(ctx, asset.BrowserDownloadURL, limit)
}

func (f *githubFeed) getJSON(ctx context.Context, endpoint string, dst any) error {
	resp, err := f.do(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("GET %s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxReleaseListBytes)).Decode(dst)
}

func (f *githubFeed) getRaw(ctx context.Context, endpoint string, limit int64) ([]byte, error) {
	resp, err := f.do(ctx, endpoint, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %d", endpoint, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// do issues the GET. It sets no Authorization header — this build targets a
// public repo with no token — so following GitHub's cross-host asset redirect
// (api.github.com → the release CDN) carries no credential to leak. If a token
// is ever added here, add redirect-stripping too (cf. the stock provider's
// followAndStrip), or a cross-host hop would forward it.
func (f *githubFeed) do(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	return f.httpClient.Do(req)
}
