package appupdate

import (
	"net/http"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

// newGitHubProvider builds the stock provider. It exists so the matcher is
// attached in ONE place: the stock provider serves the passive latest Check
// and targetableProvider serves listing and by-tag installs, and a listing
// that disagreed with an install about which asset is "this platform's" would
// be worse than either being wrong alone. Tests that wire the production
// chain against a mock server call this too, so a fixture cannot quietly
// exercise a different matcher than the app ships.
func newGitHubProvider(repository, checksumAsset, baseURL string, client *http.Client) (*github.Provider, error) {
	config := github.Config{
		Repository:    repository,
		ChecksumAsset: checksumAsset,
		HTTPClient:    client,
		AssetMatcher:  matchReleaseAsset,
	}
	if baseURL != "" {
		config.BaseURL = baseURL
	}
	return github.New(config)
}

// releaseAssetPrefix is the product name every release asset carries.
// scripts/build-release.sh writes the names; TestReleaseAssetMatcherAgreesWithTheReleaseScript
// reads that script back and fails if the two ever disagree.
const releaseAssetPrefix = "agent-overflow-"

// matchReleaseAsset picks the release asset for one target, by EXACT name.
//
// It replaces github.DefaultAssetMatcher, which accepts any asset whose name
// merely CONTAINS the platform and arch tokens and returns the first such
// asset in the release's own order. That worked only while the three shipping
// assets happened to be disjoint under substring matching. The windowless
// serve binary, agent-overflow-headless-linux-amd64, contains "linux" and
// "amd64" too and sorts ahead of agent-overflow-linux-amd64 — so a Linux
// desktop install would have been offered it as its next update and swapped
// to a binary that opens no window, with nothing anywhere reporting a
// mismatch. A rule that holds only until the next artifact is not a rule.
//
// So: an asset belongs to a target exactly when its name is the product
// prefix, followed by "<platform>-<arch>", followed by one of the extensions
// this repo actually ships an artifact under. A qualifier anywhere else in the
// name makes it a DIFFERENT artifact, which is the whole point of putting one
// there, and any other extension is a sidecar (.sig, .asc, .sha256) rather
// than something to install.
//
// Platform is not GOOS: the WSL backend targets "wsl" and installs a Windows
// launcher binary (see targetableProvider).
func matchReleaseAsset(req updater.CheckRequest, assets []github.ReleaseAsset) int {
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	arch := strings.ToLower(strings.TrimSpace(req.Arch))
	if platform == "" || arch == "" {
		return -1
	}
	want := releaseAssetPrefix + platform + "-" + arch
	for i, asset := range assets {
		name := strings.ToLower(strings.TrimSpace(asset.Name))
		for _, ext := range releaseAssetExtensions {
			if name == want+ext {
				return i
			}
		}
	}
	return -1
}

// releaseAssetExtensions is every shape scripts/build-release.sh writes an
// installable artifact in: a bare ELF, the macOS app bundle's zip, and the
// Windows launcher. A new one is added HERE, deliberately — an artifact under
// an unlisted extension is refused rather than guessed at, and
// TestReleaseAssetMatcherAgreesWithTheReleaseScript turns that refusal into a
// build failure at the commit that adds it.
var releaseAssetExtensions = []string{"", ".zip", ".exe"}
