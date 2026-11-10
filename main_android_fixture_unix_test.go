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
// and so does one stopped by a signal, only once the compiler and every
// step it started are gone.
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
			// The fake compiler starts a compile step that, once stopped,
			// finishes only when the barrier exists, records both PIDs, then
			// holds or finishes. go itself dies at the first SIGTERM. The PIDs
			// are recorded only once the step can take that signal: stopped
			// before its handler is in place it would die and hold nothing.
			mockexec.WriteIn(t, bin, "go", `#!/usr/bin/env bash
bash -c 'trap "until [[ -e \"\$AO_TEST_GO_BARRIER\" ]]; do sleep 0.02; done; exit 0" TERM; : >"$AO_TEST_GO_PIDS.ready"; while :; do sleep 0.05; done' &
until [[ -e "$AO_TEST_GO_PIDS.ready" ]]; do sleep 0.02; done
echo "$$ $!" >"$AO_TEST_GO_PIDS.tmp"
mv "$AO_TEST_GO_PIDS.tmp" "$AO_TEST_GO_PIDS"
[[ -n "$AO_TEST_GO_HOLD" ]] || { kill $!; wait; exit 0; }
wait
`)
			pids := filepath.Join(dir, "go.pids")
			barrier := filepath.Join(dir, "barrier")
			hold := ""
			if signalled {
				hold = "1"
			} else if err := os.WriteFile(barrier, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(node, filepath.Join(repo, "e2e/scripts/build-android-bundle-fixture.ts"), filepath.Join(dir, "app.apk"))
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"AO_TEST_GO_PIDS="+pids,
				"AO_TEST_GO_BARRIER="+barrier,
				"AO_TEST_GO_HOLD="+hold,
			)
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			// A step that outlived the builder must not hold Wait on the pipe.
			cmd.WaitDelay = 2 * time.Second
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			finished := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = cmd.Wait()
				close(finished)
			}()
			var goPID, stepPID int
			// Whatever an assertion leaves running is stopped and joined: the
			// builder, then the compiler's group while the step still names it.
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-finished
				if stepPID > 0 {
					if group, err := syscall.Getpgid(stepPID); err == nil && group == goPID {
						_ = syscall.Kill(-goPID, syscall.SIGKILL)
					}
				}
			})
			waitUntil := func(what string, done func() bool) {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for !done() {
					if time.Now().After(deadline) {
						t.Fatalf("%s never happened:\n%s", what, out.String())
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			waitUntil("the compiler starting", func() bool {
				body, err := os.ReadFile(pids)
				if err != nil {
					return false
				}
				if _, err := fmt.Sscan(string(body), &goPID, &stepPID); err != nil {
					t.Fatal(err)
				}
				return true
			})
			alive := func(pid int) bool { return syscall.Kill(pid, 0) == nil }
			if signalled {
				stamped, err := os.ReadFile(filepath.Join(dist, "bundle-release.json"))
				if err != nil || string(stamped) == originals["bundle-release.json"] {
					t.Fatalf("dist was not stamped while the compiler ran: %q, %v", stamped, err)
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				waitUntil("go exiting", func() bool { return !alive(goPID) })
				// go is gone and its step is held: the builder must neither
				// restore nor exit yet.
				time.Sleep(500 * time.Millisecond)
				select {
				case <-finished:
					t.Fatalf("the builder exited while a compile step still ran:\n%s", out.String())
				default:
				}
				if body, err := os.ReadFile(filepath.Join(dist, "bundle-release.json")); err != nil || string(body) != string(stamped) {
					t.Fatalf("dist was restored while a compile step still ran: %q, %v", body, err)
				}
				if err := os.WriteFile(barrier, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Fatalf("the builder did not finish:\n%s", out.String())
			}
			want := 0
			if signalled {
				want = 143
			}
			if code := cmd.ProcessState.ExitCode(); code != want {
				t.Fatalf("exit %d (%v), want %d:\n%s", code, waitErr, want, out.String())
			}
			for name, body := range originals {
				got, err := os.ReadFile(filepath.Join(dist, name))
				if err != nil || string(got) != body {
					t.Fatalf("%s = %q, %v after the builder, want it restored to %q", name, got, err, body)
				}
			}
			// The step is reparented once go exits, so give its reaper a moment.
			for _, pid := range []int{goPID, stepPID} {
				gone := time.Now().Add(5 * time.Second)
				for alive(pid) {
					if time.Now().After(gone) {
						t.Fatalf("process %d from the build outlived the builder", pid)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
	}
}
