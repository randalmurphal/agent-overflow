package browser

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
		if theirs == "" && !placing(candidate) {
			theirs = candidate
			if err := os.WriteFile(candidate, []byte("theirs"), 0o600); err != nil {
				t.Errorf("take %s: %v", candidate, err)
			}
		}
		return linkFile(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = linkFile })
	startDownload(t, manager, access, page, "race.bin", 10)
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCompleted || got.Name != "race (1).bin" {
		t.Fatalf("a download whose name was taken lists as %+v, want race (1).bin", got)
	}
	if got, _ := os.ReadFile(filepath.Join(downloads, "race.bin")); string(got) != "theirs" || filepath.Base(theirs) != "race.bin" {
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
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
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
		if theirs == "" && !placing(candidate) {
			theirs = candidate
			if err := os.WriteFile(candidate, []byte("theirs"), 0o600); err != nil {
				t.Errorf("take %s: %v", candidate, err)
			}
		}
		return renameNoReplace(staged, candidate)
	}
	t.Cleanup(func() { linkDownload, renameDownload = linkFile, renameNoReplace })
	var staged string
	manager.engine.(*fakeEngine).beforeFinishReport = func(file string) { staged = file }
	startDownload(t, manager, access, page, "moved.bin", 10)
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
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
	workspace, err := canonicalRoot(access.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	dir := manager.workspaceDownloadDir(workspace)
	var theirs string
	linkDownload = func(source, candidate string) error {
		if theirs == "" && !placing(candidate) {
			theirs = candidate
			if err := os.WriteFile(candidate, []byte("theirs"), 0o600); err != nil {
				t.Errorf("take %s: %v", candidate, err)
			}
		}
		return linkFile(source, candidate)
	}
	t.Cleanup(func() { linkDownload = linkFile })
	startDownload(t, manager, access, page, "report.pdf", 10)
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if item.State != downloadStateCompleted || item.path != filepath.Join(dir, "report-1.pdf") {
		t.Fatalf("the download lists as %+v at %q, want completed at report-1.pdf", item.ThreadDownload, item.path)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "report.pdf")); string(got) != "theirs" || filepath.Base(theirs) != "report.pdf" {
		t.Fatalf("the file put at the name reads %q (taken %q)", got, theirs)
	}
	if got, _ := os.ReadFile(item.path); len(got) != 10 {
		t.Fatalf("the download's file holds %d bytes, want 10", len(got))
	}
	if names := folderNames(t, dir); len(names) != 2 {
		t.Fatalf("the download directory holds %q, want the two files and not the engine's", names)
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
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	if item.State != downloadStateFailed {
		t.Fatalf("the download lists as %+v, want failed", item.ThreadDownload)
	}
	manager.mu.Lock()
	var engineDir string
	for _, scope := range manager.scopes {
		engineDir = scope.downloadDir
	}
	manager.mu.Unlock()
	awaitRemoved(t, filepath.Join(engineDir, filepath.Base(string(item.engineID))))
	awaitCharged(t, manager, authority, workspaceBefore, artifactsBefore)
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
			t.Cleanup(func() { linkDownload, renameDownload = linkFile, renameNoReplace })
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
		if !placing(candidate) {
			close(linking)
			<-release
		}
		return linkFile(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = linkFile })
	startDownload(t, manager, access, page, "last.txt", 10, "hold")
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
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
	got, _ := manager.threadDownload(access, DownloadKey(item.ID))
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

