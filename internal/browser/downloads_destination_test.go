package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Where a download goes is the engine's kind (downloadSaving): an engine in
// this process writes into AO's directory and the Manager publishes the
// finished file into the user's Downloads folder; WebView2 saves there
// itself; headless Chromium's stay AO's artifacts, with quotas, a
// per-download cap and pruning. In the Downloads folder AO bounds nothing
// and deletes or replaces nothing it did not put there.

func TestEachEngineSavesDownloadsItsOwnWay(t *testing.T) {
	artifactFake := newFakeEngine()
	artifactFake.saving = downloadsAsArtifacts
	for _, tc := range []struct {
		name   string
		engine browserEngine
		want   downloadSaving
	}{
		{"headless Chromium", &headlessEngine{}, downloadsAsArtifacts},
		{"launcher-hosted WebView2", &hostedEngine{}, downloadsSavedByEngine},
		{"fake standing in for an engine in this process", newFakeEngine(), downloadsStaged},
		{"fake standing in for headless Chromium", artifactFake, downloadsAsArtifacts},
	} {
		if got := tc.engine.DownloadSaving(); got != tc.want {
			t.Errorf("%s saves downloads as %d, want %d", tc.name, got, tc.want)
		}
		manager := &Manager{engine: tc.engine}
		if got := manager.downloadsToArtifacts(); got != (tc.want == downloadsAsArtifacts) {
			t.Errorf("%s keeps downloads as artifacts: %v", tc.name, got)
		}
	}
}

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

// The reviewer's relocated folder: a valid value with an escaped $ names a
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
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if item.State != downloadStateCompleted || filepath.Dir(item.path) != archive {
		t.Fatalf("the download lists as %+v at %s, want it completed in %s", item.ThreadDownload, item.path, archive)
	}
}

// A published download takes a name nothing has: the name the site
// suggests, then " (1)", " (2)" before its extension. A file, or a link,
// already at a name stays as it is, and so does one another program puts
// there just as the download takes it.
func TestAPublishedDownloadNeverReplacesAFile(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	mine := filepath.Join(downloads, "report.pdf")
	if err := os.WriteFile(mine, []byte("the user's own report"), 0o600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(private, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(downloads, "report (1).pdf")); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "report.pdf", 10)
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCompleted || got.Name != "report (2).pdf" {
		t.Fatalf("the download lists as %+v, want report (2).pdf", got)
	}
	if got, err := os.ReadFile(mine); err != nil || string(got) != "the user's own report" {
		t.Fatalf("the user's file reads %q, %v", got, err)
	}
	if got, err := os.ReadFile(private); err != nil || string(got) != "private" {
		t.Fatalf("the link's target reads %q, %v", got, err)
	}

	// Another program creates a file at the free name just before the link.
	var theirs string
	linkDownload = func(staged, candidate string) error {
		if theirs == "" {
			theirs = candidate
			if err := os.WriteFile(candidate, []byte("theirs"), 0o600); err != nil {
				t.Errorf("take %s: %v", candidate, err)
			}
		}
		return os.Link(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = os.Link })
	startDownload(t, manager, access, page, "race.bin", 10)
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCompleted || got.Name != "race (1).bin" {
		t.Fatalf("a download whose name was taken lists as %+v, want race (1).bin", got)
	}
	if got, _ := os.ReadFile(filepath.Join(downloads, "race.bin")); string(got) != "theirs" || theirs != filepath.Join(downloads, "race.bin") {
		t.Fatalf("the other program's file reads %q (taken %q)", got, theirs)
	}
}

// The published file is the staged one, and the staged name is gone; the
// row's identity is the published file's.
func TestPublishingMovesTheStagedFile(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	var staged string
	manager.engine.(*fakeEngine).beforeFinishReport = func(file string) { staged = file }
	startDownload(t, manager, access, page, "notes.txt", 10)
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file %s is still there: %v", staged, err)
	}
	current, err := os.Stat(item.path)
	if err != nil || !sameDownloadedFile(item.file, current) {
		t.Fatalf("the row's identity is not the published file's: %v", err)
	}
}

