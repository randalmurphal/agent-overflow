package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
)

// historyPull is a pull updated hours ago, created an hour before.
func historyPull(number int, title, head, state string, hours int) forgefake.Pull {
	at := func(h int) string { return time.Now().UTC().Add(-time.Duration(h) * time.Hour).Format(time.RFC3339) }
	return forgefake.Pull{Number: number, Title: title, HeadRef: head, State: state, Author: "user1", AuthorName: "User 1",
		CreatedAt: at(hours + 1), UpdatedAt: at(hours)}
}

// listWorkBoard makes projectID list the fixture's board.
func (f workSearchFixture) listWorkBoard(t *testing.T, projectID string) {
	t.Helper()
	if _, err := f.a.SetProjectTrackerBoards(f.ctx, projectID, nil, []trackerapp.ProjectBoard{
		{Connection: f.conn.ID, Board: "1", OpensFirst: true, Name: "WEB board", TrackerProject: "WEB"},
	}); err != nil {
		t.Fatal(err)
	}
}

// historyOps counts the forge requests after from by route.
func (f workSearchFixture) historyOps(from int) (map[string]int, int) {
	ops, last := f.forgeOps(from)
	out := map[string]int{}
	for _, op := range ops {
		out[op]++
	}
	return out, last
}

// at moves the forge history's clock to d past now.
func (f workSearchFixture) at(d time.Duration) {
	now := time.Now().Add(d)
	f.a.forgeHistory.now = func() time.Time { return now }
}

func (f workSearchFixture) sources(t *testing.T) []store.ForgeHistorySource {
	t.Helper()
	all, err := f.a.store.ListForgeHistorySources()
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func sourceKeys(sources []store.ForgeHistorySource) []string {
	var out []string
	for _, s := range sources {
		out = append(out, strings.TrimSuffix(s.RepoKey+" "+s.KeepHead, " "))
	}
	return out
}

// The linked loop's round reads the history of each repository a project
// of an enabled connection clones, on both forges, and again only once
// its last read is 15 minutes old; turning the connection off drops the
// history at once.
func TestForgeHistoryReadsTheRepositoriesOfEnabledConnections(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
			historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2), historyPull(2, "Cache the session", "cache", "open", 3)}},
		{Forge: "gitlab", Project: "grp/api", Pulls: []forgefake.Pull{historyPull(4, "Rework the API", "WEB-1-api", "closed", 5)}},
	}})
	api := forgeProjectRepo(t, f.a, "https://gitlab.com/grp/api.git")
	f.listWorkBoard(t, api)
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceKeys(f.sources(t)); !slices.Equal(got, []string{"github:example/web", "gitlab:grp/api"}) {
		t.Fatalf("sources = %v", got)
	}
	for _, s := range f.sources(t) {
		if !s.Walked || s.ReadAt == 0 || s.Failure != "" || s.OwnHead == "" {
			t.Errorf("source %s after its first read: %+v", s.RepoKey, s)
		}
	}
	rows, err := f.a.store.ForgeHistoryByKey("WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, r := range rows {
		listed = append(listed, fmt.Sprintf("%s:%d %s", r.Source.RepoKey, r.Number, r.State))
	}
	if !slices.Equal(listed, []string{"github:example/web:1 merged", "gitlab:grp/api:4 closed"}) {
		t.Fatalf("WEB-1's history = %v", listed)
	}
	_, from := f.historyOps(0)
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(from); len(ops) != 0 {
		t.Fatalf("a round 0 minutes after the last read asked %v", ops)
	}
	f.at(16 * time.Minute)
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(from); ops["gh graphql PullHistory"] != 1 || ops["glab api merge request list"] != 1 {
		t.Fatalf("a round 16 minutes after the last read asked %v; want one read of each repository", ops)
	}
	if _, err := f.a.SetTrackerConnectionEnabled(f.ctx, f.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := f.sources(t); len(got) != 0 {
		t.Fatalf("turning the connection off left sources %v", sourceKeys(got))
	}
	if rows, err := f.a.store.ForgeHistoryByKey("WEB-1"); err != nil || len(rows) != 0 {
		t.Fatalf("turning the connection off left WEB-1's history %+v, %v", rows, err)
	}
}

