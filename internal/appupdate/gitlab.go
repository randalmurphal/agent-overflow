package appupdate

// The GitLab release feed.
//
// A build linked with -X agent-overflow/internal/appupdate.gitlabProject=
// HOST/NAMESPACE/PROJECT (scripts/build-release-noremote.sh) reads its
// releases from that project instead of GitHub. The project is private, so
// every request is an HTTPS call to HOST's REST API through the injected
// GitLabClient, which the app backs with its forge API transport and the
// token of the user's own `glab` login. This package never sees, stores or
// logs the token.
//
// Release links are data from the release, so a link is only fetched after
// it is reduced to a path relative to https://HOST/api/v4/ (apiPath); a
// link to another host, another scheme or anything outside the REST API is
// refused before any request is made.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"golang.org/x/mod/semver"
)

// gitlabProject is the GitLab project this build updates from, as
// HOST/NAMESPACE/PROJECT. Set only by the release link step; empty selects
// the GitHub feed.
var gitlabProject string

// ReleaseProjectLinked reports whether this build was linked with a GitLab
// release project, which a build without remote access needs for updates.
func ReleaseProjectLinked() bool { return gitlabProject != "" }

// gitlabProviderName labels the GitLab feed in updater payloads and errors.
const gitlabProviderName = "gitlab"

// gitlabAssetURLKey is where resolveTag leaves a GitLab release link for
// gitlabProvider.Download.
const gitlabAssetURLKey = "gitlab.asset.url"

// gitlabAPIPrefix is the REST API root every fetched link must sit under.
const gitlabAPIPrefix = "/api/v4/"

// GitLabClient reads one GitLab host's REST API with the user's glab
// login. Stream GETs path, relative to the host's /api/v4/ root and
// accepting any content type, and writes the body to dst. It fails with an
// error wrapping forgeapi.ErrBodyTooLarge once more than limit bytes
// arrive, and with the transport's errors otherwise: a *forgeapi.SetupError
// when glab is missing or has no login for the host, a
// *forgeapi.StatusError for an answer outside 2xx. The ctx deadline bounds
// the whole call, body included. internal/app backs it with the forge API
// transport's Service.GitLab(host).
type GitLabClient interface {
	Stream(ctx context.Context, path string, dst io.Writer, limit int64) error
}

var (
	// ErrGitLabNoAccess reports that the release project is missing or not
	// readable by the signed-in account.
	ErrGitLabNoAccess = errors.New("no access to the release project")
	// errGitLabResponseTooLarge refuses a listing or sidecar over its cap.
	errGitLabResponseTooLarge = errors.New("the GitLab response exceeds its size limit")
)

