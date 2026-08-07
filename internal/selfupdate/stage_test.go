package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// writeArtifact writes payload to a fresh file in dir and returns its path and
// SHA-256.
func writeArtifact(t *testing.T, dir, name string, payload []byte) (string, []byte) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256(payload)
	return path, sum[:]
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestStageCopyVerifiedArtifact(t *testing.T) {
	srcDir := t.TempDir()
	payload := bytes.Repeat([]byte("agent-overflow"), 20_000)
	src, digest := writeArtifact(t, srcDir, "source.bin", payload)

	// dstDir does not exist yet: StageCopy must create it.
	dstDir := filepath.Join(t.TempDir(), StagingDirName)
	final, err := StageCopy(src, dstDir, "agent-overflow-wsl-amd64.exe", digest)
	if err != nil {
		t.Fatalf("StageCopy: %v", err)
	}

	if want := filepath.Join(dstDir, "agent-overflow-wsl-amd64.exe"); final != want {
		t.Fatalf("final path = %q, want %q", final, want)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("staged bytes differ from the source (%d vs %d bytes)", len(got), len(payload))
	}
	if names := dirEntries(t, dstDir); len(names) != 1 || names[0] != "agent-overflow-wsl-amd64.exe" {
		t.Fatalf("staging dir = %v, want only the staged artifact (temp litter left behind?)", names)
	}
}

func TestStageCopyDigestMismatchLeavesNothing(t *testing.T) {
	srcDir := t.TempDir()
	src, _ := writeArtifact(t, srcDir, "source.bin", []byte("the real payload"))
	wrong := sha256.Sum256([]byte("a different payload"))

	dstDir := filepath.Join(t.TempDir(), StagingDirName)
	if _, err := StageCopy(src, dstDir, "agent-overflow-wsl-amd64.exe", wrong[:]); err == nil {
		t.Fatal("StageCopy with a mismatched digest = nil, want an error")
	}

	if _, err := os.Stat(filepath.Join(dstDir, "agent-overflow-wsl-amd64.exe")); !os.IsNotExist(err) {
		t.Fatalf("destination exists after a digest mismatch (stat err = %v)", err)
	}
	if names := dirEntries(t, dstDir); len(names) != 0 {
		t.Fatalf("staging dir = %v, want empty after a digest mismatch", names)
	}
}

func TestStageCopyRejectsUnusableInputs(t *testing.T) {
	srcDir := t.TempDir()
	src, digest := writeArtifact(t, srcDir, "source.bin", []byte("payload"))
	dstDir := t.TempDir()

	tests := []struct {
		name     string
		src      string
		dstDir   string
		filename string
		digest   []byte
	}{
		{"empty src", "", dstDir, "a.exe", digest},
		{"missing src", filepath.Join(srcDir, "nope.bin"), dstDir, "a.exe", digest},
		{"empty dst dir", src, "", "a.exe", digest},
		{"nested filename", src, dstDir, filepath.Join("sub", "a.exe"), digest},
		{"traversal filename", src, dstDir, "../a.exe", digest},
		{"short digest", src, dstDir, "a.exe", digest[:16]},
		{"nil digest", src, dstDir, "a.exe", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := StageCopy(tc.src, tc.dstDir, tc.filename, tc.digest); err == nil {
				t.Fatal("StageCopy = nil, want an error")
			}
		})
	}
	if names := dirEntries(t, dstDir); len(names) != 0 {
		t.Fatalf("dst dir = %v, want empty after only rejected copies", names)
	}
}

func TestSweepStagingDir(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"agent-overflow-wsl-amd64.exe", stagingTempPrefix + "crashed-123"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	keep := filepath.Join(dir, "subdir")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := SweepStagingDir(dir); err != nil {
		t.Fatalf("SweepStagingDir: %v", err)
	}
	if names := dirEntries(t, dir); len(names) != 1 || names[0] != "subdir" {
		t.Fatalf("after sweep = %v, want only the subdirectory", names)
	}
}

func TestSweepStagingDirMissingDirIsNotAnError(t *testing.T) {
	if err := SweepStagingDir(filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Fatalf("SweepStagingDir on a missing dir = %v, want nil", err)
	}
	if err := SweepStagingDir(""); err == nil {
		t.Fatal("SweepStagingDir(\"\") = nil, want an error")
	}
}
