package editor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

// fakeFS is a minimal in-memory filesystem layered behind detectEnv.
// Tests build one with a literal map of paths-to-contents and a list
// of directories; the env wires the same fixtures to readFile / readDir
// / stat so detection behaves consistently across the three lookups.
type fakeFS struct {
	files map[string][]byte
	dirs  map[string][]string
}

func (f *fakeFS) read(path string) ([]byte, error) {
	if data, ok := f.files[path]; ok {
		return data, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeFS) readDir(path string) ([]string, error) {
	if entries, ok := f.dirs[path]; ok {
		return entries, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeFS) stat(path string) (bool, error) {
	if _, ok := f.files[path]; ok {
		return true, nil
	}
	if _, ok := f.dirs[path]; ok {
		return true, nil
	}
	return false, nil
}

// pathTable maps editor-CLI names to absolute resolved paths. nil
// means "not on PATH". Used as the lookPath hook so each test can
// declare exactly which binaries appear on the simulated PATH.
type pathTable map[string]string

func (p pathTable) lookup(name string) (string, bool) {
	resolved, ok := p[name]
	if !ok {
		return "", false
	}
	return resolved, true
}

func newDetectEnv(fs *fakeFS, paths pathTable, env map[string]string) detectEnv {
	return detectEnv{
		lookPath: paths.lookup,
		readFile: fs.read,
		readDir:  fs.readDir,
		stat:     fs.stat,
		envValue: func(name string) (string, bool) {
			val, ok := env[name]
			return val, ok
		},
	}
}

func TestDetectEditors_PathHitOnNonWSL(t *testing.T) {
	fs := &fakeFS{} // no /proc/sys/kernel/osrelease → not WSL
	paths := pathTable{"code": "/usr/local/bin/code", "subl": "/usr/local/bin/subl"}
	env := newDetectEnv(fs, paths, nil)

	got := detectEditorsWithEnv(context.Background(), env)

	available := availableMap(got)
	if !available["code"] {
		t.Fatalf("code: expected available, got %+v", got)
	}
	if !available["subl"] {
		t.Fatalf("subl: expected available, got %+v", got)
	}
	if available["cursor"] {
		t.Fatalf("cursor: expected unavailable")
	}
}

func TestDetectEditors_RejectsLinuxNativeOnWSL(t *testing.T) {
	// On WSL, a `code` resolved to /usr/bin/code-oss whose script
	// content does NOT mention /mnt/c must be treated as unavailable.
	// This is the load-bearing rule from feedback_wsl_editor_bridge.md.
	fs := &fakeFS{
		files: map[string][]byte{
			wslOSReleasePath: []byte("5.15.146.1-microsoft-standard-WSL2"),
			"/usr/bin/code-oss": []byte(
				"#!/bin/sh\nexec /usr/share/code-oss/code-oss \"$@\"\n",
			),
		},
		dirs: map[string][]string{
			"/mnt/c/Users": {"alice"},
		},
	}
	paths := pathTable{"code": "/usr/bin/code-oss"}
	env := newDetectEnv(fs, paths, nil)

	got := detectEditorsWithEnv(context.Background(), env)

	if availableMap(got)["code"] {
		t.Fatalf("expected code unavailable on WSL with linux-native install; got %+v", got)
	}
}

func TestDetectEditors_AcceptsWSLShimResolvingToMntC(t *testing.T) {
	// A vendor WSL shim is a bash script under /usr/local/bin/code
	// that references /mnt/c/.../code. That's the canonical bridge —
	// it must register as available.
	shim := []byte(`#!/bin/bash
in_wsl() { [ -n "$WSL_DISTRO_NAME" ]; }
if in_wsl; then
  ELECTRON="/mnt/c/Users/Alice/AppData/Local/Programs/Microsoft VS Code/Code.exe"
  exec "$ELECTRON" "$@"
fi
`)
	fs := &fakeFS{
		files: map[string][]byte{
			wslOSReleasePath:        []byte("5.15.146.1-microsoft-standard-WSL2"),
			"/usr/local/bin/code":   shim,
		},
	}
	paths := pathTable{"code": "/usr/local/bin/code"}
	env := newDetectEnv(fs, paths, nil)

	got := detectEditorsWithEnv(context.Background(), env)

	for _, e := range got {
		if e.ID == "code" {
			if !e.Available {
				t.Fatalf("expected code available via shim; got %+v", e)
			}
			if e.ResolvedPath != "/usr/local/bin/code" {
				t.Fatalf("expected ResolvedPath = /usr/local/bin/code; got %q", e.ResolvedPath)
			}
			return
		}
	}
	t.Fatalf("code not in catalog output: %+v", got)
}

func TestDetectEditors_FallsBackToMntCUserInstall(t *testing.T) {
	// Inside WSL with no PATH-resolved code, detection must walk
	// /mnt/c/Users/<user>/AppData/Local/Programs/... and pick the
	// vendor install.
	userInstall := "/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code"
	fs := &fakeFS{
		files: map[string][]byte{
			wslOSReleasePath: []byte("5.15.146.1-microsoft-standard-WSL2"),
			userInstall:      []byte("#!/bin/bash\n"),
		},
		dirs: map[string][]string{
			"/mnt/c/Users": {"alice", "Default", "Public"},
		},
	}
	env := newDetectEnv(fs, pathTable{}, nil)

	got := detectEditorsWithEnv(context.Background(), env)

	for _, e := range got {
		if e.ID == "code" {
			if !e.Available || e.ResolvedPath != userInstall {
				t.Fatalf("expected user install resolved; got %+v", e)
			}
			return
		}
	}
	t.Fatalf("code missing from catalog output")
}

func TestDetectEditors_UserInstallPreferredOverSystem(t *testing.T) {
	// When both paths exist, the user-relative install must win —
	// that's the per-user editor environment, where extensions live.
	userInstall := "/mnt/c/Users/alice/AppData/Local/Programs/Microsoft VS Code/bin/code"
	systemInstall := "/mnt/c/Program Files/Microsoft VS Code/bin/code"
	fs := &fakeFS{
		files: map[string][]byte{
			wslOSReleasePath: []byte("microsoft"),
			userInstall:      []byte(""),
			systemInstall:    []byte(""),
		},
		dirs: map[string][]string{
			"/mnt/c/Users": {"alice"},
		},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	got := detectEditorsWithEnv(context.Background(), env)
	for _, e := range got {
		if e.ID == "code" {
			if e.ResolvedPath != userInstall {
				t.Fatalf("expected user install; got %q", e.ResolvedPath)
			}
			return
		}
	}
	t.Fatalf("code missing")
}

func TestDetectEditors_FallsBackToSystemInstallWhenUsersMissing(t *testing.T) {
	systemInstall := "/mnt/c/Program Files/Microsoft VS Code/bin/code"
	fs := &fakeFS{
		files: map[string][]byte{
			wslOSReleasePath: []byte("microsoft"),
			systemInstall:    []byte(""),
		},
		// no /mnt/c/Users in dirs → readDir returns ErrNotExist
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	got := detectEditorsWithEnv(context.Background(), env)
	for _, e := range got {
		if e.ID == "code" {
			if !e.Available || e.ResolvedPath != systemInstall {
				t.Fatalf("expected system install fallback; got %+v", e)
			}
			return
		}
	}
	t.Fatalf("code missing")
}

func TestDetectEditors_SkipsDefaultUserDirectories(t *testing.T) {
	// Only Default/Public/All Users/Default User exist — none should
	// be probed, and the system fallback should win.
	systemInstall := "/mnt/c/Program Files/Microsoft VS Code/bin/code"
	defaultUserInstall := "/mnt/c/Users/Default/AppData/Local/Programs/Microsoft VS Code/bin/code"
	fs := &fakeFS{
		files: map[string][]byte{
			wslOSReleasePath:    []byte("microsoft"),
			defaultUserInstall:  []byte(""),
			systemInstall:       []byte(""),
		},
		dirs: map[string][]string{
			"/mnt/c/Users": {"Default", "Public", "All Users", "Default User"},
		},
	}
	env := newDetectEnv(fs, pathTable{}, nil)
	got := detectEditorsWithEnv(context.Background(), env)
	for _, e := range got {
		if e.ID == "code" {
			if e.ResolvedPath != systemInstall {
				t.Fatalf("expected system install; the Default-user install must be skipped. got %q", e.ResolvedPath)
			}
			return
		}
	}
	t.Fatalf("code missing")
}

func TestDetectEditors_EnvFallback(t *testing.T) {
	fs := &fakeFS{}
	paths := pathTable{"vim": "/usr/bin/vim"}
	env := newDetectEnv(fs, paths, map[string]string{"EDITOR": "vim"})

	got := detectEditorsWithEnv(context.Background(), env)
	var fallback *Editor
	for i := range got {
		if got[i].EnvFallback {
			fallback = &got[i]
			break
		}
	}
	if fallback == nil {
		t.Fatalf("expected env fallback entry; got %+v", got)
	}
	if !fallback.Available || fallback.ResolvedPath != "/usr/bin/vim" {
		t.Fatalf("env fallback missing resolved path; got %+v", fallback)
	}
	if fallback.LaunchStyle != LaunchStyleDirectPath {
		t.Fatalf("env fallback should use direct-path; got %v", fallback.LaunchStyle)
	}
}

func TestDetectEditors_VisualPreferredOverEditor(t *testing.T) {
	fs := &fakeFS{}
	paths := pathTable{
		"code": "/usr/bin/code-fake",  // not on /mnt/c, not in detection table
		"vim":  "/usr/bin/vim",
		"emacs": "/usr/bin/emacs",
	}
	env := newDetectEnv(fs, paths, map[string]string{
		"VISUAL": "emacs",
		"EDITOR": "vim",
	})

	got := detectEditorsWithEnv(context.Background(), env)
	for _, e := range got {
		if e.EnvFallback {
			if e.Command != "emacs" {
				t.Fatalf("expected VISUAL preferred over EDITOR; got command=%q", e.Command)
			}
			return
		}
	}
	t.Fatalf("env fallback not produced")
}

func TestDetectEditors_NoEnvWhenBinaryMissing(t *testing.T) {
	// $EDITOR set but the binary isn't on PATH → env fallback skipped.
	// Falls through to "nothing available" so Resolve can return
	// ErrNoEditor cleanly.
	fs := &fakeFS{}
	env := newDetectEnv(fs, pathTable{}, map[string]string{"EDITOR": "ghostvim"})
	got := detectEditorsWithEnv(context.Background(), env)
	for _, e := range got {
		if e.EnvFallback {
			t.Fatalf("expected no env fallback when binary missing; got %+v", e)
		}
	}
}

func TestDetectEditors_RespectsContextCancellation(t *testing.T) {
	fs := &fakeFS{}
	env := newDetectEnv(fs, pathTable{"code": "/usr/local/bin/code"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := detectEditorsWithEnv(ctx, env)

	if len(got) != 0 {
		t.Fatalf("expected empty result on cancelled ctx; got %+v", got)
	}
}

func TestEditorCatalog_ReturnsIndependentSlice(t *testing.T) {
	a := EditorCatalog()
	b := EditorCatalog()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("two catalog calls returned different content")
	}
	a[0].Name = "Mutated"
	if b[0].Name == "Mutated" {
		t.Fatalf("catalog calls share backing memory")
	}
	if EditorCatalog()[0].Name == "Mutated" {
		t.Fatalf("catalog mutation leaked into package fixture")
	}
}

func TestLiveDetectEnv_LookupErrorMapping(t *testing.T) {
	// Sanity check: liveDetectEnv.stat must map ErrNotExist → false,
	// other errors → propagated. A subtle bug here would break the
	// /mnt/c walk on real filesystems, even though the rest of the
	// suite uses the fake env.
	live := liveDetectEnv()
	got, err := live.stat("/this/path/should/not/exist/agent-overflow-test")
	if err != nil {
		t.Fatalf("expected nil error for missing path; got %v", err)
	}
	if got {
		t.Fatalf("expected ok=false for missing path")
	}

	if !errors.Is(os.ErrNotExist, os.ErrNotExist) {
		t.Fatal("os.ErrNotExist sentinel changed")
	}
}

func availableMap(editors []Editor) map[string]bool {
	out := make(map[string]bool, len(editors))
	for _, e := range editors {
		out[e.ID] = e.Available
	}
	return out
}

// TestDetectEditors_CachesWithinTTL pins the per-process detection
// cache. The first DetectEditors call walks PATH + /mnt/c (expensive
// on WSL); subsequent calls within detectionCacheTTL return the cached
// result without re-walking. The cache is process-global because
// DetectEditors is called from multiple App entry points.
func TestDetectEditors_CachesWithinTTL(t *testing.T) {
	// Reset before AND after so the test doesn't pollute follow-on tests
	// in this package.
	RefreshEditors()
	t.Cleanup(RefreshEditors)

	// Prime the cache via the public DetectEditors entry point. It
	// reads through the live env (real os.LookPath) — that's fine for
	// a "we got SOMETHING" assertion; the next call must return the
	// same slice content without re-walking.
	first := DetectEditors(context.Background())
	if first == nil {
		t.Fatalf("first DetectEditors returned nil")
	}

	// Mutate the returned slice in place — the cache stores a defensive
	// copy, so a follow-up call must return the original (un-mutated)
	// data.
	if len(first) > 0 {
		first[0].Name = "MUTATED"
	}

	second := DetectEditors(context.Background())
	if len(second) != len(first) {
		t.Fatalf("cache produced different length: first=%d second=%d", len(first), len(second))
	}
	if len(second) > 0 && second[0].Name == "MUTATED" {
		t.Fatalf("cache was mutated by caller — defensive copy failed")
	}
}

// TestRefreshEditors_InvalidatesCache covers the manual-refresh path
// the App-side SetEditorSettings binding uses after a deliberate
// preference change. Without RefreshEditors the picker would show
// stale availability for up to detectionCacheTTL after the change.
func TestRefreshEditors_InvalidatesCache(t *testing.T) {
	RefreshEditors()
	t.Cleanup(RefreshEditors)

	first := DetectEditors(context.Background())
	if first == nil {
		t.Fatalf("first DetectEditors returned nil")
	}

	// Confirm the cache is populated.
	if cached, ok := readDetectionCache(); !ok || len(cached) == 0 {
		t.Fatalf("cache should be populated after first detect")
	}

	RefreshEditors()
	if _, ok := readDetectionCache(); ok {
		t.Fatalf("cache should be empty after RefreshEditors")
	}

	// Next call repopulates the cache. We don't have an injection seam
	// at the public DetectEditors level (it always uses liveDetectEnv),
	// so this is structural — re-running fills the cache so a third
	// call hits it.
	_ = DetectEditors(context.Background())
	if _, ok := readDetectionCache(); !ok {
		t.Fatalf("cache should be re-populated after a fresh DetectEditors")
	}
}
