//go:build unix

package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a creator that cut a worktree takes back when it fails after the
// cut: only what is still the cut's own.

// A pull-request thread whose record fails after its worktree was cut takes
// back only what is still the cut's: a checkout with a file written into it
// while it was being cut stays, with the pr/N branch it has checked out,
// and the refusal names both.
func TestOpenPRThreadRollbackKeepsACheckoutWithAChangeInIt(t *testing.T) {
	t.Parallel()
	c := newPRThreadCase(t)
	c.push(t, "refs/pull/5/head")
	execOnStore(t, c.dbPath, `CREATE TRIGGER refuse_pr_record BEFORE INSERT ON thread_pull_requests BEGIN SELECT RAISE(ABORT, 'injected record failure'); END`)
	started, release := gateCheckoutHook(t, c.repo)

	opened := make(chan error, 1)
	go func() {
		_, err := c.app.OpenPRThread(context.Background(), c.request("fork-work", true))
		opened <- err
	}()
	var checkout string
	select {
	case checkout = <-started:
	case err := <-opened:
		t.Fatalf("OpenPRThread ended before its checkout hook ran: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()

	err := <-opened
	if err == nil || !strings.Contains(err.Error(), "injected record failure") {
		t.Fatalf("OpenPRThread = %v, want the record's failure", err)
	}
	if body, readErr := os.ReadFile(filepath.Join(checkout, "notes.txt")); readErr != nil || string(body) != "mine\n" {
		t.Fatalf("the file written into the checkout = %q, %v; want it kept", body, readErr)
	}
	if !branchExists(c.repo, "pr/5") {
		t.Fatal("pr/5, checked out in the kept checkout, was deleted")
	}
	if !strings.Contains(err.Error(), "was kept") || !strings.Contains(err.Error(), "pr/5") {
		t.Fatalf("the refusal does not name the checkout and branch it kept: %v", err)
	}
	if threads := c.threads(t); len(threads) != 0 {
		t.Fatalf("threads left = %+v", threads)
	}
}

// A cut that carries the source's local changes and cannot apply them in
// the new checkout takes back only what is still the cut's: a file written
// into the checkout while it was being cut, which the apply refused to
// overwrite, stays with the checkout and its branch, and the refusal names
// them beside the stash that holds the carried changes.
func TestACarryThatCannotApplyKeepsACheckoutWithAChangeInIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("carried\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	started, release := gateCheckoutHook(t, repo)

	type result struct {
		path string
		err  error
	}
	cut := make(chan result, 1)
	go func() {
		path, _, err := app.cutWorktreeWithCarry(worktreeCutRequest{
			projectPath: repo, sourceWorkspace: repo, newBranch: "carried", carryLocalChanges: true,
		})
		cut <- result{path, err}
	}()
	var checkout string
	select {
	case checkout = <-started:
	case got := <-cut:
		t.Fatalf("the cut ended before its checkout hook ran: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(checkout, "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()

	got := <-cut
	if got.err == nil || !strings.Contains(got.err.Error(), "apply local changes") {
		t.Fatalf("cut = %+v, want the apply's failure", got)
	}
	if body, err := os.ReadFile(filepath.Join(checkout, "notes.txt")); err != nil || string(body) != "mine\n" {
		t.Fatalf("the file written into the checkout = %q, %v; want it kept", body, err)
	}
	if !branchExists(repo, "carried") {
		t.Fatal("the branch checked out in the kept checkout was deleted")
	}
	if !strings.Contains(got.err.Error(), "was kept") || !strings.Contains(got.err.Error(), "Branch carried") {
		t.Fatalf("the refusal does not name the checkout and branch it kept: %v", got.err)
	}
	if stashes := gitOutput(t, repo, "stash", "list"); !strings.Contains(stashes, "ao-carry-") {
		t.Fatalf("the stash holding the carried changes is gone: %q", stashes)
	}
}