// A ticket's Work lists the stored history's pull requests that name its
// key, folds a branch into the one it heads, and asks the forge nothing:
// GetTicketWork never, SearchTicketWork when the last read is younger
// than a minute and covers the ticket; older, it reads the repository
// once and searches nothing.
func TestTicketWorkListsTheStoredHistory(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2), historyPull(3, "Move the host", "WEB-1-cache", "closed", 3),
		historyPull(4, "WEB-12 Other", "other", "open", 1),
	}}}})
	runGit(t, f.dir, "branch", "WEB-1-cache")
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	_, from := f.historyOps(0)
	want := []string{"github:example/web:1 merged", "github:example/web:3 closed"}
	for name, read := range map[string]func() (TicketWork, error){
		"GetTicketWork":    func() (TicketWork, error) { return f.a.GetTicketWork(f.ctx, f.conn.ID, "10101") },
		"SearchTicketWork": func() (TicketWork, error) { return f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101") },
	} {
		work, err := read()
		if err != nil {
			t.Fatal(err)
		}
		if got := workPulls(work); !slices.Equal(got, want) || len(work.Branches) != 0 || len(work.ForgeProblems) != 0 {
			t.Errorf("%s = %v, branches %+v, problems %+v; want %v with WEB-1-cache folded", name, got, work.Branches, work.ForgeProblems, want)
		}
		if at := slices.IndexFunc(work.PullRequests, func(pr TicketPullRequest) bool { return pr.Ref.Number == 1 }); at < 0 ||
			work.PullRequests[at].Author != "User 1" || work.PullRequests[at].ProjectID != f.project || work.PullRequests[at].ReviewProjectID != f.project {
			t.Errorf("%s lists 1 as %+v", name, work.PullRequests)
		}
	}
	if ops, _ := f.historyOps(from); len(ops) != 0 {
		t.Fatalf("Work within a minute of the last read asked %v", ops)
	}
	f.at(2 * time.Minute)
	if _, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101"); err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(from); len(ops) != 1 || ops["gh graphql PullHistory"] != 1 {
		t.Fatalf("SearchTicketWork 2 minutes after the last read asked %v; want one history read", ops)
	}
}

