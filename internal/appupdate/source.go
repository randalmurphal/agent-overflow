package appupdate

// The release chain with no Wails handle.
//
// Everything else in this package hangs off *updater.Updater: Configure hands
// it the providers, and CheckForUpdate / DownloadUpdate / RestartToUpdate drive
// it. A supervised `serve` host has no such handle — it has no
// application.App to build one against, and it would not want the framework's
// install half anyway, because a serve host is updated by its supervisor
// rather than by swapping its own file (docs/architecture/serve-mode.md).
//
// What it DOES need is the half above the install: enumerate releases, resolve
// one, and download it verified. ReleaseSource is that half, built from the
// same two provider types Configure builds — targetableProvider for the listing
// and the by-tag resolve, verifiedProvider for the fail-closed refusal of a
// release that ships nothing to verify against. Configure is rebuilt on top of
// the same constructor, so there is ONE chain: a listing on a serve host and a
// listing on the desktop cannot disagree about which asset is this host's.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// ReleaseSource resolves and downloads releases for one target, with no
// updater handle and no install step.
//
// Must be used through a pointer: the target pointer on the shared
// targetableProvider is mutated for the duration of a resolve, and mu is what
// keeps two concurrent Fetches from reading each other's target.
type ReleaseSource struct {
	targetable *targetableProvider
	// verified wraps targetable, so a release with no checksum material is
	// refused at the provider boundary rather than downloaded and trusted.
	verified verifiedProvider
	// req is this host's target: the platform/arch tokens release assets are
	// named for, plus the running version. One value, read by the listing and
	// by every resolve.
	req updater.CheckRequest
	// mu serializes the SetTarget / Check pair. The DOWNLOAD runs outside it:
	// it reads the resolved release and touches no shared state, and holding a
	// lock across a multi-minute transfer would make a second caller wait for
	// bytes rather than for an answer.
	mu sync.Mutex
}

// Resolved names what a Fetch actually downloaded, so the caller can stage it
// under a version and report a digest it did not have to recompute.
type Resolved struct {
	// Tag is the release tag as published, e.g. "v0.0.7".
	Tag string
	// Version is that tag without the leading "v".
	Version string
	// AssetName is the artifact file name the digest belongs to.
	AssetName string
	// Digest is the hex sha256 the downloaded bytes were verified against.
	Digest string
}

