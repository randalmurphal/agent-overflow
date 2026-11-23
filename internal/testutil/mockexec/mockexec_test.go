//go:build !windows

package mockexec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRunsTheScriptWithItsShebangArgsAndExitCode(t *testing.T) {
	t.Parallel()
	path := WriteIn(t, t.TempDir(), "tool", "#!/bin/bash\necho \"$BASH_VERSION\" \"$@\"\nexit 3\n")

	out, err := exec.Command(path, "a b", "c").Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("exit = %v, want code 3", err)
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), " ", 2)
	if fields[0] == "" || len(fields) != 2 || fields[1] != "a b c" {
		t.Fatalf("output = %q, want a bash version followed by the args", out)
	}
}

func TestWriteWithoutShebangUsesSh(t *testing.T) {
	t.Parallel()
	path := WriteIn(t, t.TempDir(), "tool", "echo plain\n")
	out, err := exec.Command(path).Output()
	if err != nil || string(out) != "plain\n" {
		t.Fatalf("output = %q, %v", out, err)
	}
}

func TestWriteReplacesWithoutTouchingTheSharedWrapper(t *testing.T) {
	t.Parallel()
	wrapper := wrapperPath(t)
	before, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tool")
	Write(t, path, "#!/bin/sh\necho one\n")
	Write(t, path, "#!/bin/sh\necho two\n")
	out, err := exec.Command(path).Output()
	if err != nil || string(out) != "two\n" {
		t.Fatalf("output = %q, %v", out, err)
	}
	after, err := os.ReadFile(wrapper)
	if err != nil || string(after) != string(before) {
		t.Fatalf("shared wrapper changed: %v", err)
	}
}

// WriteFile over a path Write linked to the shared wrapper replaces the
// link, and Write over WriteFile's file links it again: neither changes the
// wrapper that every other mock runs.
func TestWriteFileReplacesAWriteLinkWithoutTouchingTheSharedWrapper(t *testing.T) {
	t.Parallel()
	wrapper := wrapperPath(t)
	before, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tool")
	Write(t, path, "#!/bin/sh\necho linked\n")
	WriteFile(t, path, []byte("#!/bin/sh\necho own\n"), 0o755)
	if out, err := exec.Command(path).Output(); err != nil || string(out) != "own\n" {
		t.Fatalf("after WriteFile: output = %q, %v", out, err)
	}
	Write(t, path, "#!/bin/sh\necho linked again\n")
	if out, err := exec.Command(path).Output(); err != nil || string(out) != "linked again\n" {
		t.Fatalf("after Write: output = %q, %v", out, err)
	}
	after, err := os.ReadFile(wrapper)
	if err != nil || string(after) != string(before) {
		t.Fatalf("shared wrapper changed: %v", err)
	}
}
