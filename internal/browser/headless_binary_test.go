package browser

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// Discovery is proved against a FAKE PATH on every platform: no browser is
// looked for on the machine running the suite, and none is ever downloaded.

// pathWith answers a lookPath over an explicit set of installed programs,
// the way exec.LookPath answers over the real one.
func pathWith(installed ...string) func(string) (string, error) {
	set := make(map[string]struct{}, len(installed))
	for _, name := range installed {
		set[name] = struct{}{}
	}
	return func(name string) (string, error) {
		if _, ok := set[name]; ok {
			if strings.Contains(name, "/") {
				return name, nil
			}
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestFindChromiumPrefersTheOverrideAndThenThePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		override  string
		goos      string
		installed []string
		want      string
	}{
		{
			name:      "an override wins over everything on PATH",
			override:  "/opt/chromium/chrome",
			goos:      "linux",
			installed: []string{"/opt/chromium/chrome", "chromium", "google-chrome"},
			want:      "/opt/chromium/chrome",
		},
		{
			name:      "surrounding whitespace is not part of the path",
			override:  "  /opt/chromium/chrome  ",
			goos:      "linux",
			installed: []string{"/opt/chromium/chrome"},
			want:      "/opt/chromium/chrome",
		},
		{
			name:      "chromium comes before chrome",
			goos:      "linux",
			installed: []string{"google-chrome-stable", "chromium", "chrome"},
			want:      "/usr/bin/chromium",
		},
		{
			name:      "the distro name is tried too",
			goos:      "linux",
			installed: []string{"chrome", "chromium-browser"},
			want:      "/usr/bin/chromium-browser",
		},
		{
			name:      "google-chrome before its -stable alias",
			goos:      "linux",
			installed: []string{"google-chrome-stable", "google-chrome"},
			want:      "/usr/bin/google-chrome",
		},
		{
			name:      "chrome alone is still a browser",
			goos:      "linux",
			installed: []string{"chrome"},
			want:      "/usr/bin/chrome",
		},
		{
			// A Mac install puts nothing on PATH, so the bundle
			// executables are the only way to find one.
			name:      "the macOS app bundle",
			goos:      "darwin",
			installed: []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
			want:      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		},
		{
			name:      "the macOS Chromium bundle",
			goos:      "darwin",
			installed: []string{"/Applications/Chromium.app/Contents/MacOS/Chromium"},
			want:      "/Applications/Chromium.app/Contents/MacOS/Chromium",
		},
		{
			// PATH still wins on a Mac: a browser somebody put there is a
			// deliberate install, and the bundles are the fallback.
			name:      "PATH before the bundles on darwin",
			goos:      "darwin",
			installed: []string{"chromium", "/Applications/Chromium.app/Contents/MacOS/Chromium"},
			want:      "/usr/bin/chromium",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := findChromium(tc.override, pathWith(tc.installed...), tc.goos)
			if err != nil || got != tc.want {
				t.Fatalf("findChromium(%q, %v) = %q, %v; want %q", tc.override, tc.installed, got, err, tc.want)
			}
		})
	}
}

// An override that cannot be run is the ANSWER, not a hint. Falling through
// to whatever is on PATH would hand the operator a different browser than
// the one they named, and nothing anywhere would say so.
func TestFindChromiumRefusesABadOverrideRatherThanFallingBack(t *testing.T) {
	_, err := findChromium("/opt/gone/chrome", pathWith("chromium", "google-chrome"), "linux")
	if err == nil {
		t.Fatal("a broken override silently fell back to a browser on PATH")
	}
	if !strings.Contains(err.Error(), chromiumSettingKey) {
		t.Fatalf("error %v does not name the setting that fixes it", err)
	}
	if !strings.Contains(err.Error(), "/opt/gone/chrome") {
		t.Fatalf("error %v does not name the path that failed", err)
	}
}

// The not-found sentence is what a serve boot logs verbatim, so it has to
// say both halves of the fix: name the setting, or install a browser.
func TestFindChromiumSaysHowToFixANotFound(t *testing.T) {
	_, err := findChromium("", pathWith("firefox", "safari"), "linux")
	if err == nil {
		t.Fatal("a machine with no Chromium reported one")
	}
	for _, want := range []string{"no Chromium found", chromiumSettingKey, "install chromium"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not carry %q", err, want)
		}
	}
	// The bundle paths belong to macOS. A Linux host that happens to have
	// that directory must not be told it has a browser.
	if _, err := findChromium("", pathWith(darwinChromiumBundles[0]), "linux"); err == nil {
		t.Fatal("a Linux host resolved a macOS app bundle")
	}
}

// Selection resolved a real file; Start re-checks it, because a serve host
// outlives the package upgrade that replaces its browser.
func TestValidateChromiumBinaryNamesTheSettingWhenTheBrowserIsGone(t *testing.T) {
	if err := validateChromiumBinary("/usr/bin/chromium", pathWith("/usr/bin/chromium")); err != nil {
		t.Fatalf("a present binary was refused: %v", err)
	}
	err := validateChromiumBinary("/usr/bin/chromium", pathWith())
	if err == nil {
		t.Fatal("a missing binary passed validation")
	}
	if !strings.Contains(err.Error(), chromiumSettingKey) || !strings.Contains(err.Error(), "/usr/bin/chromium") {
		t.Fatalf("error %v names neither the file nor the setting", err)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("error %v drops the lookup's own reason", err)
	}
}

// A failed launch's diagnosis is its LAST lines. Keeping the head would keep
// the startup chatter and drop the abort.
func TestTailOfKeepsTheEnd(t *testing.T) {
	if got := tailOf("short", 16); got != "short" {
		t.Fatalf("tailOf(short) = %q", got)
	}
	got := tailOf("noise-noise-noise-FATAL", 5)
	if got != "...FATAL" {
		t.Fatalf("tailOf = %q, want the tail and an ellipsis", got)
	}
}
