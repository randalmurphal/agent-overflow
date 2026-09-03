package transport

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestMethodsGen_InSync regenerates methods_gen.go into a tempfile and
// asserts the bytes match the committed file. A developer who adds an
// App method without running `go run ./internal/transport/methodgen`
// fails this test, and the failure message points to the fix.
//
// Skipped on Windows in CI because the methodgen tool reads the repo
// root and the relative-path math depends on POSIX-y filesystem layout.
// The CI matrix runs the test on Linux, which is sufficient.
func TestMethodsGen_InSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("methodgen integrity test runs on POSIX CI")
	}

	repoRoot := findRepoRoot(t)

	tempDir := t.TempDir()
	tempOut := filepath.Join(tempDir, "methods_gen.go")
	tempTSOut := filepath.Join(tempDir, "methodRoutes.ts")
	tempInputs := filepath.Join(tempDir, "inputs.txt")

	cmd := exec.Command("go", "run", "./internal/transport/methodgen",
		"-out", tempOut,
		"-ts-out", tempTSOut,
		"-root", repoRoot,
		"-inputs", tempInputs,
	)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run methodgen: %v", err)
	}

	observeGeneratorInputs(t, tempInputs)

	want, err := os.ReadFile(tempOut)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot, "internal/transport/methods_gen.go"))
	if err != nil {
		t.Fatalf("read committed: %v", err)
	}

	if !bytes.Equal(want, got) {
		t.Fatalf("methods_gen.go is out of sync with App methods.\n" +
			"Run `make methodgen` and commit the result.")
	}

	// The TS route mirror comes out of the SAME run, so it is checked by
	// the same gate. A separate command to regenerate it would be a
	// separate command to forget, and the failure would then land on a
	// developer who had no reason to think that file was stale.
	wantTS, err := os.ReadFile(tempTSOut)
	if err != nil {
		t.Fatalf("read TS tempfile: %v", err)
	}
	gotTS, err := os.ReadFile(filepath.Join(repoRoot, tsRouteMirrorPath))
	if err != nil {
		t.Fatalf("read committed TS mirror: %v", err)
	}
	if !bytes.Equal(wantTS, gotTS) {
		t.Fatalf("%s is out of sync with App methods.\n"+
			"Run `make methodgen` and commit the result.", tsRouteMirrorPath)
	}
}

// tsRouteMirrorPath is the generated client-side copy of the Route
// column, named once so the drift gate and the parse gate below cannot
// point at different files.
const tsRouteMirrorPath = "frontend/src/lib/transport/methodRoutes.ts"

// TestFrontendMethodRoutesMatchGeneratedTable pins the TS mirror against
// GeneratedMethods, in both directions, by ID and by route.
//
// The byte-diff above already fails on a stale mirror, but it fails by
// shelling out — which is exactly the cache-key hole the input manifest
// exists to close, and it closes it for internal/app, not for the TS
// file. This test READS the committed mirror in-process, so a hand-edit
// to it is in this package's cache key and cannot survive a cached PASS.
// It is also the test that states the contract in the terms the client
// uses: $Call.ByID(<id>) must find a route for every method it can
// reach, and must find no route for a method that is gone.
func TestFrontendMethodRoutesMatchGeneratedTable(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", tsRouteMirrorPath))
	if err != nil {
		t.Fatalf("read %s: %v", tsRouteMirrorPath, err)
	}
	declared := parseMethodRouteMirror(t, string(source))

	if len(declared) != len(GeneratedMethods) {
		t.Fatalf("%s declares %d routes, the generated table has %d methods",
			tsRouteMirrorPath, len(declared), len(GeneratedMethods))
	}
	for _, m := range GeneratedMethods {
		route, ok := declared[m.ID]
		if !ok {
			t.Errorf("%s (id %d) has no row in %s", m.Name, m.ID, tsRouteMirrorPath)
			continue
		}
		if route != string(m.Route) {
			t.Errorf("%s (id %d): mirror says %q, the generated table says %q",
				m.Name, m.ID, route, m.Route)
		}
		if !m.Route.Valid() {
			t.Errorf("%s carries route %q, which routes.go does not declare", m.Name, m.Route)
		}
	}
}

// parseMethodRouteMirror reads the `<id>: '<route>',` rows out of the
// generated module. Textual on purpose, the way the scope vocabulary
// gate reads scopes.ts: the literal is a flat map by construction, and
// parsing it with a real TS parser would be a second dependency for a
// shape the generator controls.
func parseMethodRouteMirror(t *testing.T, module string) map[uint32]string {
	t.Helper()
	const marker = "export const METHOD_ROUTES: Readonly<Record<number, MethodRoute>> = {"
	start := strings.Index(module, marker)
	if start < 0 {
		t.Fatalf("no METHOD_ROUTES literal in %s; the gate is reading the wrong shape", tsRouteMirrorPath)
	}
	rest := module[start+len(marker):]
	end := strings.Index(rest, "\n};")
	if end < 0 {
		t.Fatalf("METHOD_ROUTES literal never closes")
	}
	out := map[uint32]string{}
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		id, err := strconv.ParseUint(line[:colon], 10, 32)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(line[colon+1:])
		quoteEnd := strings.Index(value[1:], "'")
		if len(value) < 3 || value[0] != '\'' || quoteEnd < 0 {
			t.Fatalf("row %q does not carry a quoted route", line)
		}
		out[uint32(id)] = value[1 : 1+quoteEnd]
	}
	if len(out) == 0 {
		t.Fatalf("METHOD_ROUTES parsed to nothing; the gate is reading the wrong shape")
	}
	return out
}

// observeGeneratorInputs opens, and immediately closes, every path the
// generator read. Nothing is done with the contents; the OPEN is the
// point.
//
// `go test` keys its result cache on the files the test PROCESS opens, and
// everything this test actually inspects lives in package transport. The
// source that decides the answer — internal/app — is opened by a
// subprocess, which the cache cannot see. Without these reads the gate
// reports a cached PASS over an internal/app it never looked at, so a
// newly exported App method stays undeclared through a green `make
// go-test` (that is exactly how RedeemPairing and RenewSession slipped
// past a full gate run on 2026-08-30). Reading the manifest's files puts
// them in the cache key.
//
// Directories are in the manifest alongside their files, and both halves
// are load-bearing: Go hashes a file by content and a directory by its
// entry list, so files alone would still miss a method declared in a file
// that did not exist on the cached run.
func observeGeneratorInputs(t *testing.T, manifest string) {
	t.Helper()
	listing, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read generator input manifest: %v", err)
	}
	paths := strings.Split(strings.TrimSpace(string(listing)), "\n")
	if len(paths) < 5 {
		t.Fatalf("the generator named %d inputs; it reads at least the skip list, the scope vocabulary, "+
			"the route vocabulary, one receiver directory, and its files", len(paths))
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open generator input %s: %v", path, err)
		}
		_ = f.Close()
	}
}

// findRepoRoot walks up from the test binary's location until it
// finds go.mod. Tests run from internal/transport/, so we expect to
// find go.mod two levels up.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (no go.mod above test cwd)")
	return ""
}
