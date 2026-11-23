package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"agent-overflow/internal/atomicfile"
)

// Where the filesystem refuses hard links and the no-replace rename both,
// atomicfile.ExclusiveRename's own error must still read as a refused
// rename, so the row says why the download is not saved. No filesystem in
// a test refuses RENAME_NOREPLACE, so the rename here moves a directory
// into itself: the kernel refuses that with the same EINVAL, and no hard
// link can make it either, so ExclusiveRename answers its real error for
// both refusals.
func TestAPublishWhereTheFilesystemRefusesBothSaysSoOnTheRow(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	linkDownload = func(staged, candidate string) error {
		return &os.LinkError{Op: "link", Old: staged, New: candidate, Err: syscall.EPERM}
	}
	refusing := t.TempDir()
	renameDownload = func(string, string) error {
		return atomicfile.ExclusiveRename(refusing, filepath.Join(refusing, "into-itself"))
	}
	t.Cleanup(func() { linkDownload, renameDownload = linkFile, atomicfile.ExclusiveRename })
	var staged string
	manager.engine.(*fakeEngine).beforeFinishReport = func(file string) { staged = file }
	startDownload(t, manager, access, page, "stuck.bin", 10)

	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateFailed || !strings.Contains(got.Error, "supports neither hard links nor renames") {
		t.Fatalf("the download lists as %+v, want a failure saying the filesystem supports neither", got)
	}
	if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file is still there: %v", err)
	}
	if names := folderNames(t, downloads); len(names) != 0 {
		t.Fatalf("the Downloads folder holds %q", names)
	}
}
