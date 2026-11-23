// Package updatefake answers the provider release hosts in process, for
// isolated boots and tests, so provider update checks and release notes run
// end to end through the real client without a network.
//
// It serves the Claude channel endpoints, the Codex npm manifest and latest
// GitHub release, and for both GitHub repositories the matching-refs
// listing and releases by tag, at the exact URLs providerupdate requests.
// Every other request is left to the caller. Its catalogue is fixed: stable
// releases 99.0.0 (the mock provider's own version), 99.0.1 and 99.1.0, and
// a 99.1.0-beta.1 prerelease tag that a notes lookup must filter out. The
// advertised latest version defaults to 99.0.0, so an isolated boot offers
// no update until a harness or test advertises a later one.
//
// MockInstall is the other side of the same isolated world: the CLI
// install ao-mockprovider simulates, whose update installs the version the
// harness advertised.
package updatefake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"

	"agent-overflow/internal/providerupdate"
)

// Baseline is the mock provider's built-in version and the default latest.
const Baseline = "99.0.0"

// Stable is every stable version in the catalogue, oldest first.
var Stable = []string{Baseline, "99.0.1", "99.1.0"}

// prerelease is a tag with a release that is not a stable version.
const prerelease = "99.1.0-beta.1"

var repos = []struct {
	repo, tagPrefix string
}{
	{"anthropics/claude-code", "v"},
	{"openai/codex", "rust-v"},
}

// Feed is one isolated boot's release hosts. Safe for concurrent use.
type Feed struct {
	mu     sync.Mutex
	latest string
}

// New returns a Feed advertising Baseline.
func New() *Feed { return &Feed{latest: Baseline} }

// SetLatest advertises version, one of Stable, for both providers. Releases
// after it are not published yet: their tags answer 404.
func (f *Feed) SetLatest(version string) error {
	if !slices.Contains(Stable, version) {
		return fmt.Errorf("updatefake: latest must be one of %v, got %q", Stable, version)
	}
	f.mu.Lock()
	f.latest = version
	f.mu.Unlock()
	return nil
}

// Reset advertises Baseline again and reports whether it changed anything.
func (f *Feed) Reset() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	changed := f.latest != Baseline
	f.latest = Baseline
	return changed
}

// Latest is the advertised version.
func (f *Feed) Latest() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest
}

// Serve answers req when it is a request to a release host the feed fakes.
// ok is false otherwise, and the caller answers the request.
func (f *Feed) Serve(req *http.Request) (resp *http.Response, ok bool, err error) {
	if f == nil || req.Method != http.MethodGet || req.URL.RawQuery != "" || req.URL.Fragment != "" {
		return nil, false, nil
	}
	latest := f.Latest()
	url := req.URL.String()
	switch url {
	case providerupdate.ClaudeChannelURL("latest"), providerupdate.ClaudeChannelURL("stable"):
		return response(req, http.StatusOK, latest+"\n"), true, nil
	case providerupdate.CodexNPMLatestURL:
		return jsonResponse(req, map[string]string{"name": "@openai/codex", "version": latest})
	case providerupdate.CodexLatestReleaseURL:
		return release(req, "openai/codex", "rust-v", "rust-v"+latest, latest)
	case providerupdate.AgyManifestURL(runtime.GOOS, providerupdate.AgyArch()):
		return jsonResponse(req, map[string]string{"version": latest})
	case providerupdate.AgyChangelogURL:
		return response(req, http.StatusOK, "## Antigravity CLI\n\n- Mock release "+latest+"\n"), true, nil
	}
	for _, r := range repos {
		if prefix, found := strings.CutPrefix(url, providerupdate.MatchingRefsURL(r.repo, "")); found {
			return jsonResponse(req, matchingRefs(r.tagPrefix, prefix))
		}
		if tag, found := strings.CutPrefix(url, providerupdate.ReleaseByTagURL(r.repo, "")); found {
			return release(req, r.repo, r.tagPrefix, tag, latest)
		}
	}
	return nil, false, nil
}

type ref struct {
	Ref string `json:"ref"`
}

func matchingRefs(tagPrefix, prefix string) []ref {
	refs := []ref{}
	for _, version := range append(slices.Clone(Stable), prerelease) {
		if tag := tagPrefix + version; strings.HasPrefix(tag, prefix) {
			refs = append(refs, ref{Ref: "refs/tags/" + tag})
		}
	}
	return refs
}

func release(req *http.Request, repo, tagPrefix, tag, latest string) (*http.Response, bool, error) {
	version, ok := strings.CutPrefix(tag, tagPrefix)
	published := ok && (version == prerelease || slices.Contains(Stable[:slices.Index(Stable, latest)+1], version))
	if !published {
		return response(req, http.StatusNotFound, `{"message":"Not Found"}`), true, nil
	}
	return jsonResponse(req, map[string]any{
		"tag_name":     tag,
		"name":         version,
		"body":         "## " + version + "\n\n- Changes in " + version + ".\n",
		"html_url":     providerupdate.ReleasesPageURL(repo) + "/tag/" + tag,
		"published_at": "2026-11-01T00:00:00Z",
		"prerelease":   version == prerelease,
		"draft":        false,
	})
}

func jsonResponse(req *http.Request, payload any) (*http.Response, bool, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, true, errors.Join(errors.New("updatefake: encode answer"), err)
	}
	return response(req, http.StatusOK, string(body)), true, nil
}

func response(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