var (
	gitlabHostPattern    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
	gitlabSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

// gitlabSource is a validated gitlabProject.
type gitlabSource struct {
	host    string // e.g. "gitlab.com"
	project string // full path, e.g. "group/subgroup/project"
}

// parseGitLabProject validates HOST/NAMESPACE.../PROJECT. The host is a
// bare DNS name, which is how glab keys its logins, and each path segment
// is a plain GitLab path.
func parseGitLabProject(raw string) (gitlabSource, error) {
	parts := strings.Split(raw, "/")
	if len(parts) < 3 {
		return gitlabSource{}, fmt.Errorf("updater: GitLab release project %q is not HOST/NAMESPACE/PROJECT", raw)
	}
	if !gitlabHostPattern.MatchString(parts[0]) {
		return gitlabSource{}, fmt.Errorf("updater: GitLab release project %q has an invalid host", raw)
	}
	for _, segment := range parts[1:] {
		if !gitlabSegmentPattern.MatchString(segment) || strings.Contains(segment, "..") {
			return gitlabSource{}, fmt.Errorf("updater: GitLab release project %q has an invalid path segment %q", raw, segment)
		}
	}
	return gitlabSource{host: strings.ToLower(parts[0]), project: strings.Join(parts[1:], "/")}, nil
}

// newGitLabTargetable builds the provider chain over the GitLab feed.
func newGitLabTargetable(project string, config Config, req updater.CheckRequest) (*targetableProvider, error) {
	source, err := parseGitLabProject(project)
	if err != nil {
		return nil, err
	}
	if config.GitLab == nil {
		return nil, errors.New("updater: the GitLab release feed needs a GitLab client")
	}
	client := config.GitLab(source.host)
	if client == nil {
		return nil, fmt.Errorf("updater: no GitLab client for %s", source.host)
	}
	feed := &gitlabFeed{host: source.host, project: source.project, client: client}
	inner := &gitlabProvider{feed: feed}
	targetable := newTargetableProvider(inner, feed, gitlabAssetURLKey, config.ChecksumAsset, req)
	inner.latest = targetable.resolveLatest
	return targetable, nil
}

// gitlabProvider serves the latest check and the artifact download for the
// GitLab feed. GitLab has no "latest release" that knows this host's asset,
// so latest is the newest installable entry of the listing.
type gitlabProvider struct {
	feed   *gitlabFeed
	latest func(ctx context.Context) (*updater.Release, error)
}

func (p *gitlabProvider) Name() string { return gitlabProviderName }

// Check resolves the newest installable release newer than the running one.
// The request is the one the targetable provider was built with, as for a
// by-tag resolve.
func (p *gitlabProvider) Check(ctx context.Context, _ updater.CheckRequest) (*updater.Release, error) {
	return p.latest(ctx)
}

// Download streams the release link resolveTag picked. GitLab release links
// carry no size, so progress reports the release's size, which is zero.
// verifiedProvider bounds dst.
func (p *gitlabProvider) Download(ctx context.Context, rel *updater.Release, dst io.Writer, onProgress func(written, total int64)) error {
	if rel == nil {
		return errors.New("gitlab: no release to download")
	}
	link, _ := rel.Metadata[gitlabAssetURLKey].(string)
	if link == "" {
		return errors.New("gitlab: release metadata names no asset link")
	}
	path, err := p.feed.apiPath(link)
	if err != nil {
		return err
	}
	out := &progressWriter{dst: dst, total: rel.Artifact.Size, onProgress: onProgress}
	return p.feed.stream(ctx, path, out, maxDownloadBytes, errDownloadTooLarge)
}

// progressWriter reports each write to onProgress.
type progressWriter struct {
	dst        io.Writer
	written    int64
	total      int64
	onProgress func(written, total int64)
}

func (w *progressWriter) Write(b []byte) (int, error) {
	n, err := w.dst.Write(b)
	w.written += int64(n)
	if n > 0 && w.onProgress != nil {
		w.onProgress(w.written, w.total)
	}
	return n, err
}

// gitlabFeed reads one project's releases over its host's REST API.
type gitlabFeed struct {
	host    string
	project string
	client  GitLabClient
}

// gitlabRelease is the subset of GitLab's release API the feed maps.
type gitlabRelease struct {
	TagName         string    `json:"tag_name"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	ReleasedAt      time.Time `json:"released_at"`
	UpcomingRelease bool      `json:"upcoming_release"`
	Assets          struct {
		Links []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"links"`
	} `json:"assets"`
}

// toAPIRelease maps a GitLab release onto the shape the provider matches.
// GitLab has no prerelease flag, so a semver prerelease tag is one. A
// release scheduled for the future is not published yet and is skipped like
// a GitHub draft. Links carry no size.
func (r gitlabRelease) toAPIRelease() apiRelease {
	out := apiRelease{
		TagName:     r.TagName,
		Name:        r.Name,
		Body:        r.Description,
		Prerelease:  semver.Prerelease(ensureVPrefix(r.TagName)) != "",
		Draft:       r.UpcomingRelease,
		PublishedAt: r.ReleasedAt,
	}
	for _, link := range r.Assets.Links {
		out.Assets = append(out.Assets, apiAsset{Name: link.Name, BrowserDownloadURL: link.URL})
	}
	return out
}

func (f *gitlabFeed) name() string { return gitlabProviderName }

func (f *gitlabFeed) projectPath() string {
	return "projects/" + url.PathEscape(f.project)
}

func (f *gitlabFeed) list(ctx context.Context) ([]apiRelease, error) {
	var raw []gitlabRelease
	path := f.projectPath() + "/releases?order_by=released_at&sort=desc&per_page=" + strconv.Itoa(releaseListPageSize)
	if err := f.getJSON(ctx, path, &raw); err != nil {
		return nil, err
	}
	out := make([]apiRelease, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.toAPIRelease())
	}
	return out, nil
}

func (f *gitlabFeed) byTag(ctx context.Context, tag string) (apiRelease, error) {
	var raw gitlabRelease
	if err := f.getJSON(ctx, f.projectPath()+"/releases/"+url.PathEscape(tag), &raw); err != nil {
		return apiRelease{}, err
	}
	return raw.toAPIRelease(), nil
}

func (f *gitlabFeed) readSidecar(ctx context.Context, asset apiAsset, limit int64) ([]byte, error) {
	path, err := f.apiPath(asset.BrowserDownloadURL)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	if err := f.stream(ctx, path, &cappedWriter{dst: &body, remaining: limit}, limit, errGitLabResponseTooLarge); err != nil {
		return nil, fmt.Errorf("read %s: %w", asset.Name, err)
	}
	return body.Bytes(), nil
}