// A page that leaves while its download's file is being published leaves
// that download to the publish, which settles it once, completed.
func TestAPageLeavingDuringThePublishLeavesItToFinish(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	linking, release := make(chan struct{}), make(chan struct{})
	linkDownload = func(staged, candidate string) error {
		if !placing(candidate) {
			close(linking)
			<-release
		}
		return linkFile(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = linkFile })
	startDownload(t, manager, access, page, "late.txt", 10, "hold")
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	profile := fakeProfileOf(t, manager)
	finished := make(chan error, 1)
	go func() { finished <- profile.finishHeld(item.engineID) }()
	<-linking
	log.take()
	if err := manager.closeLivePage(t.Context(), access.tb, PageID(page.ID)); err != nil {
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

// heldLinkingHere reports whether a placement on the filesystem of t's
// temporary directories, where the tests' Downloads folder and artifacts
// are, links the open, checked file itself: one probe of placeHeld, the
// call placeNoReplaceIn decides by.
func heldLinkingHere(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	held := openRootForTest(t, dir)
	info, err := held.Lstat("source")
	if err != nil {
		t.Fatal(err)
	}
	into := filepath.Join(dir, "into")
	if err := os.Mkdir(into, 0o700); err != nil {
		t.Fatal(err)
	}
	intoRoot := openRootForTest(t, into)
	_, _, err = placeHeld(placeSource{path: filepath.Join(dir, "source"), info: info, dir: held}, placeDir{path: into, root: intoRoot}, "the probe", func(int) string { return "placed" })
	if errors.Is(err, errNoHeldLink) {
		return false
	}
	if err != nil {
		t.Fatalf("probe the held link: %v", err)
	}
	return true
}

// openRootForTest opens dir held, closed with t.
func openRootForTest(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

// placeBy readies t for a placement by held link (held) or by name: a held
// link is skipped where heldLinkingHere finds none, and a placement by name
// is forced through placeHeldFile on a system that links held files.
func placeBy(t *testing.T, held bool) {
	t.Helper()
	if held {
		if !heldLinkingHere(t) {
			t.Skip("this filesystem cannot link a held file")
		}
		return
	}
	placeHeldFile = func(placeSource, placeDir, string, func(int) string) (string, os.FileInfo, error) {
		return "", nil, errNoHeldLink
	}
	t.Cleanup(func() { placeHeldFile = placeHeld })
}

// A staged file swapped for other bytes after the publish checked it never
// gives those bytes a public name. Linking the held file publishes the
// bytes it checked while they have a name anywhere, here one the swap kept
// them under, and leaves the swapped file and that name alone; with no
// name left it fails. Placing by name, by link or by rename, refuses the
// swap before it has a public name. A failed publish leaves the Downloads
// folder holding neither a file under the download's name nor a private
// name.
func TestAStagedFileSwappedAsItIsPublishedNeverHasAPublicName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		held      bool
		linking   bool
		keepAside bool
		reason    string
	}{
		{"held link, the checked file kept aside", true, true, true, ""},
		{"held link, the checked file's last name gone", true, true, false, "went before the download was saved"},
		{"by name, link", false, true, false, "changed before the download was saved"},
		{"by name, rename", false, false, false, "changed before the download was saved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placeBy(t, tc.held)
			manager, _, access, page := newDownloadManager(t)
			downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
			var staged, aside string
			manager.engine.(*fakeEngine).beforeFinishReport = func(file string) { staged = file }
			swapped := false
			move := func(real func(string, string) error) func(string, string) error {
				return func(from, to string) error {
					// The placement's first move, after its check of the
					// staged file.
					if !swapped {
						swapped = true
						if tc.keepAside {
							aside = filepath.Join(t.TempDir(), "kept")
							if err := os.Link(staged, aside); err != nil {
								t.Error(err)
							}
						}
						replacement := staged + ".swap"
						if err := os.WriteFile(replacement, []byte("not the download"), 0o600); err != nil {
							t.Error(err)
						}
						if err := os.Rename(replacement, staged); err != nil {
							t.Error(err)
						}
					}
					return real(from, to)
				}
			}
			if tc.linking {
				linkDownload = move(linkFile)
			} else {
				linkDownload = func(from, to string) error {
					return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EPERM}
				}
				renameDownload = move(renameNoReplace)
			}
			t.Cleanup(func() { linkDownload, renameDownload = linkFile, renameNoReplace })
			startDownload(t, manager, access, page, "published.txt", 10)
			got := threadDownloads(manager, access)[0]
			if tc.reason == "" {
				published := filepath.Join(downloads, "published.txt")
				data, err := os.ReadFile(published)
				if got.State != downloadStateCompleted || err != nil || !bytes.Equal(data, make([]byte, 10)) {
					t.Fatalf("the download lists as %+v and its file reads %q, %v, want it completed with the checked 10 bytes", got, data, err)
				}
				kept, err := os.Stat(aside)
				if current, statErr := os.Stat(published); err != nil || statErr != nil || !os.SameFile(kept, current) {
					t.Fatalf("the published file is not the checked one kept aside: %v, %v", err, statErr)
				}
				if names := folderNames(t, downloads); !slices.Equal(names, []string{stagingDirName, "published.txt"}) && !slices.Equal(names, []string{"published.txt"}) {
					t.Fatalf("the Downloads folder holds %q", names)
				}
				return
			}
			for _, name := range folderNames(t, downloads) {
				if name != stagingDirName {
					t.Fatalf("the Downloads folder holds %s", name)
				}
			}
			if got.State != downloadStateFailed || !strings.Contains(got.Error, tc.reason) {
				t.Fatalf("the download lists as %+v, want it failed saying %q", got, tc.reason)
			}
		})
	}
}

// A file put at the download's public name just after its link, before the
// publish checks that name, is not taken as the download, staged or an
// artifact: the row fails, the file put there stays as it is, and it is
// not granted as the download.
func TestAFileSwappedInAtThePublicNameIsNotTheDownload(t *testing.T) {
	for _, saving := range []downloadSaving{downloadsStaged, downloadsAsArtifacts} {
		for _, held := range []bool{true, false} {
			t.Run(fmt.Sprint(saving, ", held link ", held), func(t *testing.T) {
				placeBy(t, held)
				manager, _, access, page := newDownloadManagerFor(t, saving)
				dir := filepath.Join(os.Getenv("HOME"), "Downloads")
				if saving == downloadsAsArtifacts {
					dir = authorityOf(t, manager, access).downloadDir
				}
				public := ""
				linkDownload = func(from, to string) error {
					if err := linkFile(from, to); err != nil {
						return err
					}
					if public == "" && !placing(to) {
						public = filepath.Join(dir, filepath.Base(to))
						replacement := to + ".swap"
						if err := os.WriteFile(replacement, []byte("unrelated file"), 0o600); err != nil {
							t.Error(err)
						}
						if err := os.Rename(replacement, to); err != nil {
							t.Error(err)
						}
					}
					return nil
				}
				t.Cleanup(func() { linkDownload = linkFile })
				startDownload(t, manager, access, page, "report.txt", 10)
				got := threadDownloads(manager, access)[0]
				if got.State != downloadStateFailed || !strings.Contains(got.Error, "changed as the download was saved there") || got.Openable {
					t.Fatalf("the download lists as %+v, want it failed for the swap", got)
				}
				if data, err := os.ReadFile(public); err != nil || string(data) != "unrelated file" {
					t.Fatalf("the file put at %s reads %q, %v", public, data, err)
				}
				// The workspace's artifact directory, AO's own, opens by its
				// directory; a file in the Downloads folder only by its row.
				if _, err := manager.authorizeFile(access.Caller(), public); saving == downloadsStaged && err == nil {
					t.Fatal("the file put at the public name opens as the download")
				}
			})
		}
	}
}

