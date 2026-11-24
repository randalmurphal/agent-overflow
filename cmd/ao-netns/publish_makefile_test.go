//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/testutil/reporoot"
)

// publishFixture is a makefile that publishes out/tool through the
// Makefile's go-build-tool, with a fake go first on PATH that writes
// $CONTENT as the built tool, or exits $GO_STATUS. With $BARRIER set, mv
// waits until $PUBLISHERS publishers have all reached it, so their staging
// and renames overlap every time.
func publishFixture(t *testing.T) (dir string, run func(env ...string) ([]byte, error)) {
	t.Helper()
	dir = t.TempDir()
	fakeBin := filepath.Join(dir, "fakebin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGo := `#!/bin/sh
[ "${GO_STATUS:-0}" = 0 ] || exit "$GO_STATUS"
while [ $# -gt 0 ]; do
	if [ "$1" = -o ]; then out=$2; fi
	shift
done
printf '%s' "$CONTENT" > "$out"
`
	realMv, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	fakeMv := `#!/bin/sh
if [ -n "${BARRIER:-}" ]; then
	: > "$BARRIER/$$"
	i=0
	while [ "$(ls "$BARRIER" | wc -l)" -lt "$PUBLISHERS" ]; do
		i=$((i + 1))
		if [ "$i" -gt 1000 ]; then echo "mv: the other publishers never reached mv" >&2; exit 1; fi
		sleep 0.01
	done
fi
exec ` + realMv + ` "$@"
`
	for name, script := range map[string]string{"go": fakeGo, "mv": fakeMv} {
		mockexec.Write(t, filepath.Join(fakeBin, name), script)
	}
	// Read here, not included by path, so the test cache sees the file.
	macro, err := os.ReadFile(reporoot.Path(t, "build/go-build-tool.mk"))
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(macro) + "\npublish:\n\t@$(call go-build-tool,out/tool,./tool)\n"
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatal(err)
	}
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Fatal(err)
	}
	run = func(env ...string) ([]byte, error) {
		cmd := exec.Command(makeBin, "--no-print-directory", "-C", dir, "publish")
		var clean []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "MAKEFLAGS=") && !strings.HasPrefix(kv, "MFLAGS=") && !strings.HasPrefix(kv, "MAKELEVEL=") {
				clean = append(clean, kv)
			}
		}
		cmd.Env = append(clean, append([]string{"PATH=" + fakeBin + ":" + os.Getenv("PATH")}, env...)...)
		return cmd.CombinedOutput()
	}
	return dir, run
}

// staged lists what a publisher left in out/ besides the tool.
func staged(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		if entry.Name() != "tool" {
			left = append(left, entry.Name())
		}
	}
	return left
}

func TestGoBuildToolPublishesOnlyChangedBytes(t *testing.T) {
	dir, run := publishFixture(t)
	tool := filepath.Join(dir, "out", "tool")
	if out, err := run("CONTENT=one"); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	info, err := os.Stat(tool)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("published tool: %v, %v; want mode 0755", info, err)
	}
	if out, err := run("CONTENT=one"); err != nil {
		t.Fatalf("publish again: %v\n%s", err, out)
	}
	again, err := os.Stat(tool)
	if err != nil || !again.ModTime().Equal(info.ModTime()) {
		t.Fatalf("an unchanged tool was rewritten: %v -> %v", info.ModTime(), again.ModTime())
	}
	if out, err := run("GO_STATUS=3"); err == nil {
		t.Fatalf("a failed build published: %s", out)
	}
	if data, _ := os.ReadFile(tool); string(data) != "one" || len(staged(t, dir)) > 0 {
		t.Fatalf("after a failed build: tool %q, staging left %v", data, staged(t, dir))
	}
}

// TestConcurrentPublishersEachRenameAWholeFile: two publishers of
// different bytes that reach the rename together both succeed, the tool
// ends as one publisher's whole bytes, and no staging file stays behind.
func TestConcurrentPublishersEachRenameAWholeFile(t *testing.T) {
	dir, run := publishFixture(t)
	barrier := filepath.Join(dir, "barrier")
	if err := os.Mkdir(barrier, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := []string{"first", "second"}
	var wg sync.WaitGroup
	errs := make([]error, len(contents))
	outs := make([][]byte, len(contents))
	for i, content := range contents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = run("CONTENT="+content, "BARRIER="+barrier, fmt.Sprintf("PUBLISHERS=%d", len(contents)))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("publisher %d: %v\n%s", i, err, outs[i])
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(contents, string(data)) || len(staged(t, dir)) > 0 {
		t.Fatalf("tool is %q, want one publisher's bytes; staging left %v", data, staged(t, dir))
	}
}