// A repository whose history has not read back to the ticket's creation
// is searched by the key, as one with no history is.
func TestSearchTicketWorkSearchesWhereTheHistoryDoesNotReach(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2),
	}}}})
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	_, created, err := f.a.mirroredTicket(f.conn.ID, "10101")
	if err != nil || created == 0 {
		t.Fatalf("the ticket's creation = %d, %v", created, err)
	}
	src := f.sources(t)[0]
	now := time.Now()
	if err := f.a.store.ApplyForgeHistory(store.ForgeHistoryRead{Source: src.ForgeHistoryRef, Walk: src.Walk,
		Reach: created - store.ForgeHistoryClockMargin.Milliseconds() + 1, ReadAt: now.UnixMilli(), WindowStart: forgeHistoryWindowStart(now).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	_, from := f.historyOps(0)
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(from); ops["gh graphql PullsByKey"] != 1 || ops["gh graphql PullHistory"] != 0 {
		t.Fatalf("SearchTicketWork asked %v; want the key search and no history read", ops)
	}
	if got := workPulls(work); !slices.Equal(got, []string{"github:example/web:1 merged"}) {
		t.Errorf("pull requests = %v", got)
	}
}

// A repository whose last history read failed shows that failure, in
// the Pull requests page's terms, beside its stored rows, and is not
// searched.
func TestTicketWorkShowsAFailedHistoryReadWithoutSearching(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2),
	}}}})
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullHistory", Status: 403, Times: 1,
		Header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Resource": "graphql"}}); err != nil {
		t.Fatal(err)
	}
	f.at(16 * time.Minute)
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatalf("a round whose read the forge refused = %v; want the failure on the source", err)
	}
	_, from := f.historyOps(0)
	for name, read := range map[string]func() (TicketWork, error){
		"GetTicketWork":    func() (TicketWork, error) { return f.a.GetTicketWork(f.ctx, f.conn.ID, "10101") },
		"SearchTicketWork": func() (TicketWork, error) { return f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101") },
	} {
		work, err := read()
		if err != nil {
			t.Fatal(err)
		}
		if len(work.ForgeProblems) != 1 {
			t.Fatalf("%s problems = %+v; want the history's", name, work.ForgeProblems)
		}
		p := work.ForgeProblems[0]
		if resume, err := time.Parse(time.RFC3339, p.ResumeAt); !p.History || p.ErrorKind != forgeFailureRateLimited || p.ProjectID != f.project ||
			p.Repo.Key() != "github:example/web" || p.ErrorDetails == nil || p.FailedAt == 0 || err != nil || !resume.After(time.Now()) {
			t.Errorf("%s problem = %+v", name, p)
		}
		if got := workPulls(work); !slices.Equal(got, []string{"github:example/web:1 merged"}) {
			t.Errorf("%s pull requests = %v; want the stored row beside the failure", name, got)
		}
	}
	if ops, _ := f.historyOps(from); len(ops) != 0 {
		t.Fatalf("Work of a failed repository within a minute of its read asked %v", ops)
	}
}

// SearchTicketWork's read of a repository whose last read failed, once
// that read is a minute old, clears the failure when it lands.
func TestALandedHistoryReadClearsItsFailure(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2),
	}}}})
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullHistory", Status: 502, Times: 1, Body: json.RawMessage(`{"message":"down"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.ForgeProblems) != 1 || !work.ForgeProblems[0].History || work.ForgeProblems[0].ErrorKind != forgeFailureForge || len(work.PullRequests) != 0 {
		t.Fatalf("Work after a failed first read = %v, problems %+v; want the failure alone", workPulls(work), work.ForgeProblems)
	}
	_, from := f.historyOps(0)
	f.at(2 * time.Minute)
	work, err = f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.ForgeProblems) != 0 || !slices.Equal(workPulls(work), []string{"github:example/web:1 merged"}) {
		t.Errorf("Work after a read that landed = %v, problems %+v", workPulls(work), work.ForgeProblems)
	}
	if ops, _ := f.historyOps(from); len(ops) != 1 || ops["gh graphql PullHistory"] != 1 {
		t.Errorf("SearchTicketWork asked %v; want one history read and no search", ops)
	}
}

// A fork's parent is read for the pull requests whose head is the fork,
// from the round after the fork's first read: Work lists the parent's
// pull request the fork's branch heads under the fork's project with the
// branch folded into it, and not one of another head, and searches
// nothing.
func TestTicketWorkReadsAForksParentForTheForksPullRequests(t *testing.T) {
	t.Parallel()
	other := historyPull(5, "WEB-1 Another head", "WEB-1-other", "open", 1)
	fromFork := historyPull(2, "Fix login", "WEB-1-login", "merged", 2)
	fromFork.HeadRepo = "alice/up"
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "github", Project: "team/up", Pulls: []forgefake.Pull{fromFork, other}},
		{Forge: "github", Project: "alice/up", Parent: "team/up"},
	}})
	fork := forgeProjectRepo(t, f.a, "https://github.com/alice/up.git")
	f.listWorkBoard(t, fork)
	p, err := f.a.store.GetProject(fork)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, p.Path, "branch", "WEB-1-login")
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceKeys(f.sources(t)); !slices.Equal(got, []string{"github:alice/up", "github:example/web"}) {
		t.Fatalf("sources after the first round = %v", got)
	}
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	sources := f.sources(t)
	head := sources[0].OwnHead
	if got := sourceKeys(sources); len(got) != 3 || got[2] != "github:team/up "+head {
		t.Fatalf("sources after the second round = %v; want team/up keeping alice/up's head %q", got, head)
	}
	_, from := f.historyOps(0)
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := workPulls(work); !slices.Equal(got, []string{"github:team/up:2 merged"}) || len(work.Branches) != 0 || len(work.ForgeProblems) != 0 {
		t.Fatalf("Work = %v, branches %+v, problems %+v; want team/up's 2 with WEB-1-login folded", got, work.Branches, work.ForgeProblems)
	}
	if work.PullRequests[0].ProjectID != fork {
		t.Errorf("team/up's 2 is listed under %q; want the fork's project", work.PullRequests[0].ProjectID)
	}
	if ops, _ := f.historyOps(from); len(ops) != 0 {
		t.Errorf("Work the history covers asked %v", ops)
	}
}

