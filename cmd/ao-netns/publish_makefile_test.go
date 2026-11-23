//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// publishFixture is a makefile that publishes out/tool through the
// Makefile's go-build-tool, with a fake go first on PATH that writes
// $CONTENT as the built tool ($SIZE copies of that one character when
// set), or exits $GO_STATUS.
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
if [ -n "${SIZE:-}" ]; then head -c "$SIZE" /dev/zero | tr '\0' "$CONTENT" > "$out"; else printf '%s' "$CONTENT" > "$out"; fi
`
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
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

// TestConcurrentPublishersEachRenameAWholeFile: makes publishing different
// bytes at once all succeed, the tool ends as one publisher's whole bytes,
// and no staging file stays behind.
func TestConcurrentPublishersEachRenameAWholeFile(t *testing.T) {
	dir, run := publishFixture(t)
	const publishers = 4
	contents := make([]string, publishers)
	var wg sync.WaitGroup
	errs := make([]error, publishers)
	outs := make([][]byte, publishers)
	for i := range publishers {
		char := string(rune('a' + i))
		contents[i] = strings.Repeat(char, 4<<20)
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = run("CONTENT="+char, fmt.Sprintf("SIZE=%d", 4<<20))
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
	whole := false
	for _, content := range contents {
		whole = whole || bytes.Equal(data, []byte(content))
	}
	if !whole || len(staged(t, dir)) > 0 {
		t.Fatalf("tool is one publisher's bytes: %v; staging left %v", whole, staged(t, dir))
	}
}
