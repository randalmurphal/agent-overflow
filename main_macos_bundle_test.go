package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise real macOS signature validation, not just whether the new bytes
// reached disk. A live process needs its old signed bundle after replacement.
func TestMacOSBundleReplacementPreservesRunningCode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS code-signing lifecycle")
	}
	root := t.TempDir()
	source := filepath.Join(root, "probe.c")
	if err := os.WriteFile(source, []byte(`#include <Security/Security.h>
#include <stdio.h>
int main(void) {
 setbuf(stdout, NULL);
 do {
  SecCodeRef code = NULL;
  OSStatus status = SecCodeCopySelf(kSecCSDefaultFlags, &code);
  if (!status) status = SecCodeCheckValidity(code, kSecCSDefaultFlags, NULL);
  if (code) CFRelease(code);
  printf("%s %d\n", LABEL, (int)status);
 } while (getchar() != EOF);
}
`), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, cmd *exec.Cmd) {
		t.Helper()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", cmd.Args, err, output)
		}
	}
	binary := func(label string) string {
		t.Helper()
		path := filepath.Join(root, label)
		run(t, exec.Command("clang", source, `-DLABEL="`+label+`"`, "-framework", "Security", "-framework", "CoreFoundation", "-o", path))
		return path
	}
	old, newer := binary("old"), binary("new")
	packageApp := func(t *testing.T, binary, destination string) {
		t.Helper()
		run(t, exec.Command("sh", "scripts/package-macos-app.sh", binary, "build/darwin/Info.plist", destination))
	}
	for _, mode := range []string{"build", "install-app", "install-zip"} {
		t.Run(mode, func(t *testing.T) {
			home := filepath.Join(root, mode)
			dest := filepath.Join(home, "Applications", "Agent Overflow.app")
			packageApp(t, old, dest)
			process := exec.Command(filepath.Join(dest, "Contents", "MacOS", "agent-overflow"))
			stdin, err := process.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := process.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				stdin.Close()
				if process.ProcessState == nil {
					process.Process.Kill()
					process.Wait()
				}
			}()
			reader := bufio.NewReader(stdout)
			valid := func() {
				t.Helper()
				line, err := reader.ReadString('\n')
				if err != nil || line != "old 0\n" {
					t.Fatalf("running original lost signature validity: %q (%v)", line, err)
				}
			}
			valid()
			update := func() { packageApp(t, newer, dest) }
			if mode != "build" {
				artifact := filepath.Join(home, "candidate", "New.app")
				packageApp(t, newer, artifact)
				if mode == "install-zip" {
					zipPath := filepath.Join(home, "candidate.zip")
					zip := exec.Command("zip", "-qry", zipPath, "New.app")
					zip.Dir = filepath.Dir(artifact)
					run(t, zip)
					artifact = zipPath
				}
				update = func() {
					cmd := exec.Command("sh", "scripts/install.sh", "--macos", artifact)
					cmd.Env = append(os.Environ(), "HOME="+home)
					run(t, cmd)
				}
			}
			// A second replacement must not prune a still-running earlier build.
			for range 2 {
				update()
				if _, err := fmt.Fprint(stdin, "x"); err != nil {
					t.Fatal(err)
				}
				valid()
			}
			output, err := exec.Command(filepath.Join(dest, "Contents", "MacOS", "agent-overflow")).CombinedOutput()
			if err != nil || string(output) != "new 0\n" {
				t.Fatalf("replacement must launch as valid new code: %q (%v)", output, err)
			}
			retired := func() []string {
				paths, err := filepath.Glob(filepath.Join(filepath.Dir(dest), ".Agent Overflow.app.previous.*"))
				if err != nil {
					t.Fatal(err)
				}
				return paths
			}
			if got := retired(); len(got) != 1 {
				t.Fatalf("keep only the running bundle: %v", got)
			}

			// Failed preparation must not disturb the published or running app.
			failed := exec.Command("sh", "scripts/package-macos-app.sh", newer, filepath.Join(root, "missing.plist"), dest)
			if err := failed.Run(); err == nil {
				t.Fatal("missing bundle input succeeded")
			}
			if _, err := fmt.Fprint(stdin, "x"); err != nil {
				t.Fatal(err)
			}
			valid()
			// Fail the final publish after the original has been moved aside.
			// The same publisher handles builds and both installer formats.
			if mode == "build" {
				tools := filepath.Join(home, "tools")
				if err := os.MkdirAll(tools, 0700); err != nil {
					t.Fatal(err)
				}
				wrapper := "#!/bin/sh\ncase \"$1\" in */.ao-bundle.*/*) exit 17;; esac\nexec /bin/mv \"$@\"\n"
				if err := os.WriteFile(filepath.Join(tools, "mv"), []byte(wrapper), 0700); err != nil {
					t.Fatal(err)
				}
				failed = exec.Command("sh", "scripts/package-macos-app.sh", newer, "build/darwin/Info.plist", dest)
				failed.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))
				if output, err := failed.CombinedOutput(); err == nil {
					t.Fatalf("publish failure did not propagate: %s", output)
				}
				output, err := exec.Command(filepath.Join(dest, "Contents", "MacOS", "agent-overflow")).CombinedOutput()
				if err != nil || string(output) != "new 0\n" {
					t.Fatalf("failed replacement lost the installed app: %q (%v)", output, err)
				}
				if _, err := fmt.Fprint(stdin, "x"); err != nil {
					t.Fatal(err)
				}
				valid()
			}
			stdin.Close()
			if err := process.Wait(); err != nil {
				t.Fatal(err)
			}
			update()
			if got := retired(); len(got) != 0 {
				t.Fatalf("exited versions were not reclaimed: %v", got)
			}
		})
	}
}

func TestDarwinBundleTasksUseSharedPackager(t *testing.T) {
	body, err := os.ReadFile("build/darwin/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, plist := range []string{"Info.plist", "Info.dev.plist"} {
		if !strings.Contains(string(body), `sh scripts/package-macos-app.sh "{{.BIN_DIR}}/{{.APP_NAME}}" build/darwin/`+plist) {
			t.Errorf("%s bypasses the running-bundle preservation policy", plist)
		}
	}
}
