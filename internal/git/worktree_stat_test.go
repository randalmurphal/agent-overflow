package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A path under a regular file names nothing, on every platform.
func TestStatWorktreeFileReadsAPathUnderAFileAsMissing(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, follow := range []bool{false, true} {
		if _, state := statWorktreeFile(filepath.Join(file, ".gitattributes"), time.Now(), follow); state != worktreeFileMissing {
			t.Fatalf("follow=%v: state = %v, want missing", follow, state)
		}
	}
}