// A filesystem that refuses hard links gets the rename that refuses to
// replace: the download still takes a free name, and a file that appears at
// a name as it is taken stays as it is.
func TestALinkRefusedFallsBackToANoReplaceRename(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	linkDownload = func(staged, candidate string) error {
		return &os.LinkError{Op: "link", Old: staged, New: candidate, Err: syscall.EPERM}
	}
	var theirs string
	renameDownload = func(staged, candidate string) error {
		if theirs == "" {
			theirs = candidate
			if err := os.WriteFile(candidate, []byte("theirs"), 0o600); err != nil {
				t.Errorf("take %s: %v", candidate, err)
			}
		}
		return renameNoReplace(staged, candidate)
	}
	t.Cleanup(func() { linkDownload, renameDownload = os.Link, renameNoReplace })
	var staged string
	manager.engine.(*fakeEngine).beforeFinishReport = func(file string) { staged = file }
	startDownload(t, manager, access, page, "moved.bin", 10)
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if item.State != downloadStateCompleted || item.Name != "moved (1).bin" {
		t.Fatalf("the renamed download lists as %+v", item.ThreadDownload)
	}
	if got, _ := os.ReadFile(filepath.Join(downloads, "moved.bin")); string(got) != "theirs" || theirs != filepath.Join(downloads, "moved.bin") {
		t.Fatalf("the file put at the name reads %q (taken %q)", got, theirs)
	}
	current, err := os.Stat(item.path)
	if err != nil || !sameDownloadedFile(item.file, current) {
		t.Fatalf("the row's identity is not the published file's: %v", err)
	}
	if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file is still there: %v", err)
	}
}

// A finished artifact download takes its name in the workspace's download
// directory without replacing a file: another jar's download of the same
// name, finishing on its own engine stream, can take the name first, and
// keeps it.
func TestAnArtifactDownloadNeverReplacesAFileTakingItsName(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	workspace, err := canonicalRoot(access.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	dir := manager.workspaceDownloadDir(workspace)
	var theirs string
	linkDownload = func(source, candidate string) error {
		if theirs == "" {
			theirs = candidate
			if err := os.WriteFile(candidate, []byte("theirs"), 0o600); err != nil {
				t.Errorf("take %s: %v", candidate, err)
			}
		}
		return os.Link(source, candidate)
	}
	t.Cleanup(func() { linkDownload = os.Link })
	startDownload(t, manager, access, page, "report.pdf", 10)
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if item.State != downloadStateCompleted || item.path != filepath.Join(dir, "report-1.pdf") {
		t.Fatalf("the download lists as %+v at %q, want completed at report-1.pdf", item.ThreadDownload, item.path)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "report.pdf")); string(got) != "theirs" || theirs != filepath.Join(dir, "report.pdf") {
		t.Fatalf("the file put at the name reads %q (taken %q)", got, theirs)
	}
	if got, _ := os.ReadFile(item.path); len(got) != 10 {
		t.Fatalf("the download's file holds %d bytes, want 10", len(got))
	}
	if names := folderNames(t, dir); len(names) != 2 {
		t.Fatalf("the download directory holds %q, want the two files and not the engine's", names)
	}
}

// Of a workspace's artifact downloads only the finished files open: the
// file the engine is still writing is not in the workspace's directory.
func TestARunningArtifactDownloadIsNotOpenable(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	startDownload(t, manager, access, page, "done.bin", 10)
	startDownload(t, manager, access, page, "running.bin", 10, "hold")
	var done, running threadDownload
	for _, row := range threadDownloads(manager, access) {
		item, _ := manager.threadDownload(access, row.ID)
		if row.Name == "done.bin" {
			done = item
		} else {
			running = item
		}
	}
	if _, err := manager.authorizeFile(access.Caller(), done.path); err != nil {
		t.Fatalf("the finished download does not open: %v", err)
	}
	if _, err := manager.authorizeFile(access.Caller(), heldFile(t, manager, running.engineID)); err == nil {
		t.Fatal("the file of a running download opens")
	}
}

// A file an earlier release left in the workspace's old download directory,
// where its engine wrote running downloads beside finished ones, does not
// open: only the finished files AO moved since are granted.
func TestAnEarlierReleasesDownloadFileIsNotOpenable(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	workspace, err := canonicalRoot(access.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(manager.artifactRoot, "downloads", jarDirName(workspace))
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"4b1f6a2e-guid.crdownload", "report.pdf"} {
		path := filepath.Join(old, name)
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.authorizeFile(access.Caller(), path); err == nil {
			t.Fatalf("%s in an earlier release's download directory opens", name)
		}
	}
	startDownload(t, manager, access, page, "done.bin", 10)
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if _, err := manager.authorizeFile(access.Caller(), item.path); err != nil {
		t.Fatalf("a finished download does not open: %v", err)
	}
}

