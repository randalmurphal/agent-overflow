package appupdate

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ReleaseSource is what a supervised serve host uses instead of the Wails
// updater: the same provider chain, no handle, no install. Everything here
// runs against the same mock GitHub server the provider tests use, so nothing
// reaches the network and no release the app does not publish is ever named.

// newHeadlessSource builds a source targeting the windowless serve artifact,
// which is the production case: the running binary is
// agent-overflow-headless-linux-amd64 and the release it is looking at also
// ships agent-overflow-linux-amd64.
func newHeadlessSource(t *testing.T, specs []relSpec, checksums func(string) string, current string) *ReleaseSource {
	t.Helper()
	srv := newMockGitHub(t, specs, checksums)
	source, err := NewReleaseSource(Config{
		CurrentVersion: current,
		Platform:       headlessPlatform,
		Arch:           headlessArch,
		Repository:     testRepo,
		ChecksumAsset:  "SHASUMS256",
		BaseURL:        srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewReleaseSource: %v", err)
	}
	return source
}

// A source with no artifact token would match no asset in any release and
// report "no updates" for what is really a configuration mistake. It is
// refused at construction so the caller learns which.
func TestNewReleaseSourceRefusesAHostWithNoArtifactToken(t *testing.T) {
	for _, tc := range []struct{ name, platform, arch string }{
		{"no platform", "", "amd64"},
		{"no arch", "linux", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReleaseSource(Config{Platform: tc.platform, Arch: tc.arch}); err == nil {
				t.Fatal("NewReleaseSource accepted a target it cannot name an asset for")
			}
		})
	}
}

func TestReleaseSourceListsOnlyInstallableReleases(t *testing.T) {
	source := newHeadlessSource(t, []relSpec{
		{tag: "v0.0.9", name: "Nine", withHeadless: true, withChecksum: true, published: time.Unix(900, 0).UTC()},
		{tag: "v0.0.8", name: "Eight (no sidecar)", withHeadless: true},
		{tag: "v0.0.7", name: "Seven (no asset for this host)", withBogus: true, withChecksum: true},
		{tag: "v0.0.6", name: "Six", withHeadless: true, withChecksum: true},
	}, sumsForHeadless, "0.0.6")

	releases, err := source.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("List returned %d releases (%+v), want the 2 that are installable here", len(releases), releases)
	}
	if releases[0].Tag != "v0.0.9" || !releases[0].IsLatest {
		t.Errorf("first row = %+v, want v0.0.9 marked latest", releases[0])
	}
	if releases[1].Tag != "v0.0.6" || !releases[1].IsCurrent {
		t.Errorf("second row = %+v, want v0.0.6 marked current", releases[1])
	}
}

// The passive check's whole answer: a newer stable release, or nil.
func TestReleaseSourceLatestIsTheNewsOrNothing(t *testing.T) {
	specs := []relSpec{
		{tag: "v1.0.0-rc1", name: "Candidate", prerelease: true, withHeadless: true, withChecksum: true},
		{tag: "v0.9.0", name: "Nine", withHeadless: true, withChecksum: true},
		{tag: "v0.8.0", name: "Eight", withHeadless: true, withChecksum: true},
	}

	behind := newHeadlessSource(t, specs, sumsForHeadless, "0.8.0")
	latest, err := behind.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest == nil {
		t.Fatal("Latest reported nothing for a host a version behind")
	}
	if latest.Tag != "v0.9.0" {
		t.Fatalf("Latest = %+v, want v0.9.0 (a prerelease is not the latest)", latest)
	}

	for _, current := range []string{"0.9.0", "1.5.0"} {
		source := newHeadlessSource(t, specs, sumsForHeadless, current)
		latest, err := source.Latest(context.Background())
		if err != nil {
			t.Fatalf("Latest at %s: %v", current, err)
		}
		if latest != nil {
			t.Errorf("Latest at %s = %+v, want nil (nothing to install)", current, latest)
		}
	}
}