// While git cannot read the origin of a project of an enabled connection,
// the round removes no source, since that origin may be one of them, and
// says why; once that project is gone, the sources no project names go.
func TestForgeHistoryKeepsEverySourceWhileAnOriginIsUnread(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}, {Forge: "github", Project: "example/old"}}})
	old := store.ForgeHistoryRef{RepoKey: "github:example/old", Forge: "github", Host: "github.com", Namespace: "example", Repo: "old"}
	if _, err := f.a.store.SetForgeHistorySources([]store.ForgeHistoryRef{old}); err != nil {
		t.Fatal(err)
	}
	broken := store.Project{ID: "project-broken", Path: t.TempDir(), Name: "broken", CreatedAt: 1, UpdatedAt: 1}
	if _, err := f.a.store.CreateProject(broken); err != nil {
		t.Fatal(err)
	}
	f.listWorkBoard(t, broken.ID)
	err := f.a.readForgeHistory(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("a round with an unread origin = %v; want it named", err)
	}
	if got := sourceKeys(f.sources(t)); !slices.Equal(got, []string{"github:example/old", "github:example/web"}) {
		t.Fatalf("sources = %v; want example/old kept beside example/web", got)
	}
	if err := f.a.store.DeleteProject(broken.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceKeys(f.sources(t)); !slices.Equal(got, []string{"github:example/web"}) {
		t.Fatalf("sources = %v; want example/web alone", got)
	}
}

// Two reads of one source never run at once: the second waits for the
// first and reads nothing when the first left it fresh.
func TestForgeHistoryReadsOneSourceOnceAtATime(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	if _, _, err := f.a.syncForgeHistorySources(f.ctx); err != nil {
		t.Fatal(err)
	}
	ref := f.sources(t)[0].ForgeHistoryRef
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullHistory", Status: 502, Times: 1, Hold: true,
		Body: json.RawMessage(`{"message":"held"}`)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Go(func() { errs[0] = f.a.readForgeHistorySource(f.ctx, ref, forgeHistoryCatchUp) })
	deadline := time.Now().Add(20 * time.Second)
	for {
		if ops, _ := f.historyOps(0); ops["gh graphql PullHistory"] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first read never asked")
		}
		time.Sleep(time.Millisecond)
	}
	wg.Go(func() { errs[1] = f.a.readForgeHistorySource(f.ctx, ref, forgeHistoryCatchUp) })
	// Unlocked, the second read would ask within this time.
	time.Sleep(200 * time.Millisecond)
	if err := f.engine.ReleaseRefusal(forgefake.Release{Route: "gh graphql PullHistory"}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("reads = %v", errs)
	}
	if ops, _ := f.historyOps(0); ops["gh graphql PullHistory"] != 1 {
		t.Fatalf("two reads of one source asked %v; want one request", ops)
	}
}