// A finished artifact download its workspace's directory cannot take fails
// with nothing kept: the engine's file goes and neither quota holds its
// bytes, so repeated failures use up no allowance.
func TestAnArtifactDownloadThatCannotBePlacedKeepsNothing(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	if err := os.RemoveAll(authority.downloadDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authority.downloadDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceBefore, artifactsBefore := authority.downloadBytes.Used(), manager.artifactBytes.Used()
	startDownload(t, manager, access, page, "report.pdf", 5)
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if item.State != downloadStateFailed {
		t.Fatalf("the download lists as %+v, want failed", item.ThreadDownload)
	}
	manager.mu.Lock()
	var engineDir string
	for _, scope := range manager.scopes {
		engineDir = scope.downloadDir
	}
	manager.mu.Unlock()
	awaitRemoved(t, filepath.Join(engineDir, filepath.Base(item.engineID)))
	if got := authority.downloadBytes.Used(); got != workspaceBefore {
		t.Fatalf("the workspace holds %d download bytes, want %d", got, workspaceBefore)
	}
	if got := manager.artifactBytes.Used(); got != artifactsBefore {
		t.Fatalf("the artifact root holds %d bytes, want %d", got, artifactsBefore)
	}
}

// With neither hard links nor a no-replace rename, or a staging directory
// on another filesystem, the download fails on its row with the reason:
// nothing is copied, nothing appears in Downloads, and the staged file goes.
func TestAPublishThatCannotMoveFailsTheRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		link   error
		rename error
		reason string
	}{
		{"neither", syscall.EPERM, syscall.EINVAL, "supports neither hard links nor renames"},
		{"no rename on the platform", syscall.EOPNOTSUPP, errNoReplaceRename, "supports neither hard links nor renames"},
		{"another filesystem", syscall.EXDEV, nil, "cross-device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManager(t)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			linkDownload = func(staged, candidate string) error {
				return &os.LinkError{Op: "link", Old: staged, New: candidate, Err: tc.link}
			}
			renameDownload = func(string, string) error { return tc.rename }
			t.Cleanup(func() { linkDownload, renameDownload = os.Link, renameNoReplace })
			var staged string
			manager.engine.(*fakeEngine).beforeFinishReport = func(file string) { staged = file }
			startDownload(t, manager, access, page, "stuck.bin", 10)
			got := threadDownloads(manager, access)[0]
			if got.State != downloadStateFailed || !strings.Contains(got.Error, tc.reason) {
				t.Fatalf("the download lists as %+v, want a failure saying %q", got, tc.reason)
			}
			if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the staged file is still there: %v", err)
			}
			if names := folderNames(t, downloads); len(names) != 0 {
				t.Fatalf("the Downloads folder holds %q", names)
			}
		})
	}
}

// Close waits for a publish in flight: when it returns, the file is in the
// Downloads folder and its row says so.
func TestCloseWaitsForAPublishInFlight(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	linking, release := make(chan struct{}), make(chan struct{})
	linkDownload = func(staged, candidate string) error {
		close(linking)
		<-release
		return os.Link(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = os.Link })
	startDownload(t, manager, access, page, "last.txt", 10, "hold")
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	profile := fakeProfileOf(t, manager)
	finished := make(chan error, 1)
	go func() { finished <- profile.finishHeld(item.engineID) }()
	<-linking
	closed := make(chan error, 1)
	go func() { closed <- manager.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned during the publish: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	got, _ := manager.threadDownload(access, item.ID)
	if got.State != downloadStateCompleted {
		t.Fatalf("after Close the download lists as %+v", got.ThreadDownload)
	}
	if data, err := os.ReadFile(got.path); err != nil || len(data) != 10 {
		t.Fatalf("after Close the published file reads %d bytes, %v", len(data), err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

// The staging directory is the app's own, private, in the Downloads
// folder: created with a missing Downloads folder, a real directory (a link
// put in its place is refused, and the download fails on its row with
// nothing written through it), and gone once no download stages in it.
func TestTheStagingDirectoryIsPrivateAndGoesWhenEmpty(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	if err := os.Remove(downloads); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "held.bin", 10, "hold")
	staging := filepath.Join(downloads, stagingDirName)
	info, err := os.Lstat(staging)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the staging directory: %v, %v", info, err)
	}
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if err := fakeProfileOf(t, manager).finishHeld(item.engineID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the empty staging directory stayed: %v", err)
	}
	if names := folderNames(t, downloads); !slices.Equal(names, []string{"held.bin"}) {
		t.Fatalf("the Downloads folder holds %q", names)
	}

	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, staging); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "linked.bin", 10)
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateFailed || !strings.Contains(got.Error, "is not a folder") {
		t.Fatalf("a download through a linked staging directory lists as %+v", got)
	}
	if names := folderNames(t, elsewhere); len(names) != 0 {
		t.Fatalf("the link's target holds %q", names)
	}
}