func (f *gitlabFeed) getJSON(ctx context.Context, path string, dst any) error {
	var body bytes.Buffer
	if err := f.stream(ctx, path, &cappedWriter{dst: &body, remaining: maxReleaseListBytes}, maxReleaseListBytes, errGitLabResponseTooLarge); err != nil {
		return err
	}
	if err := json.Unmarshal(body.Bytes(), dst); err != nil {
		return fmt.Errorf("decode GitLab response: %w", err)
	}
	return nil
}

// stream GETs path, relative to the host's API root, into dst, which
// enforces limit with its own refusal. tooLarge is that refusal, returned
// as well when the transport refuses a declared length over limit before
// dst sees a byte. An error dst returns is returned as dst gave it, not as
// the transport's report of a failed body read.
func (f *gitlabFeed) stream(ctx context.Context, path string, dst io.Writer, limit int64, tooLarge error) error {
	out := &firstErrorWriter{dst: dst}
	err := f.client.Stream(ctx, path, out, limit)
	switch {
	case err == nil:
		return nil
	case out.err != nil:
		return out.err
	case errors.Is(err, forgeapi.ErrBodyTooLarge):
		return tooLarge
	}
	if setup, ok := errors.AsType[*forgeapi.SetupError](err); ok {
		return f.setupFailure(setup)
	}
	// GitLab answers a project it will not show the caller, signed in or
	// not, with 404 Project Not Found.
	if status, ok := errors.AsType[*forgeapi.StatusError](err); ok && status.Status == http.StatusNotFound &&
		strings.Contains(strings.ToLower(status.Body), "project not found") {
		return fmt.Errorf("%w: no access to %s/%s; sign in with glab as an account that can read it", ErrGitLabNoAccess, f.host, f.project)
	}
	return err
}

// setupFailure restates a glab login problem as the step that fixes it for
// this feed's host.
func (f *gitlabFeed) setupFailure(setup *forgeapi.SetupError) *forgeapi.SetupError {
	out := &forgeapi.SetupError{Forge: forgeapi.ForgeGitLab, Binary: "glab", Kind: setup.Kind, Err: setup}
	if setup.Kind == forgeapi.SetupMissing {
		out.Message = fmt.Sprintf("glab is not installed: install the GitLab CLI and run `glab auth login --hostname %s` to receive updates", f.host)
	} else {
		out.Message = fmt.Sprintf("sign in with glab to check for updates: run `glab auth login --hostname %s`", f.host)
	}
	return out
}

// firstErrorWriter remembers the first error dst returns.
type firstErrorWriter struct {
	dst io.Writer
	err error
}

func (w *firstErrorWriter) Write(b []byte) (int, error) {
	n, err := w.dst.Write(b)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// apiPath reduces a release link to a path relative to the host's REST API
// root, or refuses it. Every request carries the user's token, so only
// https links on the configured host under /api/v4/ are fetched, and the
// relative form is what reaches the client.
func (f *gitlabFeed) apiPath(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		// url.Error repeats the link, which could carry credentials.
		return "", errors.New("gitlab: refusing an unparsable release link")
	}
	switch {
	case u.Scheme != "https":
		return "", fmt.Errorf("gitlab: refusing a release link that is not https (scheme %q)", u.Scheme)
	case u.User != nil:
		return "", errors.New("gitlab: refusing a release link that carries credentials")
	case u.Port() != "" || !strings.EqualFold(u.Hostname(), f.host):
		return "", fmt.Errorf("gitlab: refusing a release link to %q; links must be on %s", u.Host, f.host)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("gitlab: refusing a release link with a query or fragment")
	}
	rel, ok := strings.CutPrefix(u.EscapedPath(), gitlabAPIPrefix)
	if !ok {
		return "", fmt.Errorf("gitlab: refusing a release link outside %s on %s", gitlabAPIPrefix, f.host)
	}
	// A ":" could make the relative path parse as a URL with a scheme, and
	// an empty or dot segment would not name the file the link names.
	if rel == "" || strings.Contains(rel, ":") {
		return "", errors.New("gitlab: refusing a release link with an unsafe API path")
	}
	for segment := range strings.SplitSeq(rel, "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." {
			return "", errors.New("gitlab: refusing a release link with an unsafe API path")
		}
	}
	return rel, nil
}

// cappedWriter fails once more than remaining bytes are written.
type cappedWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *cappedWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.remaining {
		return 0, errGitLabResponseTooLarge
	}
	n, err := w.dst.Write(b)
	w.remaining -= int64(n)
	return n, err
}