// applyDefaults fills the fields every caller would otherwise repeat. It is on
// Config so the one constructor below is the only place that has to remember
// them, and neither Configure nor NewReleaseSource can be configured against a
// different repository or sidecar than the other.
func (c *Config) applyDefaults() {
	if c.Repository == "" {
		c.Repository = updaterRepository
	}
	if c.ChecksumAsset == "" {
		c.ChecksumAsset = updaterChecksumAsset
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
}

// NewReleaseSource builds the provider chain for one target.
//
// An empty Platform is refused rather than defaulted. Platform is the artifact
// token matchReleaseAsset builds a file name from, and it is NOT runtime.GOOS:
// the WSL backend is a linux process that installs a Windows launcher binary,
// and a headless serve host installs `headless-<GOOS>`. A source built with no
// token would match no asset in any release and report every release
// uninstallable, which reads as "there are no updates" rather than as the
// configuration mistake it is.
func NewReleaseSource(config Config) (*ReleaseSource, error) {
	config.applyDefaults()
	if strings.TrimSpace(config.Platform) == "" {
		return nil, errors.New("updater: a release source needs the artifact platform token this host installs")
	}
	if strings.TrimSpace(config.Arch) == "" {
		return nil, errors.New("updater: a release source needs the artifact arch token this host installs")
	}

	provider, err := newGitHubProvider(config.Repository, config.ChecksumAsset, config.BaseURL, config.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("github provider: %w", err)
	}
	req := updater.CheckRequest{
		CurrentVersion: config.CurrentVersion,
		Platform:       config.Platform,
		Arch:           config.Arch,
	}
	targetable := newTargetableProvider(provider, config.Repository, config.ChecksumAsset, req, config.HTTPClient)
	if config.BaseURL != "" {
		targetable.baseURL = config.BaseURL
	}
	return &ReleaseSource{
		targetable: targetable,
		verified:   verifiedProvider{inner: targetable},
		req:        req,
	}, nil
}

// List enumerates the installable releases for this target, newest first,
// annotated relative to the running version. Metadata only: nothing is
// downloaded.
func (s *ReleaseSource) List(ctx context.Context) ([]ReleaseSummary, error) {
	releases, err := s.targetable.listReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	return releases, nil
}

// Latest is the newest stable installable release, or nil when this host is
// already on it or ahead of it.
//
// It reads the LISTING rather than /releases/latest, because the listing is
// the only answer that carries a tag and is already filtered to what this host
// can actually install: a "latest" that ships no asset for this platform is
// not an update, it is a release for somebody else. The newest non-prerelease
// row is the one /releases/latest would name; the annotations the listing
// already computed decide whether it is news.
//
// A running version semver cannot compare (an unstamped build) leaves both
// annotations false, so the newest release is reported. That is the honest
// answer: an unstamped build has no claim to being current.
func (s *ReleaseSource) Latest(ctx context.Context) (*ReleaseSummary, error) {
	releases, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range releases {
		if !releases[i].IsLatest {
			continue
		}
		if releases[i].IsCurrent || releases[i].IsOlder {
			return nil, nil
		}
		found := releases[i]
		return &found, nil
	}
	return nil, nil
}

// Fetch resolves one tag and writes its verified bytes to dst.
//
// Verification is the whole contract, and it is fail-closed twice over. The
// resolve goes through verifiedProvider, which refuses a release that ships no
// checksum material before a byte is requested. The bytes are then hashed as
// they stream and compared against that digest, and a mismatch is an error —
// so a caller that staged whatever landed in dst on a nil error cannot exist.
// The caller still owns dst: on any error the partial write is its to discard,
// which is why the app's flow downloads into a temp file it removes on every
// exit path.
//
// The tag is resolved with no newer-than-current gate, exactly as
// DownloadUpdate's by-tag branch does: the caller picked this version
// explicitly, including a downgrade back to one that worked.
func (s *ReleaseSource) Fetch(
	ctx context.Context, tag string, dst io.Writer, onProgress func(written, total int64),
) (Resolved, error) {
	if !validReleaseTag(tag) {
		return Resolved{}, fmt.Errorf("%w: %q", ErrInvalidReleaseTag, tag)
	}
	release, err := s.resolve(ctx, tag)
	if err != nil {
		return Resolved{}, err
	}

	hasher := sha256.New()
	if err := s.verified.Download(ctx, release, io.MultiWriter(dst, hasher), onProgress); err != nil {
		return Resolved{}, fmt.Errorf("download %s: %w", release.Artifact.Filename, err)
	}
	want := release.Verification.Digest
	if got := hasher.Sum(nil); !bytes.Equal(got, want) {
		return Resolved{}, fmt.Errorf(
			"the downloaded %s is sha256 %s and the published checksum is %s, so it was not installed",
			release.Artifact.Filename, hex.EncodeToString(got), hex.EncodeToString(want))
	}
	return Resolved{
		Tag:       ensureVPrefix(release.Version),
		Version:   release.Version,
		AssetName: release.Artifact.Filename,
		Digest:    hex.EncodeToString(want),
	}, nil
}

// resolve aims the shared provider at one tag and reads the answer back under
// the same lock, so a second Fetch cannot retarget it between the two.
func (s *ReleaseSource) resolve(ctx context.Context, tag string) (*updater.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetable.SetTarget(tag)
	// Restored so the provider is left as it was found. It matters on the
	// desktop, where Configure hands this same provider to the Updater and a
	// left-over target would make the next passive check report the version
	// somebody rolled back to as "latest".
	defer s.targetable.SetTarget("")

	release, err := s.verified.Check(ctx, s.req)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", tag, err)
	}
	if release == nil {
		return nil, fmt.Errorf("release %s is not installable on this host", tag)
	}
	return release, nil
}