// A staged file another run left behind never collides with a new
// download's (staged names carry a token of their process), an old one is
// removed when the first download of the process uses the directory, a
// recent one stays, and a running download's file is never removed.
func TestStagedLeftoversNeverCollideAndOldOnesGo(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	staging := filepath.Join(os.Getenv("HOME"), "Downloads", stagingDirName)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * stagedLeftoverAge)
	stale, recent := filepath.Join(staging, "dl-2"), filepath.Join(staging, "dl-1")
	for _, path := range []string{stale, recent} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "first.bin", 10)
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCompleted {
		t.Fatalf("the first download after a leftover lists as %+v", got)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old leftover is still there: %v", err)
	}
	if got, err := os.ReadFile(recent); err != nil || string(got) != "partial" {
		t.Fatalf("the recent leftover reads %q, %v", got, err)
	}

	// The prune spares a file a running download owns, however old.
	startDownload(t, manager, access, page, "running.bin", 10, "hold")
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	running := manager.stagedPath(item.engineID)
	// WebKitGTK writes beside the staged file until the download finishes.
	companion := running + ".wkdownload"
	if err := os.WriteFile(companion, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{running, companion} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	stagingPrune(t, filepath.Dir(running))
	for _, path := range []string{running, companion} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the running download's file was pruned: %v", err)
		}
	}
	// Its cancel takes the companion with the staged file.
	if err := manager.CancelThreadDownload(access, item.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{running, companion} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("after the cancel %s is still there: %v", path, err)
		}
	}
	if got, _ := os.ReadFile(recent); string(got) != "partial" {
		t.Fatal("the recent leftover was pruned")
	}
}

