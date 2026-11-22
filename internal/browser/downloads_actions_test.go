package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Show in folder hands the finished file to the file manager; a file gone
// since is an error naming it.
func TestRevealingADownloadShowsItsFile(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			gone := "notes.txt is no longer at"
			if mode.saving == downloadsAsArtifacts {
				gone = "notes.txt is gone; downloads are removed after 7 days"
			}
			testRevealingADownloadShowsItsFile(t, mode.saving, gone)
		})
	}
}

func testRevealingADownloadShowsItsFile(t *testing.T, saving downloadSaving, gone string) {
	manager, _, access, page := newDownloadManagerFor(t, saving)
	var revealed []string
	manager.revealFileInFileManager = func(_ context.Context, path string) error {
		revealed = append(revealed, path)
		return nil
	}
	startDownload(t, manager, access, page, "notes.txt", 10)
	download := threadDownloads(manager, access)[0]
	item, _ := manager.threadDownload(access, DownloadKey(download.ID))
	if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(download.ID)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(revealed, []string{item.path}) {
		t.Fatalf("revealed %q, want %q", revealed, item.path)
	}
	if err := os.Remove(item.path); err != nil {
		t.Fatal(err)
	}
	if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(download.ID)); err == nil || !strings.Contains(err.Error(), gone) {
		t.Fatalf("revealing a download whose file is gone answers %v, want %q", err, gone)
	}
	// Another thread cannot reach this thread's download.
	other := manager.Bind(Caller{ThreadID: "other", Workspace: access.Workspace()})
	if err := manager.RevealThreadDownload(t.Context(), other, DownloadKey(download.ID)); !errors.Is(err, ErrDownloadUnavailable) {
		t.Fatalf("another thread's reveal answers %v", err)
	}
}

// A row acts only on the file its download wrote. Moving a download out
// frees its name for the next one, so the two rows share a path: the older
// row refuses the newer file, and once the older file is back in its place
// the newer row refuses it.
func TestADownloadRowActsOnlyOnItsOwnFile(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	var revealed []string
	manager.revealFileInFileManager = func(_ context.Context, path string) error {
		revealed = append(revealed, path)
		return nil
	}
	act := func(id string) error {
		revealErr := manager.RevealThreadDownload(t.Context(), access, DownloadKey(id))
		_, openErr := manager.OpenThreadDownload(t.Context(), access, DownloadKey(id))
		if (revealErr == nil) != (openErr == nil) {
			t.Fatalf("Show in folder answers %v and Open %v", revealErr, openErr)
		}
		return revealErr
	}
	replaced := func(err error) bool { return err != nil && strings.Contains(err.Error(), "moved, changed or replaced") }

	startDownload(t, manager, access, page, "report.pdf", 10)
	older := threadDownloads(manager, access)[0]
	olderItem, _ := manager.threadDownload(access, DownloadKey(older.ID))
	moved := olderItem.path + ".moved"
	if err := os.Rename(olderItem.path, moved); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "report.pdf", 20)
	newer := threadDownloads(manager, access)[0]
	newerItem, _ := manager.threadDownload(access, DownloadKey(newer.ID))
	if newerItem.path != olderItem.path {
		t.Fatalf("the second download is at %s, not the freed name %s", newerItem.path, olderItem.path)
	}

	if err := act(older.ID); !replaced(err) {
		t.Fatalf("the older row acting on the newer file answers %v", err)
	}
	if err := act(newer.ID); err != nil {
		t.Fatalf("the newer row: %v", err)
	}

	if err := os.Rename(newerItem.path, newerItem.path+".newer"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, olderItem.path); err != nil {
		t.Fatal(err)
	}
	if err := act(newer.ID); !replaced(err) {
		t.Fatalf("the newer row acting on the older file answers %v", err)
	}
	if err := act(older.ID); err != nil {
		t.Fatalf("the older row with its own file back: %v", err)
	}
	if !slices.Equal(revealed, []string{newerItem.path, olderItem.path}) {
		t.Fatalf("revealed %q", revealed)
	}
}

