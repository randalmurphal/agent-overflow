package appupdate

// Version selection for the in-app updater. The stock Wails updater is
// latest-only: github.Provider.Check hits /releases/latest and gates on
// semver.IsNewer, and the Updater only ever installs that pending release.
//
// targetableProvider wraps the stock provider to add two things the framework
// doesn't expose: enumerate releases (ListReleases) and resolve a SPECIFIC tag
// for download — including an older one, so the user can roll back. The latest
// path (empty target) delegates straight to the stock provider, and the actual
// byte download is always the stock Provider.Download (it reads the asset URL
// off Release.Metadata, which resolveTag populates). Only the by-tag resolve
// and the release listing are reimplemented here; matching and downloading are
// reused. verifiedProvider still wraps this, so every selected version is
// checksum-verified or rejected fail-closed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
	"golang.org/x/mod/semver"
)

// Config identifies the release target one updater instance can install.
// Platform is explicit because the WSL backend downloads a Windows launcher
// artifact while running as a Linux process.
type Config struct {
	CurrentVersion string
	Platform       string
	Arch           string
	Repository     string
	ChecksumAsset  string
	BaseURL        string
	HTTPClient     *http.Client
}

// Configure wires a Wails updater handle to the GitHub release provider and
// retains the targetable provider used by ListReleases and tagged downloads.
//
// The chain comes from NewReleaseSource rather than being built here, so the
// desktop's updater and a serve host's ReleaseSource are the SAME resolve,
// the same matcher and the same fail-closed verification wrapper. Two
// constructors would be two answers to "which asset is this host's", and the
// one that is wrong is the one nobody is looking at.
func (a *Service) Configure(handle *updater.Updater, config Config) error {
	if handle == nil {
		return errors.New("updater: nil updater handle")
	}
	source, err := NewReleaseSource(config)
	if err != nil {
		return err
	}
	if err := handle.Init(updater.Config{
		CurrentVersion: source.req.CurrentVersion,
		Platform:       source.req.Platform,
		Arch:           source.req.Arch,
		Providers:      []updater.Provider{source.verified},
		Window:         updater.WindowNone,
	}); err != nil {
		return fmt.Errorf("init updater: %w", err)
	}

	a.updater.handle = handle
	a.updater.provider = source.targetable
	return nil
}

const (
	// maxReleaseListBytes caps the /releases JSON we'll decode (defensive
	// bound on a remote response). 30 releases of metadata is a few KB; 1 MiB
	// is generous headroom.
	maxReleaseListBytes = 1 << 20
	// maxChecksumBytes caps a SHASUMS256 sidecar read. It lists a handful of
	// filenames + hex digests — kilobytes — so 64 KiB is plenty.
	maxChecksumBytes = 64 << 10
	// releaseListPageSize bounds how many releases the picker offers. The app
	// has had under a dozen releases; one page is plenty and avoids paging.
	releaseListPageSize = 30
	// defaultGitHubAPIBase is the GitHub REST base. Overridable per-provider so
	// tests can point at an httptest server.
	defaultGitHubAPIBase = "https://api.github.com"
)

// releaseTagPattern validates a release tag before it is interpolated into a
// GitHub API URL path. Tags come from our own ListReleases output, but this is
// defense-in-depth against path traversal / URL injection if that ever changes.
var releaseTagPattern = regexp.MustCompile(`^v?[0-9A-Za-z][0-9A-Za-z.\-_+]{0,63}$`)

func validReleaseTag(tag string) bool { return releaseTagPattern.MatchString(tag) }

// ValidReleaseTag is the same rule for a caller outside this package. The
// remote update trigger (internal/app) refuses a tag SYNCHRONOUSLY, before it
// claims the one-flow fence and hands the work to a goroutine, so a bad
// argument comes back as a bad argument rather than as a failed update three
// frames later. It stays one predicate: a second spelling of "is this a tag"
// is how the two ends of one flow end up disagreeing about what they accept.
func ValidReleaseTag(tag string) bool { return validReleaseTag(tag) }

