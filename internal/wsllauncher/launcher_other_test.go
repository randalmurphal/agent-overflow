//go:build !windows

package wsllauncher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestListDistros_NoWSLShortCircuits pins the !isWSL() branch: on
// macOS / native Linux the package's ListDistros returns (nil, nil)
// without ever shelling out. The picker UI's empty-list branch
// triggers install guidance, so a regression that started erroring on
// non-WSL hosts would change observable product behavior.
func TestListDistros_NoWSLShortCircuits(t *testing.T) {
	withFakeIsWSL(t, false)
	got, err := ListDistros(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty slice when !isWSL, got %d distros", len(got))
	}
}

// TestListDistros_WSLButWslExeMissing pins the second short-circuit:
// when the backend believes itself to be inside WSL but wsl.exe isn't
// reachable through interop, return (nil, nil) rather than an error.
// Settings hides the switcher in that case.
func TestListDistros_WSLButWslExeMissing(t *testing.T) {
	withFakeIsWSL(t, true)
	t.Setenv("PATH", "")

	got, err := ListDistros(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty slice when wsl.exe is missing, got %d distros", len(got))
	}
}

// TestListDistros_WSLWithFakeWslExe walks the full path: isWSL=true,
// wsl.exe resolvable, output parsed. PATH points at a tempdir that
// holds a shim emitting the standard one-distro UTF-16 LE fixture.
func TestListDistros_WSLWithFakeWslExe(t *testing.T) {
	withFakeIsWSL(t, true)

	dir := t.TempDir()
	// Write the fixture bytes (UTF-16 LE w/ BOM) into a sibling file
	// so the shim doesn't need any tool on PATH to emit them. The
	// shim itself just `cat`s the file via an absolute path.
	fixturePath := filepath.Join(dir, "wsl-output.bin")
	fixture := buildWSLListFixture()
	if err := os.WriteFile(fixturePath, fixture, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	shim := filepath.Join(dir, "wsl.exe")
	shimBody := "#!/bin/sh\nexec /bin/cat " + fixturePath + "\n"
	if err := os.WriteFile(shim, []byte(shimBody), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", dir)

	got, err := ListDistros(context.Background())
	if err != nil {
		t.Fatalf("ListDistros: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 distro from fake shim, got %d (%+v)", len(got), got)
	}
	if got[0].Name != "Ubuntu-24.04" {
		t.Errorf("Name = %q, want Ubuntu-24.04", got[0].Name)
	}
	if !got[0].Default {
		t.Errorf("Default = false, want true (the shim emits a leading '*')")
	}
	if got[0].Version != 2 {
		t.Errorf("Version = %d, want 2", got[0].Version)
	}
}

// buildWSLListFixture returns a minimal UTF-16 LE wsl.exe -l -v output
// with one default distro: "* Ubuntu-24.04   Running   2".
func buildWSLListFixture() []byte {
	const text = "  NAME              STATE     VERSION\r\n* Ubuntu-24.04      Running   2\r\n"
	out := make([]byte, 0, 2+2*len(text))
	out = append(out, 0xFF, 0xFE) // UTF-16 LE BOM
	for _, r := range text {
		// All runes here are ASCII so the high byte is zero.
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// withFakeIsWSL replaces the package-level isWSL seam for the lifetime
// of t and restores it on Cleanup. The seam exists only on the
// non-Windows build, hence this helper's build tag.
func withFakeIsWSL(t *testing.T, value bool) {
	t.Helper()
	prev := isWSL
	isWSL = func() bool { return value }
	t.Cleanup(func() { isWSL = prev })
}
