//go:build !windows

package gitroot

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO under one of the pointer names blocks os.Open until somebody opens
// the other end — forever, inside a session-import scan that has no way to
// cancel it, wedging the listing and every thread creation behind it. Every
// pointer read stats first, so each name degrades exactly as an absent file
// would.
//
// The layouts here are only reachable on unix; Windows has no mkfifo.
func TestPointerReadsRefuseAFIFO(t *testing.T) {
	tmp := t.TempDir()
	repo := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "repo")))

	// A FIFO where the registration's back-pointer belongs: unreadable is
	// unconfirmed, so the worktree answers for itself.
	unregistered := mkdirAll(t, filepath.Join(tmp, "unregistered"))
	unregisteredPrivate := mkdirAll(t, filepath.Join(repo, gitDirName, "worktrees", "unregistered"))
	writeFile(t, filepath.Join(unregistered, gitDirName), "gitdir: "+unregisteredPrivate+"\n")
	writeFile(t, filepath.Join(unregisteredPrivate, "commondir"), "../..\n")
	mkfifo(t, filepath.Join(unregisteredPrivate, "gitdir"))

	// A FIFO where `commondir` belongs, with the back-pointer intact: the
	// pre-2.13 marker fallback still names the repository off the gitdir path.
	noCommonDir := linkedWorktree(t, repo, "nocommondir",
		mkdirAll(t, filepath.Join(tmp, "nocommondir")), "")
	mkfifo(t, filepath.Join(repo, gitDirName, "worktrees", "nocommondir", "commondir"))

	// A FIFO as the `.git` entry itself: neither a directory nor a regular
	// file, so the entry is unusable and the walk stops there.
	fifoGitDir := mkdirAll(t, filepath.Join(tmp, "fifo-gitdir"))
	mkfifo(t, filepath.Join(fifoGitDir, gitDirName))

	cases := []struct {
		name string
		in   string
		out  string
	}{
		{name: "registration back-pointer", in: unregistered, out: unregistered},
		{name: "commondir", in: noCommonDir, out: repo},
		{name: "the .git entry itself", in: fifoGitDir, out: fifoGitDir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				root string
				ok   bool
			)
			mustReturn(t, func() { root, ok = MainRoot(tc.in) })
			if !ok || root != want(t, tc.out) {
				t.Fatalf("MainRoot(%q) = %q, %v; want %q", tc.in, root, ok, want(t, tc.out))
			}
		})
	}

	// The registry read reaches the same guard from the other side: the FIFO
	// registration is skipped and the readable ones still list.
	var (
		paths []string
		err   error
	)
	mustReturn(t, func() { paths, err = RegisteredWorktrees(repo) })
	if err != nil {
		t.Fatalf("RegisteredWorktrees: %v", err)
	}
	if len(paths) != 1 || paths[0] != filepath.Clean(noCommonDir) {
		t.Fatalf("RegisteredWorktrees = %v, want only the readable registration %q", paths, noCommonDir)
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo %s: %v", path, err)
	}
}

// mustReturn fails instead of letting a blocked read hang the whole package
// run to the test binary's timeout — a wedge is the defect under test, so it
// has to be reported as one.
func mustReturn(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pointer read blocked on a FIFO — the scan has no way to cancel this")
	}
}