// ReleaseSummary describes one installable release for the version picker. Only
// releases that ship an asset for the running platform AND a checksum sidecar
// are surfaced — anything else can't be installed here, so it's omitted.
type ReleaseSummary struct {
	Tag         string `json:"tag"`         // e.g. "v0.0.7"
	Version     string `json:"version"`     // tag without the leading "v"
	Name        string `json:"name"`        // release title
	PublishedAt string `json:"publishedAt"` // RFC3339, or "" if absent
	Prerelease  bool   `json:"prerelease"`
	IsLatest    bool   `json:"isLatest"`  // newest stable (matches /releases/latest)
	IsCurrent   bool   `json:"isCurrent"` // same version as the running build
	IsOlder     bool   `json:"isOlder"`   // older than the running build (a downgrade)
}

// targetableProvider is an updater.Provider that resolves either the latest
// release (empty target, via the stock provider) or a specific tag.
//
// inner is the stock provider as an interface, not *github.Provider: this type
// only ever delegates Check (latest), Download, and Name to it, and keeping it
// behind the interface both loosens the coupling and lets tests substitute a
// recording fake for the delegation path.
//
// req is the build's own target: the platform/arch tokens release assets are
// named for, plus the running version. Every asset-matching decision this type
// makes — by-tag resolve, listReleases, installable — reads it, so a listing and
// a by-tag install can never disagree about what "this platform" means. It is
// NOT derivable from runtime.GOOS: the headless WSL backend is a linux/amd64
// process that installs `agent-overflow-wsl-amd64.exe` for the Windows launcher
// to swap in, so it targets platform "wsl".
type targetableProvider struct {
	inner         updater.Provider // stock provider: latest Check + Download
	repo          string           // "owner/repo"
	checksumAsset string           // sidecar asset name, e.g. "SHASUMS256"
	req           updater.CheckRequest
	baseURL       string // GitHub API base (overridable for tests)
	httpClient    *http.Client
	matcher       github.AssetMatcher // reused asset picker
	target        atomic.Pointer[string]
}

// newTargetableProvider builds the provider around an already-constructed stock
// github provider so both share the same matching/verification configuration.
//
// req must be the same CheckRequest the caller configures the Updater with
// (updater.Config's CurrentVersion / Platform / Arch), so the passive latest
// check the stock provider serves and the listing this type serves describe the
// same set of assets.
func newTargetableProvider(inner updater.Provider, repo, checksumAsset string, req updater.CheckRequest, httpClient *http.Client) *targetableProvider {
	return &targetableProvider{
		inner:         inner,
		repo:          repo,
		checksumAsset: checksumAsset,
		req:           req,
		baseURL:       defaultGitHubAPIBase,
		httpClient:    httpClient,
		matcher:       matchReleaseAsset,
	}
}

// SetTarget aims a subsequent Check at a specific release tag. An empty tag
// restores the default (latest) behavior.
func (p *targetableProvider) SetTarget(tag string) {
	if tag == "" {
		p.target.Store(nil)
		return
	}
	t := tag
	p.target.Store(&t)
}

func (p *targetableProvider) Name() string { return p.inner.Name() }

// Check resolves the targeted release: the stock latest path when no target is
// set, or a specific tag otherwise. The by-tag path deliberately skips the
// newer-than-current gate so a rollback resolves, and matches assets against
// this provider's configured target rather than the incoming request, so it
// resolves exactly what listReleases offered.
func (p *targetableProvider) Check(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	if t := p.target.Load(); t != nil && *t != "" {
		return p.resolveTag(ctx, *t, p.req)
	}
	return p.inner.Check(ctx, req)
}

// Download reuses the stock provider, which reads the asset URL that resolveTag
// (or the stock Check) stashed on Release.Metadata.
func (p *targetableProvider) Download(ctx context.Context, rel *updater.Release, dst io.Writer, onProgress func(written, total int64)) error {
	return p.inner.Download(ctx, rel, dst, onProgress)
}

