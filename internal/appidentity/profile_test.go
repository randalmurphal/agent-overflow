package appidentity

import "testing"

func TestNormalizeProfile(t *testing.T) {
	for _, raw := range []string{"", "  ", "soak", " SOAK "} {
		got, err := NormalizeProfile(raw)
		if err != nil {
			t.Fatalf("NormalizeProfile(%q): %v", raw, err)
		}
		want := ""
		if got != "" {
			want = ProfileSoak
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
	}
	for _, tc := range cases {
		if got := LauncherMode(tc.build, tc.profile); got != tc.want {
			t.Errorf("LauncherMode(%q, %q) = %q, want %q", tc.build, tc.profile, got, tc.want)
		}
	}
}

// TestSoakIdentitiesAreDistinct is the collision gate: every per-instance
// name the launcher derives must differ between the soak profile and both
// ordinary modes, or a soak run reaches into the developer's instance.
func TestSoakIdentitiesAreDistinct(t *testing.T) {
	modes := []string{ModeDev, ModeProd, ModeSoak}
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
	}
	// Log and window state deliberately DO share between dev and prod;
	// only soak splits off.
	if StateFileName("launcher.log", ModeDev) != StateFileName("launcher.log", ModeProd) {
		t.Error("dev and prod must share launcher.log")
	}
	if got := StateFileName("launcher.log", ModeSoak); got != "launcher-soak.log" {
		t.Errorf("soak log = %q, want launcher-soak.log", got)
	}
	if got := StateFileName("window.json", ModeSoak); got != "window-soak.json" {
		t.Errorf("soak window state = %q, want window-soak.json", got)
	}
	if got := StateFileName("noext", ModeSoak); got != "noext-soak" {
		t.Errorf("extensionless soak name = %q, want noext-soak", got)
	}
	if AppTitle(ModeSoak) == AppTitle(ModeDev) || AppTitle(ModeSoak) == AppTitle(ModeProd) {
		t.Error("the soak window title must be distinguishable from dev and prod")
	}
}

func TestDevToolsPortsDoNotCollide(t *testing.T) {
	if got := DevToolsPort(ModeProd); got != 0 {
		t.Errorf("prod CDP port = %d, want 0 (the protocol is unauthenticated)", got)
	}
	dev, soak := DevToolsPort(ModeDev), DevToolsPort(ModeSoak)
	if dev == 0 || soak == 0 {
		t.Fatalf("dev/soak CDP ports = %d/%d, want both non-zero", dev, soak)
	}
	if dev == soak {
		t.Fatalf("dev and soak share CDP port %d; both instances can be up at once", dev)
	}
}
