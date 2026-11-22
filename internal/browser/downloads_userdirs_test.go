package browser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Downloads folder resolves through the environment a home controls:
// XDG_DOWNLOAD_DIR in the user-dirs file on Linux, $HOME/Downloads
// otherwise. A missing folder is created, as browsers do; a path that is
// not a folder is an error, never a fallback.
func TestTheDownloadsFolderResolvesFromTheHome(t *testing.T) {
	downloads := testDownloadsFolder(t)
	home := filepath.Dir(downloads)
	if got, err := userDownloadsDir(); err != nil || got != downloads {
		t.Fatalf("userDownloadsDir() = %q, %v, want %q", got, err, downloads)
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(home, "Elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	userDirs := filepath.Join(config, "user-dirs.dirs")
	for _, tc := range []struct{ line, want string }{
		{`XDG_DOWNLOAD_DIR="$HOME/Elsewhere"`, elsewhere},
		{`XDG_DOWNLOAD_DIR="` + elsewhere + `"`, elsewhere},
		{`XDG_DESKTOP_DIR="$HOME/Desktop"`, downloads},
		{"XDG_DOWNLOAD_DIR=\"$HOME/Downloads\"\nXDG_DOWNLOAD_DIR=\"$HOME/Elsewhere\"", elsewhere},
	} {
		if err := os.WriteFile(userDirs, []byte("# xdg-user-dirs\n"+tc.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		want := tc.want
		if runtime.GOOS != "linux" {
			want = downloads
		}
		if got, err := userDownloadsDir(); err != nil || got != want {
			t.Errorf("with %s: userDownloadsDir() = %q, %v, want %q", tc.line, got, err, want)
		}
	}
	if err := os.Remove(userDirs); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(downloads); err != nil {
		t.Fatal(err)
	}
	if got, err := userDownloadsDir(); err != nil || got != downloads {
		t.Fatalf("a missing Downloads folder answers %q, %v, want it created", got, err)
	}
	if info, err := os.Stat(downloads); err != nil || !info.IsDir() {
		t.Fatalf("the missing Downloads folder was not created: %v", err)
	}
	if err := os.Remove(downloads); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(downloads, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := userDownloadsDir(); err == nil || !strings.Contains(err.Error(), "is not a folder") {
		t.Fatalf("a Downloads file answers %v", err)
	}
}

// A user-dirs.dirs value reads as xdg-user-dirs writes it: double-quoted,
// a backslash taking the next character, $HOME expanded at the start only.
func TestUserDirsValuesReadAsXdgUserDirsWritesThem(t *testing.T) {
	const home = "/home/u"
	for _, tc := range []struct {
		value, want, err string
	}{
		{`"$HOME/Downloads\$archive"`, `/home/u/Downloads$archive`, ""},
		{`"$HOME/say \"hi\""`, `/home/u/say "hi"`, ""},
		{`"$HOME/back\\slash"`, `/home/u/back\slash`, ""},
		{`"$HOME"`, `/home/u`, ""},
		{`"$HOME/"`, `/home/u`, ""},
		{`"/srv/downloads"`, `/srv/downloads`, ""},
		{`"/srv/x" # a comment`, `/srv/x`, ""},
		{`"\$HOME/x"`, "", "not absolute"},
		{`"Downloads"`, "", "not absolute"},
		{`"$HOMEDIR/x"`, "", "more than a path"},
		{`"$HOME/unterminated`, "", "closing quote"},
		{`$HOME/Downloads`, "", "double quotes"},
		{`"/a" "/b"`, "", "follows the closing quote"},
	} {
		got, err := userDirsValue(tc.value, home)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s reads as %q, %v, want an error saying %q", tc.value, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s reads as %q, %v, want %q", tc.value, got, err, tc.want)
		}
	}
}

// A relocated folder: a valid value with an escaped $ names a
// folder downloads go to.
func TestARelocatedDownloadsFolderWithAnEscapeIsUsed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("user-dirs.dirs is read on Linux")
	}
	manager, _, access, page := newDownloadManager(t)
	home := os.Getenv("HOME")
	archive := filepath.Join(home, "Downloads$archive")
	if err := os.Mkdir(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "user-dirs.dirs"), []byte(`XDG_DOWNLOAD_DIR="$HOME/Downloads\$archive"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "a.bin", 10)
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if item.State != downloadStateCompleted || filepath.Dir(item.path) != archive {
		t.Fatalf("the download lists as %+v at %s, want it completed in %s", item.ThreadDownload, item.path, archive)
	}
}

// A Downloads folder relocated while a profile lives takes the next
// download; one removed while a download runs fails that download on its
// row, and nothing is written anywhere else.
func TestADownloadsFolderThatMovesOrGoesNeverRedirectsAWrite(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the user folders file is Linux's")
	}
	manager, _, access, page := newDownloadManager(t)
	home := os.Getenv("HOME")
	relocated := filepath.Join(home, "Elsewhere")
	if err := os.Mkdir(relocated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "user-dirs.dirs"), []byte(`XDG_DOWNLOAD_DIR="$HOME/Elsewhere"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "moved.bin", 10)
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if item.State != downloadStateCompleted || filepath.Dir(item.path) != relocated {
		t.Fatalf("after the move the download lists as %+v at %s", item.ThreadDownload, item.path)
	}

	// The folder goes after the engine wrote the file and before its finish
	// is handled; WebKit writes through the file it opened.
	manager.engine.(*fakeEngine).beforeFinishReport = func(string) {
		if err := os.RemoveAll(relocated); err != nil {
			t.Error(err)
		}
	}
	startDownload(t, manager, access, page, "orphan.bin", 10, "hold")
	held, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if err := fakeProfileOf(t, manager).finishHeld(held.engineID); err != nil {
		t.Fatal(err)
	}
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateFailed || !strings.Contains(got.Error, "is gone from") {
		t.Fatalf("a download whose folder went lists as %+v", got)
	}
	if _, err := os.Stat(relocated); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed folder was recreated: %v", err)
	}
	if names := folderNames(t, filepath.Join(home, "Downloads")); len(names) != 0 {
		t.Fatalf("the old Downloads folder holds %q", names)
	}
}