// resolveTag fetches a single release by tag and builds the updater.Release the
// framework needs: the platform asset's download URL on Metadata and the
// SHASUMS256 digest on Verification. No IsNewer gate — the caller chose this
// version explicitly, including downgrades.
func (p *targetableProvider) resolveTag(ctx context.Context, tag string, req updater.CheckRequest) (*updater.Release, error) {
	if !validReleaseTag(tag) {
		return nil, fmt.Errorf("github: refusing to resolve unsafe release tag %q", tag)
	}
	var rel apiRelease
	endpoint := p.baseURL + "/repos/" + p.repo + "/releases/tags/" + url.PathEscape(tag)
	if err := p.getJSON(ctx, endpoint, &rel); err != nil {
		return nil, fmt.Errorf("github: fetch release %s: %w", tag, err)
	}

	idx := p.matcher(req, toReleaseAssets(rel.Assets))
	if idx < 0 || idx >= len(rel.Assets) {
		return nil, fmt.Errorf("github: release %s has no asset for %s/%s", tag, req.Platform, req.Arch)
	}
	picked := rel.Assets[idx]

	out := &updater.Release{
		Version: trimVPrefix(rel.TagName),
		Name:    rel.Name,
		Notes:   rel.Body,
		Artifact: updater.Artifact{
			Filename: picked.Name,
			Size:     picked.Size,
			Platform: req.Platform,
			Arch:     req.Arch,
		},
		// "github.asset.url" is the stock github.Provider.Download contract: it
		// reads the asset URL back off this key. Populating it is what lets the
		// by-tag path reuse the stock download (auth, redirect-strip, progress)
		// instead of reimplementing it — keep the key in sync with the provider.
		Metadata: map[string]any{"github.asset.url": picked.BrowserDownloadURL},
	}

	if p.checksumAsset != "" {
		digest, err := p.fetchChecksum(ctx, rel.Assets, p.checksumAsset, picked.Name)
		if err != nil {
			return nil, fmt.Errorf("github: load checksum for %s: %w", tag, err)
		}
		out.Verification = &updater.Verification{DigestAlgo: "sha256", Digest: digest}
	}
	return out, nil
}

// listReleases enumerates installable releases (those with an asset for the
// configured target and a checksum sidecar), newest first, annotated relative to
// the running version.
func (p *targetableProvider) listReleases(ctx context.Context) ([]ReleaseSummary, error) {
	var raw []apiRelease
	endpoint := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", p.baseURL, p.repo, releaseListPageSize)
	if err := p.getJSON(ctx, endpoint, &raw); err != nil {
		return nil, err
	}

	curV := ensureVPrefix(p.req.CurrentVersion)
	out := make([]ReleaseSummary, 0, len(raw))
	latestMarked := false
	for _, r := range raw {
		if r.Draft || !p.installable(p.req, r) {
			continue
		}
		s := ReleaseSummary{
			Tag:        r.TagName,
			Version:    trimVPrefix(r.TagName),
			Name:       r.Name,
			Prerelease: r.Prerelease,
		}
		if !r.PublishedAt.IsZero() {
			s.PublishedAt = r.PublishedAt.Format(time.RFC3339)
		}
		if relV := ensureVPrefix(r.TagName); semver.IsValid(relV) && semver.IsValid(curV) {
			switch semver.Compare(relV, curV) {
			case 0:
				s.IsCurrent = true
			case -1:
				s.IsOlder = true
			}
		}
		// GitHub returns releases newest-first; the first non-prerelease is the
		// "latest" the passive check (/releases/latest) would resolve.
		if !latestMarked && !r.Prerelease {
			s.IsLatest = true
			latestMarked = true
		}
		out = append(out, s)
	}
	return out, nil
}

