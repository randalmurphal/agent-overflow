package app

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// What a ticket's Work and the pull request rows' tickets cost at the size
// of a busy computer: 2,000 stored pull requests over 10 repositories and a
// project of 500 branches. Work scans every stored list for the key and
// decodes only the lists that hold it; a list's landing reads its rows'
// tickets in one statement. The numbers are logged (go test -v) and the
// allocations bounded, so a change that decodes every list on every open,
// or reads per row, fails here.
//
// Serial: it measures the process's allocations.
func TestTicketWorkCostAtRealSize(t *testing.T) {
	if testing.Short() {
		t.Skip("measures at real size")
	}
	f := newTrackerBoardFixture(t)
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})
	f.subscribe(t, ctx)
	f.boardEvent(t, "for the first read", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10101") })

	project, err := f.a.store.GetProject(f.project)
	if err != nil {
		t.Fatal(err)
	}
	dir := project.Path
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	writeFile(t, dir, ".gitkeep", "")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	// 500 branches in one git process; three name WEB-1.
	var refs strings.Builder
	for i := range 500 {
		name := fmt.Sprintf("feature/OPS-%d-change-the-thing", i)
		if i%200 == 7 {
			name = fmt.Sprintf("WEB-1-attempt-%d", i)
		}
		fmt.Fprintf(&refs, "create refs/heads/%s HEAD\n", name)
	}
	update := exec.Command("git", "update-ref", "--stdin")
	update.Dir = dir
	update.Stdin = strings.NewReader(refs.String())
	if out, err := update.CombinedOutput(); err != nil {
		t.Fatalf("git update-ref: %v: %s", err, out)
	}

	// 10 repositories of 200 rows; WEB-1 is in one of them (typical) or in
	// every one (worst).
	store := func(every bool) {
		for r := range 10 {
			repo := gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: fmt.Sprintf("repo-%d", r)}
			rows := make([]gitops.PRListRow, 200)
			for i := range rows {
				rows[i] = prListRow(repo, i+1, func(row *gitops.PRListRow) {
					row.Title = fmt.Sprintf("OPS-%d Change how the sync loop reads the cursor after a retry of the read %d", i, i)
					row.HeadRefName = fmt.Sprintf("feature/OPS-%d-sync-cursor", i)
					if i == 3 && (every || r == 0) {
						row.Title = "WEB-1 Retry cursor"
					}
				})
			}
			storePRList(t, f.a, f.project, repo, rows, time.Minute)
		}
	}

	measure := func(name string, run func() error, maxBytes int64) {
		t.Helper()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if err := run(); err != nil {
					b.Fatal(err)
				}
			}
		})
		t.Logf("%s: %s, %d B/op, %d allocs/op", name, time.Duration(result.NsPerOp()), result.AllocedBytesPerOp(), result.AllocsPerOp())
		if got := result.AllocedBytesPerOp(); got > maxBytes {
			t.Errorf("%s allocated %d bytes an open; want at most %d", name, got, maxBytes)
		}
	}
	work := func() error {
		w, err := f.a.GetTicketWork(ctx, f.conn.ID, "10101")
		if err == nil && (len(w.PullRequests) == 0 || len(w.Branches) != 3) {
			err = fmt.Errorf("work = %d pull requests, %d branches; want some and 3", len(w.PullRequests), len(w.Branches))
		}
		return err
	}

	measure("the 500-branch listing alone", func() error {
		_, err := f.a.gitCore().ListBranches(dir)
		return err
	}, 2<<20)
	// 30 other projects: clones of the 10 stored repositories and 20
	// more. Work reads every project's origin only for an answer that
	// lists a pull request, to name the one that can show each.
	for i := range 30 {
		forgeProjectRepo(t, f.a, fmt.Sprintf("https://github.com/example/repo-%d.git", i))
	}
	origins := func() error {
		_, err := f.a.forgeProjects(ctx)
		return err
	}
	// The fixture builds a git core per call, so these two reads have
	// nothing cached, as the first read in repoMetaTTL does: an answer
	// with no pull request reads no other project's origin. A running
	// app's one core caches each origin, which the reads after use.
	measure("the 31 projects' origins, nothing cached", origins, 4<<20)
	measure("Work, no pull request, nothing cached", func() error {
		w, err := f.a.GetTicketWork(ctx, f.conn.ID, "10101")
		if err == nil && (len(w.PullRequests) != 0 || len(w.Branches) != 3) {
			err = fmt.Errorf("work = %d pull requests, %d branches; want none and 3", len(w.PullRequests), len(w.Branches))
		}
		return err
	}, 1<<20)
	f.a.git = f.a.gitCore()
	measure("the 31 projects' origins, cached", origins, 1<<20)
	store(false)
	// The stored lists are read whole from the store (about 3 MB here);
	// only the list that holds the key is decoded.
	measure("Work, WEB-1 in one of 10 stored lists", work, 6<<20)
	store(true)
	measure("Work, WEB-1 in every stored list", work, 12<<20)

	rows := make([]gitops.PRListRow, 200)
	for i := range rows {
		rows[i] = prListRow(prListRepoA, i+1, func(row *gitops.PRListRow) {
			row.Title = fmt.Sprintf("WEB-%d Change how the sync loop reads the cursor", i+1)
			row.HeadRefName = fmt.Sprintf("web-%d-sync-cursor", i+1)
		})
	}
	measure("tickets of a 200-row landing", func() error {
		tickets := f.a.prRowTickets(prListRepoA.Key(), f.project, rows)
		if len(tickets[0]) != 1 || tickets[0][0].IssueID != "10101" {
			return fmt.Errorf("row 1 tickets = %+v; want WEB-1's issue", tickets[0])
		}
		return nil
	}, 1<<20)
}