// Deleting a finished download and downloading the same name again can give
// the new file the deleted one's inode; the older row must not act on it.
// The reuse is forced: the older row's record is made to name the newer
// file's device and inode, once as a same-size download made earlier (the
// usual redownload) and once at the newer file's time with another size.
func TestADownloadRowRefusesALaterDownloadOnAReusedInode(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	manager.revealFileInFileManager = func(context.Context, string) error { return nil }
	startDownload(t, manager, access, page, "report.pdf", 10)
	older := threadDownloads(manager, access)[0]
	olderItem, _ := manager.threadDownload(access, DownloadKey(older.ID))
	if err := os.Remove(olderItem.path); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "report.pdf", 10)
	newer := threadDownloads(manager, access)[0]
	newerItem, _ := manager.threadDownload(access, DownloadKey(newer.ID))
	if newerItem.path != olderItem.path {
		t.Fatalf("the second download is at %s, not the freed name %s", newerItem.path, olderItem.path)
	}

	for _, record := range []struct {
		name    string
		size    int64
		modTime time.Time
	}{
		// Apart in time, as two downloads are, however fine the
		// filesystem's timestamps.
		{"same size, earlier", olderItem.file.Size(), newerItem.file.ModTime().Add(-time.Minute)},
		{"other size, same time", newerItem.file.Size() + 1, newerItem.file.ModTime()},
	} {
		reused := statReshaped(t, newerItem.path, record.size, record.modTime, newerItem.file)
		current, err := os.Stat(newerItem.path)
		if err != nil || !os.SameFile(reused, current) {
			t.Fatalf("%s: the forced record does not share the newer file's inode: %v", record.name, err)
		}
		manager.downloadsMu.Lock()
		access.tb.downloads.find(DownloadKey(older.ID)).file = reused
		manager.downloadsMu.Unlock()

		if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(older.ID)); err == nil || !strings.Contains(err.Error(), "moved, changed or replaced") {
			t.Fatalf("%s: the older row on a reused inode answers %v", record.name, err)
		}
		if _, err := manager.OpenThreadDownload(t.Context(), access, DownloadKey(older.ID)); err == nil {
			t.Fatalf("%s: the older row opened the newer download", record.name)
		}
		if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(newer.ID)); err != nil {
			t.Fatalf("%s: the newer row: %v", record.name, err)
		}
	}
}

// statReshaped answers a stat of path taken while it had size and modTime,
// then puts the file back as it was (the fake engine writes zeros).
func statReshaped(t *testing.T, path string, size int64, modTime time.Time, was os.FileInfo) os.FileInfo {
	t.Helper()
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	reshaped, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, was.Size()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, was.ModTime(), was.ModTime()); err != nil {
		t.Fatal(err)
	}
	return reshaped
}

// Open shows a finished download of a type tabs show in a new tab; a page
// may open its own workspace's downloads and no other workspace's.
func TestOpeningADownloadShowsItInANewTab(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "chart.png", 10)
	startDownload(t, manager, access, page, "page.html", 10)
	listed := threadDownloads(manager, access)
	html, png := listed[0], listed[1]
	if html.Openable || !png.Openable {
		t.Fatalf("openable: html=%v png=%v, want only the image", html.Openable, png.Openable)
	}
	opened, err := manager.OpenThreadDownload(t.Context(), access, DownloadKey(png.ID))
	if err != nil {
		t.Fatal(err)
	}
	item, _ := manager.threadDownload(access, DownloadKey(png.ID))
	if opened.ID == page.ID || !strings.HasSuffix(opened.URL, filepath.ToSlash(item.path)) {
		t.Fatalf("the download opened as %+v, want a new tab at %s", opened, item.path)
	}
	if _, err := manager.OpenThreadDownload(t.Context(), access, DownloadKey(html.ID)); err == nil {
		t.Fatal("an HTML download opened in a tab")
	}

	otherWorkspace := manager.Bind(Caller{ThreadID: "other", Workspace: t.TempDir()})
	if _, err := manager.authorizeFile(otherWorkspace.Caller(), item.path); err == nil {
		t.Fatal("another workspace may open this workspace's download")
	}
	if _, err := manager.authorizeFile(access.Caller(), item.path); err != nil {
		t.Fatalf("the workspace may not open its own download: %v", err)
	}
}