// A staged download the Manager refuses gives up its staged file, and the
// staging folder goes once no download stages in it, even one whose engine
// has not created its file yet; a staged name carries its process's token.
func TestARefusedStagedDownloadLeavesNothing(t *testing.T) {
	manager, _, _, _ := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	if _, err := manager.stageDownload("unwritten"); err != nil {
		t.Fatal(err)
	}
	staged, err := manager.stageDownload("refused")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^dl-[0-9a-f]{12}-[0-9]+$`).MatchString(filepath.Base(staged)) {
		t.Fatalf("the staged name %s carries no process token", staged)
	}
	if err := os.WriteFile(staged, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if manager.downloadStarted(downloadStart{Frame: "no such frame", ID: "refused", SuggestedName: "r.bin"}) == downloadAdmitted {
		t.Fatal("a download from no page was admitted")
	}
	if names := folderNames(t, downloads); !slices.Equal(names, []string{stagingDirName}) {
		t.Fatalf("with a download still staging the Downloads folder holds %q", names)
	}
	if names := folderNames(t, filepath.Join(downloads, stagingDirName)); len(names) != 0 {
		t.Fatalf("after the refusal the staging folder holds %q", names)
	}
	manager.releaseStagedDownload("unwritten")
	if names := folderNames(t, downloads); len(names) != 0 {
		t.Fatalf("after the refusal the Downloads folder holds %q", names)
	}
}

// The prune removes only names AO stages under, exactly, and their engine's
// sidecars: an old file of the user's or another program's in the staging
// folder stays, a name that merely starts like a staged one included.
func TestTheStagingPruneLeavesFilesAONeverStaged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), stagingDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * stagedLeftoverAge)
	names := []string{"notes.txt", "dl-report", "dl-2", "dl-0123456789ab-7", "dl-0123456789ab-8.wkdownload", "dl-p42-3", "dl-2.user-notes.txt", "dl-3.wkdownload.bak"}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	stagingPrune(t, dir)
	if got := folderNames(t, dir); !slices.Equal(got, []string{"dl-2.user-notes.txt", "dl-3.wkdownload.bak", "dl-report", "notes.txt"}) {
		t.Fatalf("after the prune the staging folder holds %q, want only the files AO never staged", got)
	}
}

// Releasing a staged file removes it and its engine's sidecar, and nothing
// else beside it that starts with its name.
func TestReleasingAStagedFileRemovesOnlyItAndItsSidecar(t *testing.T) {
	downloads := testDownloadsFolder(t)
	staged, err := stageDownload()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{staged, staged + ".wkdownload", staged + ".user-notes.txt"} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	releaseStaged(staged)
	if got := folderNames(t, filepath.Join(downloads, stagingDirName)); !slices.Equal(got, []string{filepath.Base(staged) + ".user-notes.txt"}) {
		t.Fatalf("after the release the staging folder holds %q, want only the file AO never staged", got)
	}
}

// stagingPrune runs the staging directory's prune as its first use does.
func stagingPrune(t *testing.T, dir string) {
	t.Helper()
	staging.mu.Lock()
	defer staging.mu.Unlock()
	pruneStagingLocked(dir, time.Now())
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
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
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
	held, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
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

// A finish whose file was swapped for a link before its report was handled
// grants nothing: staged, the publish refuses the link and the row fails;
// saved by the engine, the row completes saying the file is a link, and
// neither the link nor its target opens.
func TestALinkSwappedInBeforeTheFinishGrantsNothing(t *testing.T) {
	for _, mode := range []struct {
		name   string
		saving downloadSaving
		state  string
		reason string
	}{
		{"staged", downloadsStaged, downloadStateFailed, "not a regular file"},
		{"saved by the engine", downloadsSavedByEngine, downloadStateCompleted, "is a link"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, mode.saving)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			private := filepath.Join(t.TempDir(), "private.txt")
			if err := os.WriteFile(private, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			manager.engine.(*fakeEngine).beforeFinishReport = func(file string) {
				if err := os.Remove(file); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(private, file); err != nil {
					t.Error(err)
				}
			}
			startDownload(t, manager, access, page, "file.txt", 10)
			got := threadDownloads(manager, access)[0]
			if got.State != mode.state || !strings.Contains(got.Error, mode.reason) || got.Openable {
				t.Fatalf("the swapped download lists as %+v, want %s saying %q", got, mode.state, mode.reason)
			}
			if _, err := manager.authorizeFile(access.Caller(), private); err == nil {
				t.Fatal("the link's target is openable")
			}
			if _, err := manager.authorizeFile(access.Caller(), filepath.Join(downloads, "file.txt")); err == nil {
				t.Fatal("the link is openable")
			}
			if _, err := manager.OpenThreadDownload(t.Context(), access, got.ID); err == nil {
				t.Fatal("the row opens")
			}
			if got, _ := os.ReadFile(private); string(got) != "private" {
				t.Fatalf("the link's target reads %q", got)
			}
		})
	}
}

// A download the engine saved under a path it could not name or reach
// (the launcher's report had no room for it, or wslpath failed) completes
// saying so, with nothing to open.
func TestAnEngineSavedFileWithNoPathCompletesWithItsReason(t *testing.T) {
	manager, _, access, page := newDownloadManagerFor(t, downloadsSavedByEngine)
	frame := paneHandle(manager, access, page.ID)
	if manager.downloadStarted(downloadStart{Frame: frame, ID: "dl-x-1", URL: "https://example.test/x", SuggestedName: "x.bin"}) != downloadAdmitted {
		t.Fatal("the download was refused")
	}
	const reason = "the saved file's path is too long to report"
	manager.downloadProgress(downloadProgress{ID: "dl-x-1", Received: 5, Total: 5, State: downloadCompleted, Error: reason})
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateCompleted || got.Error != reason || got.Openable {
		t.Fatalf("the download lists as %+v, want completed saying %q with nothing to open", got, reason)
	}
	if _, err := manager.OpenThreadDownload(t.Context(), access, got.ID); err == nil {
		t.Fatal("the row opens")
	}
}

// A page that leaves while its download's file is being published leaves
// that download to the publish, which settles it once, completed.
func TestAPageLeavingDuringThePublishLeavesItToFinish(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	linking, release := make(chan struct{}), make(chan struct{})
	linkDownload = func(staged, candidate string) error {
		close(linking)
		<-release
		return os.Link(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = os.Link })
	startDownload(t, manager, access, page, "late.txt", 10, "hold")
	item, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	profile := fakeProfileOf(t, manager)
	finished := make(chan error, 1)
	go func() { finished <- profile.finishHeld(item.engineID) }()
	<-linking
	log.take()
	if err := manager.closeLivePage(t.Context(), access.tb, page.ID); err != nil {
		t.Fatal(err)
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateRunning {
		t.Fatalf("while its file is published the download lists as %+v, want running", got)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateCompleted || !got.Openable {
		t.Fatalf("the published download lists as %+v, want completed", got)
	}
	if n := countKind(log.take(), "download"); n != 1 {
		t.Fatalf("the download was announced %d times, want once", n)
	}
}

// A canceled or failed download leaves the Downloads folder as it was: an
// engine in this process wrote only into the staging folder, which goes
// with its staged file, and one that saves there itself takes its own
// partial file with it and nothing beside it.
func TestACanceledOrFailedDownloadLeavesDownloadsAsItWas(t *testing.T) {
	for _, mode := range []struct {
		name   string
		saving downloadSaving
	}{{"staged", downloadsStaged}, {"saved by the engine", downloadsSavedByEngine}} {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, mode.saving)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			neighbor := filepath.Join(downloads, "neighbor.bin")
			if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			startDownload(t, manager, access, page, "held.bin", 100, "hold")
			held := threadDownloads(manager, access)[0]
			item, _ := manager.threadDownload(access, held.ID)
			partial := heldFile(t, manager, item.engineID)
			// While it runs, only the staging folder is new in Downloads, or
			// the engine's own file where it saves there itself.
			want := []string{stagingDirName, "neighbor.bin"}
			if mode.saving == downloadsSavedByEngine {
				want = []string{"held.bin", "neighbor.bin"}
			} else if filepath.Dir(partial) != filepath.Join(downloads, stagingDirName) {
				t.Fatalf("the running download writes %s", partial)
			}
			if names := folderNames(t, downloads); !slices.Equal(names, want) {
				t.Fatalf("during the download the Downloads folder holds %q, want %q", names, want)
			}
			if err := manager.CancelThreadDownload(access, held.ID); err != nil {
				t.Fatal(err)
			}
			startDownload(t, manager, access, page, "broken.bin", 100, "fail")
			listed := threadDownloads(manager, access)
			if listed[0].State != downloadStateFailed || listed[1].State != downloadStateCanceled {
				t.Fatalf("the thread lists %+v, want a failure and a cancel", listed)
			}
			if _, err := os.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the partial file %s is still there: %v", partial, err)
			}
			if names := folderNames(t, downloads); !slices.Equal(names, []string{"neighbor.bin"}) {
				t.Fatalf("the Downloads folder holds %q, want only the neighbor", names)
			}
		})
	}
}

func folderNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// No quota, per-download cap or pruning applies to a download that ends up
// in the Downloads folder; all three bound downloads AO keeps as
// artifacts.
func TestOnlyArtifactDownloadsAreBounded(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			artifacts := mode.saving == downloadsAsArtifacts
			manager, _, access, page := newDownloadManagerFor(t, mode.saving)
			frame := paneHandle(manager, access, page.ID)

			authorityOf(t, manager, access).downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
			admitted := manager.downloadStarted(downloadStart{Frame: frame, ID: "quota", SuggestedName: "q.bin"}) == downloadAdmitted
			if admitted == artifacts {
				t.Fatalf("a download past the workspace quota was admitted: %v", admitted)
			}
			authorityOf(t, manager, access).downloadBytes.SetUsed(0)

			if manager.downloadStarted(downloadStart{Frame: frame, ID: "big", SuggestedName: "big.bin"}) != downloadAdmitted {
				t.Fatal("a download within quota was refused")
			}
			manager.downloadProgress(downloadProgress{ID: "big", Received: maxDownloadBytes + 1, State: downloadInProgress})
			got := threadDownloads(manager, access)[0]
			capped := got.State == downloadStateCanceled && strings.Contains(got.Error, "download exceeds")
			if capped != artifacts {
				t.Fatalf("past the per-download cap the download lists as %+v", got)
			}

			startDownload(t, manager, access, page, "old.txt", 10)
			item, err := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
			if err != nil || item.path == "" {
				t.Fatalf("the finished download: %+v, %v", item, err)
			}
			old := time.Now().Add(-2 * artifactMaxAge)
			if err := os.Chtimes(item.path, old, old); err != nil {
				t.Fatal(err)
			}
			manager.artifactInitMu.Lock()
			manager.artifactReady = false
			manager.artifactInitMu.Unlock()
			manager.prepareArtifacts()
			_, statErr := os.Stat(item.path)
			if pruned := errors.Is(statErr, os.ErrNotExist); pruned != artifacts {
				t.Fatalf("an old download was pruned: %v (%v)", pruned, statErr)
			}
		})
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

// Nothing that deletes a thread, clears site data, forgets the browser or
// prunes artifacts deletes a file in the Downloads folder: neither a
// finished download nor the user's own file.
func TestForgettingBrowsingLeavesTheDownloadsFolderAlone(t *testing.T) {
	for _, mode := range []struct {
		name   string
		saving downloadSaving
	}{{"staged", downloadsStaged}, {"saved by the engine", downloadsSavedByEngine}} {
		for _, tc := range []struct {
			name string
			act  func(t *testing.T, manager *Manager, access Access)
		}{
			{"thread delete", func(t *testing.T, manager *Manager, access Access) {
				if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
					t.Fatal(err)
				}
			}},
			{"clear site data", func(t *testing.T, manager *Manager, _ Access) {
				if err := manager.ClearSiteData(t.Context()); err != nil {
					t.Fatal(err)
				}
			}},
			{"browser turned off", func(t *testing.T, manager *Manager, _ Access) {
				if err := manager.Reconfigure(Config{Enabled: false}); err != nil {
					t.Fatal(err)
				}
			}},
			{"jar switch", func(t *testing.T, manager *Manager, access Access) {
				if err := manager.SetJar(context.Background(), access, JarPrivate, JarSwitchConfirmed); err != nil {
					t.Fatal(err)
				}
			}},
			{"artifact prune", func(t *testing.T, manager *Manager, _ Access) {
				manager.artifactInitMu.Lock()
				manager.artifactReady = false
				manager.artifactInitMu.Unlock()
				manager.prepareArtifacts()
			}},
			{"clear the list", func(t *testing.T, manager *Manager, access Access) {
				if err := manager.ClearThreadDownloads(access); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				manager, _, access, page := newDownloadManagerFor(t, mode.saving)
				downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
				mine := filepath.Join(downloads, "mine.txt")
				if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
					t.Fatal(err)
				}
				startDownload(t, manager, access, page, "done.txt", 10)
				item, err := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-2 * artifactMaxAge)
				for _, path := range []string{mine, item.path} {
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
				}
				tc.act(t, manager, access)
				for _, path := range []string{mine, item.path} {
					if _, err := os.Stat(path); err != nil {
						t.Fatalf("%s: %v", path, err)
					}
				}
			})
		}
	}
}

// browser_open_file may open a finished download of its workspace that the
// Manager still lists, and only that file: not its neighbor, not from
// another workspace, not a file that replaced it, and not once no list
// holds its row.
func TestOpenFileAllowsAListedDownloadAndNotItsNeighbor(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	neighbor := filepath.Join(downloads, "neighbor.txt")
	if err := os.WriteFile(neighbor, []byte("not a download"), 0o600); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "notes.txt", 10)
	item, err := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// Listed in the thread's pane after its page closed.
	if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.OpenFile(t.Context(), access, item.path, OpenOptions{}); err != nil {
		t.Fatalf("the workspace's listed download does not open: %v", err)
	}
	if _, err := manager.OpenFile(t.Context(), access, neighbor, OpenOptions{}); err == nil {
		t.Fatal("a file beside the download opened")
	}
	other := manager.Bind(Caller{ThreadID: "other", Workspace: t.TempDir()})
	if _, err := manager.authorizeFile(other.Caller(), item.path); err == nil {
		t.Fatal("another workspace may open this workspace's download")
	}
	// A link to the download resolves to it.
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(item.path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), link); err != nil {
		t.Fatalf("a link to the download is refused: %v", err)
	}
	// A file put in its place is not the download.
	if err := os.Remove(item.path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(item.path, bytes.Repeat([]byte("x"), 10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), item.path); err == nil {
		t.Fatal("a file that replaced the download opened")
	}
	// Once no list holds its row, the download is not openable.
	startDownload(t, manager, access, mustOpen(t, manager, access, "https://example.test/files"), "later.txt", 10)
	later, _ := manager.threadDownload(access, threadDownloads(manager, access)[0].ID)
	manager.closeAllPages(t, access)
	if err := manager.ClearThreadDownloads(access); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), later.path); err == nil {
		t.Fatal("a download no list holds opened")
	}
}

// browser_downloads lists a page's download after the thread's pane list
// dropped it, and a download it lists opens.
func TestADownloadThePageStillListsOpens(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "notes.txt", 10)
	entries, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: page.ID})
	if err != nil {
		t.Fatal(err)
	}
	path := entries.([]DownloadInfo)[0].Path
	if err := manager.ClearThreadDownloads(access); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.authorizeFile(access.Caller(), path); err != nil {
		t.Fatalf("a download the page lists is refused: %v", err)
	}
}

// The reviewer's sequence: another thread of the same workspace completes
// many downloads, and many other workspaces complete one each; a row the
// pane still lists opens throughout.
func TestAListedRowStaysOpenableWhateverOthersDownload(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "original.txt", 10)
	original := threadDownloads(manager, access)[0]

	busy := manager.Bind(Caller{ThreadID: "busy", Workspace: access.Workspace})
	busyPage, err := manager.Open(t.Context(), busy, "https://example.test/files", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		startDownload(t, manager, busy, busyPage, fmt.Sprintf("b%d.txt", i), 10)
	}
	for i := range 40 {
		elsewhere := manager.Bind(Caller{ThreadID: fmt.Sprintf("w%d", i), Workspace: t.TempDir()})
		other, err := manager.Open(t.Context(), elsewhere, "https://example.test/files", OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		startDownload(t, manager, elsewhere, other, "w.txt", 10)
		if err := manager.ClosePage(t.Context(), elsewhere, other.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.OpenThreadDownload(t.Context(), access, original.ID); err != nil {
		t.Fatalf("the listed download does not open after others downloaded: %v", err)
	}
}

// closeAllPages closes every page of the thread.
func (m *Manager) closeAllPages(t *testing.T, access Access) {
	t.Helper()
	m.mu.Lock()
	var ids []string
	for _, scope := range m.scopes {
		for id, p := range scope.pages {
			if p.owner == access.tb {
				ids = append(ids, id)
			}
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		if err := m.ClosePage(t.Context(), access, id); err != nil {
			t.Fatal(err)
		}
	}
}

// A scope with no artifact directory, and a download with no staged file,
// has no partial file of AO's to remove: the engine id is never taken as a
// path.
func TestRemovePartialDownloadNeedsAFileOfAOs(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			manager, _, _, _ := newDownloadManagerFor(t, mode.saving)
			t.Chdir(t.TempDir())
			if err := os.WriteFile("dl-1", []byte("not AO's"), 0o600); err != nil {
				t.Fatal(err)
			}
			manager.removePartialDownload(&jarScope{}, "dl-1")
			manager.removePartialDownload(nil, "dl-1")
			if _, err := os.Stat("dl-1"); err != nil {
				t.Fatalf("a download with no file of AO's removed one: %v", err)
			}
		})
	}
}

// What an engine that saves downloads itself reports must be the regular
// file at an absolute path, of the size it reported, to be openable.
func TestAnEngineSavedFileMustBeTheRegularFileItReported(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("a.bin", []byte("ten bytes!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engineSavedDownload("a.bin", 10); err == nil {
		t.Fatal("a relative saved path was accepted")
	}
	absolute, err := filepath.Abs("a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, file, err := engineSavedDownload(absolute, 10); err != nil || file == nil {
		t.Fatalf("the absolute path is refused: %v", err)
	}
	if _, _, err := engineSavedDownload(absolute, 0); err != nil {
		t.Fatalf("an unknown size is refused: %v", err)
	}
	if _, _, err := engineSavedDownload(absolute, 11); err == nil || !strings.Contains(err.Error(), "not the 11") {
		t.Fatalf("a file of another size answers %v", err)
	}
	if _, _, err := engineSavedDownload(filepath.Dir(absolute), 0); err == nil {
		t.Fatal("a folder was accepted as the saved file")
	}
	link := filepath.Join(filepath.Dir(absolute), "link.bin")
	if err := os.Symlink(absolute, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engineSavedDownload(link, 10); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Fatalf("a link answers %v", err)
	}
}