// A Downloads folder that cannot be used fails the download on its row
// with the reason as it starts, before the engine writes anything; nothing
// falls back elsewhere. A missing folder is created.
func TestAnUnusableDownloadsFolderFailsTheRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, downloads string)
		reason string
	}{
		{"missing", func(t *testing.T, downloads string) {
			if err := os.Remove(downloads); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"not a folder", func(t *testing.T, downloads string) {
			if err := os.Remove(downloads); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(downloads, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "is not a folder"},
		{"cannot be created", func(t *testing.T, downloads string) {
			if err := os.Remove(downloads); err != nil {
				t.Fatal(err)
			}
			home := filepath.Dir(downloads)
			if os.Geteuid() == 0 {
				t.Skip("root creates a folder in a read-only one")
			}
			if err := os.Chmod(home, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
		}, "create the Downloads folder"},
		{"read-only", func(t *testing.T, downloads string) {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only folder")
			}
			if err := os.Chmod(downloads, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(downloads, 0o700) })
		}, "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, log, access, page := newDownloadManager(t)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			tc.break_(t, downloads)
			log.take()
			startDownload(t, manager, access, page, "a.bin", 10)
			listed := threadDownloads(manager, access)
			if n := countKind(log.take(), "download"); n != 1 {
				t.Fatalf("the download was announced %d times, want once", n)
			}
			if tc.reason == "" {
				if len(listed) != 1 || listed[0].State != downloadStateCompleted {
					t.Fatalf("the thread lists %+v, want the download completed in a created folder", listed)
				}
				return
			}
			if len(listed) != 1 || listed[0].State != downloadStateFailed || !strings.Contains(listed[0].Error, tc.reason) {
				t.Fatalf("the thread lists %+v, want a failure that says %q", listed, tc.reason)
			}
			if info, err := os.Stat(downloads); err == nil && info.IsDir() {
				if names := folderNames(t, downloads); len(names) != 0 {
					t.Fatalf("the Downloads folder holds %q", names)
				}
			}
		})
	}
}
