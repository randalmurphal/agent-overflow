package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// gitCalls records every git process the app starts while the test runs,
// by its working directory and arguments, through a git on PATH that logs
// and runs the real one. Serial: it sets PATH.
func gitCalls(t *testing.T) func() []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the logging git is a shell script")
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "git.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\t%%s\\n' \"$(pwd -P)\" \"$*\" >> %q\nexec %q \"$@\"\n", log, real)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
}

// Which of the computer's projects can show a pull request is read from
// the origins Work already read for the projects it covers; the other
// projects' origins are read only for a row none of those clones, as
// many as it takes to find one. Counted with those origins not cached,
// over what a ticket's open sends: GetTicketWork, then SearchTicketWork.
func TestTicketWorkReadsOtherProjectsOriginsOnlyForARowNoWorkProjectClones(t *testing.T) {
	calls := gitCalls(t)
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "github", Project: "team/up", Pulls: []forgefake.Pull{{Number: 2, Title: "Fix login", HeadRef: "WEB-1-login", HeadRepo: "alice/up"}}},
		{Forge: "github", Project: "alice/up", Parent: "team/up"},
	}})
	others := map[string]bool{}
	for i := range 10 {
		p, err := f.a.store.GetProject(forgeProjectRepo(t, f.a, fmt.Sprintf("https://github.com/example/other-%d.git", i)))
		if err != nil {
			t.Fatal(err)
		}
		dir, err := filepath.EvalSymlinks(p.Path)
		if err != nil {
			t.Fatal(err)
		}
		others[dir] = true
	}
	// otherReads are the git processes and origin reads run in the other
	// projects by open, with their origins not cached, as the first open
	// in repoMetaTTL finds them.
	otherReads := func(open func()) (procs, origins int) {
		for dir := range others {
			f.a.gitCore().InvalidateForgeCache(dir)
		}
		before := len(calls())
		open()
		for _, c := range calls()[before:] {
			dir, args, _ := strings.Cut(c, "\t")
			if others[dir] {
				procs++
				if args == "remote get-url origin" {
					origins++
				}
			}
		}
		return procs, origins
	}
	open := func() {
		t.Helper()
		if _, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101"); err != nil {
			t.Fatal(err)
		}
	}

	if procs, origins := otherReads(open); procs != 0 {
		t.Errorf("a ticket with no rows ran %d git processes (%d origin reads) in the other projects; want none", procs, origins)
	}
	storePRList(t, f.a, f.project, workSearchRepo, []gitops.PRListRow{
		prListRow(workSearchRepo, 1, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = "WEB-1 Fix the login", "fix-login" }),
	}, time.Minute)
	if procs, origins := otherReads(open); procs != 0 {
		t.Errorf("a ticket whose row a Work project clones ran %d git processes (%d origin reads) in the other projects; want none", procs, origins)
	}

	// The fork's branch finds team/up's 2, which no project Work covers
	// clones: the other projects are read until one does.
	fork := forgeProjectRepo(t, f.a, "https://github.com/alice/up.git")
	fp, err := f.a.store.GetProject(fork)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, fp.Path, "branch", "WEB-1-login")
	f.linkThread(t, "t-fork", fork, fp.Path)
	parent := forgeProjectRepo(t, f.a, "https://github.com/team/up.git")
	var work TicketWork
	procs, origins := otherReads(func() {
		if work, err = f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101"); err != nil {
			t.Fatal(err)
		}
	})
	at := slices.IndexFunc(work.PullRequests, func(pr TicketPullRequest) bool { return pr.Ref.Key() == "github:team/up:2" })
	if at < 0 || work.PullRequests[at].ReviewProjectID != parent {
		t.Errorf("pull requests %+v; want team/up's 2 shown by %s", work.PullRequests, parent)
	}
	if origins == 0 || origins > len(others) {
		t.Errorf("a row no Work project clones read %d of the %d other projects' origins; want some, at most all", origins, len(others))
	}
	t.Logf("a row no Work project clones: %d git processes, %d origin reads in the %d other projects", procs, origins, len(others))
}