// installable reports whether a release ships an asset for the requested target
// and a checksum sidecar — the two things a download here needs.
func (p *targetableProvider) installable(req updater.CheckRequest, r apiRelease) bool {
	idx := p.matcher(req, toReleaseAssets(r.Assets))
	if idx < 0 || idx >= len(r.Assets) {
		return false
	}
	for _, a := range r.Assets {
		if a.Name == p.checksumAsset {
			return true
		}
	}
	return false
}

// fetchChecksum downloads the named sidecar and extracts the sha256 digest for
// targetName.
func (p *targetableProvider) fetchChecksum(ctx context.Context, assets []apiAsset, sidecarName, targetName string) ([]byte, error) {
	sidecarURL := ""
	for _, a := range assets {
		if a.Name == sidecarName {
			sidecarURL = a.BrowserDownloadURL
			break
		}
	}
	if sidecarURL == "" {
		return nil, fmt.Errorf("release ships no %s asset", sidecarName)
	}
	body, err := p.getRaw(ctx, sidecarURL, maxChecksumBytes)
	if err != nil {
		return nil, err
	}
	return parseChecksumDigest(string(body), targetName)
}

func (p *targetableProvider) getJSON(ctx context.Context, endpoint string, dst any) error {
	resp, err := p.do(ctx, endpoint, "application/vnd.github+json")
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

func (p *targetableProvider) getRaw(ctx context.Context, endpoint string, limit int64) ([]byte, error) {
	resp, err := p.do(ctx, endpoint, "application/octet-stream")
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
func (p *targetableProvider) do(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	return p.httpClient.Do(req)
}

// parseChecksumDigest extracts the sha256 digest for target from sha256sum-style
// output (`<hex>  <name>`, with an optional `*`/`./` prefix on the name).
//
// Deliberately stricter than the stock github provider's parseChecksumLine: it
// enforces the 32-byte sha256 length below, but does NOT skip `#`-comment lines
// (our SHASUMS256 generator never emits them — a comment would fail hex-decode
// and surface as a load error, which is the safe direction).
func parseChecksumDigest(body, target string) ([]byte, error) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		name = strings.TrimPrefix(name, "./")
		if name != target {
			continue
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil {
			return nil, fmt.Errorf("invalid checksum hex for %s: %w", target, err)
		}
		if len(digest) != sha256.Size {
			return nil, fmt.Errorf("checksum for %s is %d bytes, want %d", target, len(digest), sha256.Size)
		}
		return digest, nil
	}
	return nil, fmt.Errorf("no checksum entry for %s", target)
}

func toReleaseAssets(assets []apiAsset) []github.ReleaseAsset {
	out := make([]github.ReleaseAsset, len(assets))
	for i, a := range assets {
		out[i] = github.ReleaseAsset{Name: a.Name, ContentType: a.ContentType, Size: a.Size, URL: a.BrowserDownloadURL}
	}
	return out
}

func trimVPrefix(s string) string { return strings.TrimPrefix(s, "v") }

func ensureVPrefix(s string) string {
	if strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}

// apiRelease / apiAsset mirror the subset of the GitHub releases API the picker
// needs. (The stock provider has equivalents but keeps them unexported.)
type apiRelease struct {
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	Prerelease  bool       `json:"prerelease"`
	Draft       bool       `json:"draft"`
	PublishedAt time.Time  `json:"published_at"`
	Assets      []apiAsset `json:"assets"`
}

type apiAsset struct {
	Name               string `json:"name"`
	ContentType        string `json:"content_type"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// ListReleases returns the installable releases for this build's update target,
// newest first, so the frontend can offer a version picker. Read-only; LocalOnly.
func (a *Service) ListReleases() ([]ReleaseSummary, error) {
	if a.updater.handle == nil || a.updater.provider == nil {
		return nil, ErrUpdatesUnsupported
	}
	ctx, cancel := context.WithTimeout(a.context(), updaterCheckTimeout)
	defer cancel()
	rels, err := a.updater.provider.listReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	return rels, nil
}