// The happy path, end to end: the exact artifact this host installs, verified
// against the sidecar, named back to the caller so it can stage it.
func TestReleaseSourceFetchWritesVerifiedBytes(t *testing.T) {
	source := newHeadlessSource(t, []relSpec{
		{tag: "v0.9.0", name: "Nine", withHeadless: true, withChecksum: true},
	}, sumsForHeadless, "0.8.0")

	var got bytes.Buffer
	var lastWritten, lastTotal int64
	resolved, err := source.Fetch(context.Background(), "v0.9.0", &got, func(written, total int64) {
		lastWritten, lastTotal = written, total
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got.Bytes(), headlessAssetBytes) {
		t.Fatalf("Fetch wrote %q, want the headless artifact's bytes", got.String())
	}
	if resolved.Version != "0.9.0" || resolved.Tag != "v0.9.0" {
		t.Errorf("resolved = %+v, want tag v0.9.0 / version 0.9.0", resolved)
	}
	if resolved.AssetName != headlessAssetName {
		t.Errorf("AssetName = %q, want %q — the desktop artifact is in this release too",
			resolved.AssetName, headlessAssetName)
	}
	if resolved.Digest != headlessAssetDigestHex {
		t.Errorf("Digest = %q, want %q", resolved.Digest, headlessAssetDigestHex)
	}
	if lastWritten == 0 || lastTotal == 0 {
		t.Errorf("progress never reported (written=%d total=%d)", lastWritten, lastTotal)
	}
}

// The refusal that is the point of the whole chain: bytes that do not match
// the published checksum are an error, and the error names both digests so an
// operator can tell a corrupted transfer from a republished release.
func TestReleaseSourceFetchRefusesBytesTheChecksumDoesNotCover(t *testing.T) {
	source := newHeadlessSource(t, []relSpec{
		{tag: "v0.9.0", name: "Nine", withHeadless: true, withChecksum: true},
	}, sumsForHeadlessCorrupt, "0.8.0")

	var got bytes.Buffer
	resolved, err := source.Fetch(context.Background(), "v0.9.0", &got, nil)
	if err == nil {
		t.Fatalf("Fetch accepted unverified bytes and resolved %+v", resolved)
	}
	if !strings.Contains(err.Error(), headlessAssetDigestHex) || !strings.Contains(err.Error(), testValidDigestHex) {
		t.Errorf("the refusal names neither digest: %v", err)
	}
	if resolved != (Resolved{}) {
		t.Errorf("a refused Fetch still named %+v, which a caller could stage", resolved)
	}
}

// A release that ships no sidecar at all fails at the resolve, before a byte
// is requested. Fail-closed is the property; "nothing was downloaded" is how
// it is observed.
func TestReleaseSourceFetchRefusesAReleaseWithNothingToVerifyAgainst(t *testing.T) {
	source := newHeadlessSource(t, []relSpec{
		{tag: "v0.9.0", name: "Nine", withHeadless: true},
	}, sumsForHeadless, "0.8.0")

	var got bytes.Buffer
	if _, err := source.Fetch(context.Background(), "v0.9.0", &got, nil); err == nil {
		t.Fatal("Fetch installed a release with no checksum")
	}
	if got.Len() != 0 {
		t.Errorf("Fetch wrote %d bytes for a release it could not verify", got.Len())
	}
}

// An explicit pick is honored even when it goes backwards: that is what makes
// "roll back to the version that worked" reachable from a client.
func TestReleaseSourceFetchAllowsADowngrade(t *testing.T) {
	source := newHeadlessSource(t, []relSpec{
		{tag: "v0.9.0", name: "Nine", withHeadless: true, withChecksum: true},
		{tag: "v0.8.0", name: "Eight", withHeadless: true, withChecksum: true},
	}, sumsForHeadless, "0.9.0")

	var got bytes.Buffer
	resolved, err := source.Fetch(context.Background(), "v0.8.0", &got, nil)
	if err != nil {
		t.Fatalf("Fetch of an older tag: %v", err)
	}
	if resolved.Version != "0.8.0" {
		t.Fatalf("resolved %+v, want 0.8.0", resolved)
	}

	// And the provider is left as it was found: a resolve that left its target
	// set would make the next passive check report the rolled-back version as
	// the newest one.
	latest, err := source.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest after a downgrade fetch: %v", err)
	}
	if latest != nil {
		t.Errorf("Latest = %+v after fetching an older tag, want nil", latest)
	}
}

