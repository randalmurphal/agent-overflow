package appupdate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

// releaseTargets is every target this repo builds an updatable artifact for.
// Platform is not GOOS: the WSL backend runs as a linux process and installs
// the Windows launcher.
var releaseTargets = []struct {
	platform string
	arch     string
	want     string
}{
	{"linux", "amd64", "agent-overflow-linux-amd64"},
	{"darwin", "arm64", "agent-overflow-darwin-arm64.zip"},
	{"wsl", "amd64", "agent-overflow-wsl-amd64.exe"},
}

func assetsNamed(names ...string) []github.ReleaseAsset {
	out := make([]github.ReleaseAsset, 0, len(names))
	for _, name := range names {
		out = append(out, github.ReleaseAsset{Name: name})
	}
	return out
}

func matchName(t *testing.T, platform, arch string, assets []github.ReleaseAsset) string {
	t.Helper()
	idx := matchReleaseAsset(updater.CheckRequest{Platform: platform, Arch: arch}, assets)
	if idx < 0 {
		return ""
	}
	if idx >= len(assets) {
		t.Fatalf("matchReleaseAsset returned index %d for %d assets", idx, len(assets))
	}
	return assets[idx].Name
}

// The whole release directory, in the order the packaging script's own
// alphabetical listing produces — which is the order that put the headless
// binary ahead of the desktop one and made a substring matcher dangerous.
func fullReleaseListing() []github.ReleaseAsset {
	return assetsNamed(
		"SHASUMS256",
		"agent-overflow-darwin-arm64.zip",
		"agent-overflow-headless-linux-amd64",
		"agent-overflow-linux-amd64",
		"agent-overflow-wsl-amd64.exe",
		"appicon.png",
		"install.sh",
	)
}

func TestReleaseAssetMatcherPicksTheExactArtifact(t *testing.T) {
	assets := fullReleaseListing()
	for _, target := range releaseTargets {
		if got := matchName(t, target.platform, target.arch, assets); got != target.want {
			t.Errorf("%s/%s picked %q, want %q", target.platform, target.arch, got, target.want)
		}
	}
}

// The headless binary is a serve-mode artifact an operator installs by hand.
// Nothing self-updates INTO it, and — the reason this matcher exists — a Linux
// desktop must never be offered it: the swap would succeed and the app would
// then open no window.
func TestReleaseAssetMatcherNeverPicksTheHeadlessBinary(t *testing.T) {
	assets := fullReleaseListing()
	for _, target := range releaseTargets {
		got := matchName(t, target.platform, target.arch, assets)
		if got == "agent-overflow-headless-linux-amd64" {
			t.Errorf("%s/%s picked the headless binary", target.platform, target.arch)
		}
	}
	// And it is not an update target in its own right.
	if got := matchName(t, "headless", "amd64", assets); got != "" {
		t.Errorf(`platform "headless" picked %q, want no asset`, got)
	}
}

func TestReleaseAssetMatcherRefusesEverythingElse(t *testing.T) {
	cases := []struct {
		name             string
		platform, arch   string
		assets           []github.ReleaseAsset
		wantNoMatchNamed string
	}{
		{name: "sidecars", platform: "linux", arch: "amd64",
			assets: assetsNamed("SHASUMS256", "install.sh", "appicon.png")},
		{name: "a signature over the right asset", platform: "linux", arch: "amd64",
			assets: assetsNamed("agent-overflow-linux-amd64.sig")},
		{name: "another product entirely", platform: "linux", arch: "amd64",
			assets: assetsNamed("some-other-tool-linux-amd64")},
		{name: "a qualifier in front", platform: "linux", arch: "amd64",
			assets: assetsNamed("agent-overflow-debug-linux-amd64")},
		{name: "a qualifier behind", platform: "linux", arch: "amd64",
			assets: assetsNamed("agent-overflow-linux-amd64-debug")},
		{name: "the wrong arch", platform: "linux", arch: "arm64",
			assets: assetsNamed("agent-overflow-linux-amd64")},
		{name: "no platform in the request", platform: "", arch: "amd64",
			assets: assetsNamed("agent-overflow-linux-amd64")},
		{name: "no arch in the request", platform: "linux", arch: "",
			assets: assetsNamed("agent-overflow-linux-amd64")},
		{name: "no assets at all", platform: "linux", arch: "amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchName(t, tc.platform, tc.arch, tc.assets); got != "" {
				t.Errorf("picked %q, want no asset", got)
			}
		})
	}
}

// A `.sig` next to the real asset used to be skipped by an explicit suffix
// rule. Equality makes that rule unnecessary, but the real asset must still be
// found when both are present.
func TestReleaseAssetMatcherPicksThroughSidecars(t *testing.T) {
	assets := assetsNamed(
		"agent-overflow-linux-amd64.sig",
		"agent-overflow-linux-amd64.sha256",
		"agent-overflow-linux-amd64",
	)
	if got := matchName(t, "linux", "amd64", assets); got != "agent-overflow-linux-amd64" {
		t.Errorf("picked %q, want the bare binary", got)
	}
}

var releaseArtifactPattern = regexp.MustCompile(`\$OUT_DIR/([A-Za-z0-9._-]+)`)

// The release script writes the artifact names; this matcher reads them. Two
// authored copies of one list agree only until the first edit, so the test
// reads the script and holds the matcher to what it actually produces:
//
//   - every target this repo ships resolves to exactly one artifact, and
//   - no artifact is claimed by two targets, which is the collision the
//     headless binary would have caused under substring matching.
//
// A NEW artifact whose name a target also matches fails here, at the commit
// that adds it, rather than in an install some weeks later.
func TestReleaseAssetMatcherAgreesWithTheReleaseScript(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "build-release.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}

	seen := map[string]bool{}
	var names []string
	for _, m := range releaseArtifactPattern.FindAllStringSubmatch(string(body), -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		names = append(names, m[1])
	}
	sort.Strings(names)
	if len(names) < len(releaseTargets) {
		t.Fatalf("found %d artifact names in %s (%v), want at least %d — has the script changed shape?",
			len(names), script, names, len(releaseTargets))
	}

	assets := assetsNamed(names...)
	claimedBy := map[string]string{}
	for _, target := range releaseTargets {
		got := matchName(t, target.platform, target.arch, assets)
		if got != target.want {
			t.Errorf("%s/%s picked %q out of %v, want %q",
				target.platform, target.arch, got, names, target.want)
			continue
		}
		if other, dup := claimedBy[got]; dup {
			t.Errorf("artifact %q is claimed by both %s and %s/%s",
				got, other, target.platform, target.arch)
			continue
		}
		claimedBy[got] = target.platform + "/" + target.arch
	}
}
