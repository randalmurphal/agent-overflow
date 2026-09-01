package appidentity

import "testing"

func TestNormalizeProfile(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"  ":        "",
		"soak":      ProfileSoak,
		" SOAK ":    ProfileSoak,
		"harness":   ProfileHarness,
		" HARNESS ": ProfileHarness,
		"perf":      ProfilePerf,
		" PERF ":    ProfilePerf,
	}
	for raw, want := range cases {
		got, err := NormalizeProfile(raw)
		if err != nil {
			t.Fatalf("NormalizeProfile(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("NormalizeProfile(%q) = %q, want %q", raw, got, want)
		}
	}
	if _, err := NormalizeProfile("prod"); err == nil {
		t.Fatal("NormalizeProfile accepted an unknown profile; a typo must not fall back to the default instance")
	}
}

func TestLauncherModeProfileWins(t *testing.T) {
	cases := []struct {
		build, profile, want string
	}{
		{"dev", "", ModeDev},
		{"prod", "", ModeProd},
		{"", "", ModeProd},
		{"dev", ProfileSoak, ModeSoak},
		{"prod", ProfileSoak, ModeSoak},
		{"dev", ProfileHarness, ModeHarness},
		{"prod", ProfileHarness, ModeHarness},
		{"dev", ProfilePerf, ModePerf},
		{"prod", ProfilePerf, ModePerf},
	}
	for _, tc := range cases {
		if got := LauncherMode(tc.build, tc.profile); got != tc.want {
			t.Errorf("LauncherMode(%q, %q) = %q, want %q", tc.build, tc.profile, got, tc.want)
		}
	}
}

// TestIsolatedIdentitiesAreDistinct is the collision gate: every
// per-instance name the launcher derives must differ across every
// modes, or an isolated instance reaches into the developer's instance
// (or into another profile's).
func TestIsolatedIdentitiesAreDistinct(t *testing.T) {
	modes := []string{ModeDev, ModeProd, ModeHarness, ModeSoak, ModePerf}
	seen := map[string]map[string]string{}
	record := func(axis, mode, value string) {
		if seen[axis] == nil {
			seen[axis] = map[string]string{}
		}
		for otherMode, other := range seen[axis] {
			if other == value {
				t.Errorf("%s: mode %q and %q share %q", axis, otherMode, mode, value)
			}
		}
		seen[axis][mode] = value
	}
	for _, mode := range modes {
		record("singleInstanceID", mode, SingleInstanceID("wsl", mode))
		record("webviewProfileDir", mode, WebviewProfileDir(mode))
		record("renderForensicsDir", mode, RenderForensicsDir(mode))
		record("browserProfilesDir", mode, BrowserProfilesDir(mode))
		record("appTitle", mode, AppTitle(mode))
	}
	// Log and window state deliberately DO share between dev and prod;
	// only the isolated profiles split off.
	if StateFileName("launcher.log", ModeDev) != StateFileName("launcher.log", ModeProd) {
		t.Error("dev and prod must share launcher.log")
	}
	for _, tc := range []struct{ base, mode, want string }{
		{"launcher.log", ModeSoak, "launcher-soak.log"},
		{"window.json", ModeSoak, "window-soak.json"},
		{"noext", ModeSoak, "noext-soak"},
		{"launcher.log", ModeHarness, "launcher-harness.log"},
		{"window.json", ModeHarness, "window-harness.json"},
		{"noext", ModeHarness, "noext-harness"},
		{"launcher.log", ModePerf, "launcher-perf.log"},
		{"window.json", ModePerf, "window-perf.json"},
		{"noext", ModePerf, "noext-perf"},
	} {
		if got := StateFileName(tc.base, tc.mode); got != tc.want {
			t.Errorf("StateFileName(%q, %q) = %q, want %q", tc.base, tc.mode, got, tc.want)
		}
	}
}

func TestDevToolsPortsDoNotCollide(t *testing.T) {
	if got := DevToolsPort(ModeProd); got != 0 {
		t.Errorf("prod CDP port = %d, want 0 (the protocol is unauthenticated)", got)
	}
	// All diagnostic instances can be up at once, and two WebView2s
	// asked for the same remote-debugging port leave whichever lost the
	// bind unattachable with no diagnostic.
	seen := map[int]string{}
	for _, mode := range []string{ModeDev, ModeHarness, ModeSoak, ModePerf} {
		port := DevToolsPort(mode)
		if port == 0 {
			t.Fatalf("%s CDP port = 0, want non-zero", mode)
		}
		if other, ok := seen[port]; ok {
			t.Fatalf("%s and %s share CDP port %d", other, mode, port)
		}
		seen[port] = mode
	}
}