// A tag is interpolated into a URL path, so it is validated before anything
// asks the network about it — and the refusal is the typed one the app maps to
// a bad-argument answer rather than a failed update.
func TestReleaseSourceFetchRefusesAnUnsafeTag(t *testing.T) {
	source := newHeadlessSource(t, []relSpec{
		{tag: "v0.9.0", withHeadless: true, withChecksum: true},
	}, sumsForHeadless, "0.8.0")

	var got bytes.Buffer
	_, err := source.Fetch(context.Background(), "../../etc/passwd", &got, nil)
	if !errors.Is(err, ErrInvalidReleaseTag) {
		t.Fatalf("Fetch = %v, want ErrInvalidReleaseTag", err)
	}
	if got.Len() != 0 {
		t.Errorf("Fetch wrote %d bytes for a tag it refused", got.Len())
	}
}

// A tag that exists but ships nothing this host can install is refused with
// the reason, rather than resolving to the artifact of some other target.
func TestReleaseSourceFetchRefusesAReleaseWithNoAssetForThisHost(t *testing.T) {
	source := newHeadlessSource(t, []relSpec{
		{tag: "v0.9.0", withBogus: true, withChecksum: true},
	}, sumsForHeadless, "0.8.0")

	var got bytes.Buffer
	if _, err := source.Fetch(context.Background(), "v0.9.0", &got, nil); err == nil {
		t.Fatal("Fetch resolved a release with no asset for this host")
	}
}

// The collision that made matchReleaseAsset exact, asserted from both sides:
// a headless host takes only the headless artifact, and a linux desktop host
// looking at the same release takes only the desktop one.
func TestReleaseSourceTargetsExactlyItsOwnArtifact(t *testing.T) {
	specs := []relSpec{{tag: "v0.9.0", withHeadless: true, withChecksum: true}}

	headless := newHeadlessSource(t, specs, sumsForHeadless, "0.8.0")
	var headlessBytes bytes.Buffer
	resolved, err := headless.Fetch(context.Background(), "v0.9.0", &headlessBytes, nil)
	if err != nil {
		t.Fatalf("headless Fetch: %v", err)
	}
	if resolved.AssetName != headlessAssetName {
		t.Fatalf("a headless host resolved %q", resolved.AssetName)
	}

	srv := newMockGitHub(t, specs, sumsForHeadless)
	desktop, err := NewReleaseSource(Config{
		CurrentVersion: "0.8.0",
		Platform:       "linux",
		Arch:           "amd64",
		Repository:     testRepo,
		ChecksumAsset:  "SHASUMS256",
		BaseURL:        srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewReleaseSource: %v", err)
	}
	// The desktop artifact's sidecar entry is deliberately wrong here, so the
	// download must FAIL — and the failure has to name the desktop artifact,
	// which is the proof that a linux host never resolved the headless one.
	var desktopBytes bytes.Buffer
	if _, err := desktop.Fetch(context.Background(), "v0.9.0", &desktopBytes, nil); err == nil {
		t.Fatal("the desktop source accepted an artifact its checksum does not cover")
	}
	if !bytes.Equal(desktopBytes.Bytes(), plainLinuxAssetBytes) {
		t.Fatalf("a linux desktop host downloaded %q, want the desktop artifact", desktopBytes.String())
	}
}
