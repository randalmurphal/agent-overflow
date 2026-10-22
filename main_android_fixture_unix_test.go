//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/testutil/mockexec"
)

// The bundle fixture builder stamps frontend/dist and restores it after its
// build. A copy runs in a temporary repository beside a fake go, so the
// real dist is never stamped: a build that finishes restores the metadata,
// and so does one stopped by a signal, after the compiler and what it
// started are gone.
func TestAndroidBundleFixtureRestoresDist(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required: the Android smoke runs this builder with it")
	}
	for _, signalled := range []bool{false, true} {
		name := "after a build"
		if signalled {
			name = "after a signal mid-build"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			for _, rel := range []string{
				"e2e/scripts/build-android-bundle-fixture.ts",
				"frontend/scripts/bundleId.ts",
				"frontend/src/lib/native/bundleVersion.ts",
			} {
				body, err := os.ReadFile(filepath.FromSlash(rel))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(rel)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, rel), body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dist := filepath.Join(repo, "frontend", "dist")
			originals := map[string]string{
				"bundle-release.json": `{"version":"1.4.0"}` + "\n",
				"bundle-id.txt":       "original-id\n",
				"index.html":          "<!doctype html>\n",
			}
			if err := os.MkdirAll(dist, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, body := range originals {
				if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(dir, "bin")
			mockexec.WriteIn(t, bin, "unzip", "#!/bin/sh\necho '{\"version\":\"1.3.0\"}'\n")
			// The fake compiler records its PID and a child's, as go build's
			// compile steps would be, then holds or finishes.
			mockexec.WriteIn(t, bin, "go", `#!/usr/bin/env bash
sleep 300 &
echo "$$ $!" >"$AO_TEST_GO_PIDS.tmp"
mv "$AO_TEST_GO_PIDS.tmp" "$AO_TEST_GO_PIDS"
[[ -n "$AO_TEST_GO_HOLD" ]] || { kill $!; exit 0; }
wait
`)
			pids := filepath.Join(dir, "go.pids")
			hold := ""
			if signalled {
				hold = "1"
			}
			cmd := exec.Command(node, filepath.Join(repo, "e2e/scripts/build-android-bundle-fixture.ts"), filepath.Join(dir, "app.apk"))
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"AO_TEST_GO_PIDS="+pids,
				"AO_TEST_GO_HOLD="+hold,
			)
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var goPID, childPID int
			deadline := time.Now().Add(30 * time.Second)
			for {
				if body, err := os.ReadFile(pids); err == nil {
					if _, err := fmt.Sscan(string(body), &goPID, &childPID); err != nil {
						t.Fatal(err)
					}
					break
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					t.Fatalf("the compiler never started:\n%s", out.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			if signalled {
				stamped, err := os.ReadFile(filepath.Join(dist, "bundle-release.json"))
				if err != nil || string(stamped) == originals["bundle-release.json"] {
					t.Fatalf("dist was not stamped while the compiler ran: %q, %v", stamped, err)
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("the builder did not finish:\n%s", out.String())
			}
			want := 0
			if signalled {
				want = 143
			}
			if code := cmd.ProcessState.ExitCode(); code != want {
				t.Fatalf("exit %d (%v), want %d:\n%s", code, err, want, out.String())
			}
			for name, body := range originals {
				got, err := os.ReadFile(filepath.Join(dist, name))
				if err != nil || string(got) != body {
					t.Fatalf("%s = %q, %v after the builder, want it restored to %q", name, got, err, body)
				}
			}
			// The compiler is the builder's child and already reaped; its
			// child is reparented, so give its reaper a moment.
			for _, pid := range []int{goPID, childPID} {
				gone := time.Now().Add(5 * time.Second)
				for syscall.Kill(pid, 0) == nil {
					if time.Now().After(gone) {
						_ = syscall.Kill(pid, syscall.SIGKILL)
						t.Fatalf("process %d from the build outlived the builder", pid)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
	}
}
