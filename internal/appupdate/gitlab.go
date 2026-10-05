package appupdate

// The GitLab release feed.
//
// A build linked with -X agent-overflow/internal/appupdate.gitlabProject=
// HOST/NAMESPACE/PROJECT (scripts/build-release-noremote.sh) reads its
// releases from that project instead of GitHub. The project is private, so
// every request goes through the user's own `glab` login: `glab api
// --hostname HOST` attaches the token for HOST, and this package never sees,
// stores or logs it.
//
// glab sends that token to any absolute URL it is handed. Release links are
// data from the release, so a link is only fetched after it is reduced to a
// path relative to https://HOST/api/v4/ (gitlabAPIPath); a link to another
// host, another scheme or anything outside the REST API is refused before
// glab runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// GlabRunner runs `glab api ARGS...`, streaming the response body into dst.
// It fails once more than limit bytes arrive and wraps an error dst returns.
// A missing glab wraps exec.ErrNotFound. A non-zero exit is reported through
// exitCode and the bounded stderr, not err. The ctx deadline bounds the run.
// internal/git's Core.StreamGitLabAPI is the production runner.
type GlabRunner func(ctx context.Context, args []string, dst io.Writer, limit int64) (exitCode int, stderr string, err error)

var (
	// ErrGlabNotInstalled reports that the GitLab feed cannot run glab.
	ErrGlabNotInstalled = errors.New("glab is not installed")
	// ErrGlabSignedOut reports that glab has no usable login for the host.
	ErrGlabSignedOut = errors.New("sign in with glab to check for updates")
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
// bare DNS name, which is all `glab api --hostname` accepts, and each path
// segment is a plain GitLab path.
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
	if config.GlabRunner == nil {
		return nil, errors.New("updater: the GitLab release feed needs a glab runner")
	}
	feed := &gitlabFeed{host: source.host, project: source.project, run: config.GlabRunner}
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
	return p.feed.stream(ctx, path, out, maxDownloadBytes)
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

// gitlabFeed reads one project's releases through glab.
type gitlabFeed struct {
	host    string
	project string
	run     GlabRunner
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
	if err := f.stream(ctx, path, &cappedWriter{dst: &body, remaining: limit}, limit); err != nil {
		return nil, fmt.Errorf("read %s: %w", asset.Name, err)
	}
	return body.Bytes(), nil
}

func (f *gitlabFeed) getJSON(ctx context.Context, path string, dst any) error {
	var body bytes.Buffer
	if err := f.stream(ctx, path, &cappedWriter{dst: &body, remaining: maxReleaseListBytes}, maxReleaseListBytes); err != nil {
		return err
	}
	if err := json.Unmarshal(body.Bytes(), dst); err != nil {
		return fmt.Errorf("decode GitLab response: %w", err)
	}
	return nil
}

// glabStreamSlack is how far past a caller's cap the runner's own limit
// sits. dst enforces the cap and names the refusal; the runner checks each
// chunk against its limit before dst sees it, so its limit must clear the
// cap by more than one pipe read.
const glabStreamSlack = 1 << 20

// stream runs one `glab api` GET of path, relative to the host's API root,
// into dst, which enforces limit. "--" ends glab's flags, so a path can
// never be read as one.
func (f *gitlabFeed) stream(ctx context.Context, path string, dst io.Writer, limit int64) error {
	exitCode, stderr, err := f.run(ctx, []string{"--hostname", f.host, "--", path}, dst, limit+glabStreamSlack)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%w: install the GitLab CLI and run `glab auth login --hostname %s` to receive updates", ErrGlabNotInstalled, f.host)
		}
		return err
	}
	if exitCode != 0 {
		return f.exitFailure(exitCode, stderr)
	}
	return nil
}

// exitFailure names a failed glab call in the user's terms. glab prints
// the API's message on stderr, e.g. "glab: 404 Project Not Found (HTTP
// 404)". GitLab answers a private project it will not show the caller with
// 404, so a signed-out user can land on either of the first two.
func (f *gitlabFeed) exitFailure(exitCode int, stderr string) error {
	summary := stderrSummary(stderr)
	lower := strings.ToLower(summary)
	switch {
	case strings.Contains(lower, "unauthenticated") ||
		strings.Contains(lower, "401") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "glab auth login"):
		return fmt.Errorf("%w: run `glab auth login --hostname %s`", ErrGlabSignedOut, f.host)
	case strings.Contains(lower, "project not found"):
		return fmt.Errorf("%w: no access to %s/%s; sign in with glab as an account that can read it", ErrGitLabNoAccess, f.host, f.project)
	case summary == "":
		return fmt.Errorf("glab api exited %d", exitCode)
	default:
		return fmt.Errorf("glab api exited %d: %s", exitCode, summary)
	}
}

// stderrSummaryLimit bounds the glab diagnostics carried into an error.
const stderrSummaryLimit = 512

// stderrSummary is glab's diagnostics as one bounded line.
func stderrSummary(stderr string) string {
	summary := strings.Join(strings.Fields(stderr), " ")
	if len(summary) > stderrSummaryLimit {
		summary = summary[:stderrSummaryLimit] + "…"
	}
	return summary
}

// apiPath reduces a release link to a path relative to the host's REST API
// root, or refuses it. glab attaches the user's token to whatever URL it is
// given, so only https links on the configured host under /api/v4/ are
// fetched, and the relative form is what reaches glab.
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
	// ":" would let glab substitute a placeholder such as :id, and an empty
	// or dot segment would not name the file the link names.
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