// A publish that places by name gives the file its private name beside its
// source, in the run directory for a staged download and in the engine's
// directory for an artifact, where the run's prune or the artifact prune
// finds it if the publish never finishes; never in the folder it is
// published to. One that links the held file makes no private name, and its
// public name holds the file it checked. Placing by name has its limit: a
// file swapped in at the private name after its check takes the public
// name, and then the row fails and never takes that file as the download.
// Each placement runs where the system offers it, a placement by name
// everywhere.
func TestAPublishMakesItsPrivateNameBesideItsSource(t *testing.T) {
	for _, tc := range []struct {
		saving downloadSaving
		held   bool
	}{{downloadsStaged, true}, {downloadsStaged, false}, {downloadsAsArtifacts, true}, {downloadsAsArtifacts, false}} {
		placement := map[bool]string{true: "held link", false: "by name"}[tc.held]
		t.Run(fmt.Sprint(tc.saving, ", ", placement), func(t *testing.T) {
			saving := tc.saving
			placeBy(t, tc.held)
			manager, _, access, page := newDownloadManagerFor(t, saving)
			var private, source, swappedAt string
			linkDownload = func(from, to string) error {
				switch {
				case placing(to):
					private, source = to, from
				case private != "" && from == private && swappedAt == "":
					// The public link of a placement by name, after the
					// private name's check: another file takes that name.
					swappedAt = to
					replacement := private + ".swap"
					if err := os.WriteFile(replacement, []byte("not the download"), 0o600); err != nil {
						t.Error(err)
					}
					if err := os.Rename(replacement, private); err != nil {
						t.Error(err)
					}
				}
				return linkFile(from, to)
			}
			t.Cleanup(func() { linkDownload, renameDownload = linkFile, renameNoReplace })
			startDownload(t, manager, access, page, "held.txt", 10)
			item, err := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
			if err != nil {
				t.Fatal(err)
			}
			if byName := private != ""; byName == tc.held {
				t.Fatalf("placed by name: %v, want the %s", byName, placement)
			}
			if !tc.held {
				if item.State != downloadStateFailed || !strings.Contains(item.Error, "changed as the download was saved there") || item.Openable {
					t.Fatalf("the download whose private name was swapped lists as %+v, want it failed", item.ThreadDownload)
				}
				swapped, err := os.Lstat(swappedAt)
				if err != nil {
					t.Fatalf("the file swapped in left the public name: %v", err)
				}
				if item.file != nil && os.SameFile(item.file, swapped) {
					t.Fatal("the row took the swapped file as the download")
				}
			} else {
				if item.State != downloadStateCompleted {
					t.Fatalf("the held link lists as %+v", item.ThreadDownload)
				}
				published, err := os.Lstat(item.path)
				if err != nil || item.file == nil || !os.SameFile(published, item.file) {
					t.Fatalf("the public name %s does not hold the file the row checked: %v", item.path, err)
				}
				if data, err := os.ReadFile(item.path); err != nil || !bytes.Equal(data, make([]byte, 10)) {
					t.Fatalf("the public name holds %q, %v, want the download's bytes", data, err)
				}
			}

			// Placing by rename, where links are refused, on every platform.
			private, source = "", ""
			linkDownload = func(from, to string) error {
				return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EPERM}
			}
			renameDownload = func(from, to string) error {
				if placing(to) {
					private, source = to, from
				}
				return renameNoReplace(from, to)
			}
			startDownload(t, manager, access, page, "report.txt", 10)
			item, _ = manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
			if item.State != downloadStateCompleted {
				t.Fatalf("the download lists as %+v", item.ThreadDownload)
			}
			if private == "" || filepath.Dir(private) != filepath.Dir(source) || filepath.Dir(private) == filepath.Dir(item.path) {
				t.Fatalf("the private name %s is not beside the source %s, apart from %s", private, source, filepath.Dir(item.path))
			}
			if saving == downloadsStaged && !runDirPattern.MatchString(filepath.Base(filepath.Dir(private))) {
				t.Fatalf("the private name %s is in no run directory", private)
			}
			if _, err := os.Lstat(private); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the private name %s stayed: %v", private, err)
			}
		})
	}
}
