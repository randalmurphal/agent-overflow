package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
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
		out = append(out, s.RepoKey)
	}
	return out
}

// historyRows are the stored history's pull requests naming key in the
// sources of origins and their parents, as "repo:number state".
func (f workSearchFixture) historyRows(t *testing.T, key string, origins ...string) []string {
	t.Helper()
	work, err := f.a.store.ForgeHistoryForWork(key, origins, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[int64]string{}
	for _, s := range work.Sources {
		keys[s.ID] = s.RepoKey
	}
	var out []string
	for _, r := range work.Rows {
		out = append(out, fmt.Sprintf("%s:%d %s", keys[r.SourceID], r.Number, r.State))
	}
	slices.Sort(out)
	return out
}

// The linked loop's round reads the history of each repository a project
// of an enabled connection clones, on both forges, and again only once
// its last read is 15 minutes old and it owes nothing; turning the
// connection off, or removing it, drops the history at once.
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
		if s.Owed || !s.HasReach || s.Reach != 0 || s.ReadAt == 0 || s.Failure != "" || s.OwnHead == "" {
			t.Errorf("source %s after its first read: %+v", s.RepoKey, s)
		}
	}
	listed := f.historyRows(t, "WEB-1", "github:example/web", "gitlab:grp/api")
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
	if rows := f.historyRows(t, "WEB-1", "github:example/web", "gitlab:grp/api"); len(rows) != 0 {
		t.Fatalf("turning the connection off left WEB-1's history %v", rows)
	}
	if _, err := f.a.SetTrackerConnectionEnabled(f.ctx, f.conn.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := sourceKeys(f.sources(t)); len(got) != 2 {
		t.Fatalf("turning the connection on made sources %v", got)
	}
	if err := f.a.RemoveTrackerConnection(f.ctx, f.conn.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.sources(t); len(got) != 0 {
		t.Fatalf("removing the connection left sources %v", sourceKeys(got))
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
	if ops, _ := f.historyOps(from); ops["gh graphql PullHistory"] != 1 || ops["gh graphql PullsByKey"] != 0 {
		t.Fatalf("SearchTicketWork 2 minutes after the last read asked %v; want one history read and no search", ops)
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
	if _, err := f.a.store.ApplyForgeHistory(store.ForgeHistoryRead{SourceID: src.ID, Source: src.ForgeHistoryRef, OwnHead: src.OwnHead, Walk: src.Walk,
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
	if ops, _ := f.historyOps(from); ops["gh graphql PullHistory"] != 1 || ops["gh graphql PullsByKey"] != 0 {
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
	if got := sourceKeys(f.sources(t)); !slices.Equal(got, []string{"github:alice/up", "github:example/web", "github:team/up"}) {
		t.Fatalf("sources after the second round = %v; want team/up beside them", got)
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
	if _, _, err := f.a.store.SetForgeHistorySources([]store.ForgeHistoryRef{old}, nil); err != nil {
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

// Two reads of one source never run at once: a second waits for the
// first only within its own context, and once the first landed, a second
// that wants a fresh read joins it and reads nothing.
func TestForgeHistoryReadsOneSourceOnceAtATime(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	if _, _, err := f.a.syncForgeHistorySources(f.ctx); err != nil {
		t.Fatal(err)
	}
	id := f.sources(t)[0].ID
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullHistory", Status: 502, Times: 1, Hold: true,
		Body: json.RawMessage(`{"message":"held"}`)}); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- f.a.readForgeHistorySource(f.ctx, id, forgeHistoryStale) }()
	f.waitForHistoryOp(t, "gh graphql PullHistory", 1)
	// The refusal holds one request: unlocked, this read would ask and be
	// answered.
	ctx, cancel := context.WithTimeout(f.ctx, 300*time.Millisecond)
	defer cancel()
	if err := f.a.readForgeHistorySource(ctx, id, forgeHistoryStale); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a read behind a held one, past its deadline = %v", err)
	}
	if err := f.engine.ReleaseRefusal(forgefake.Release{Route: "gh graphql PullHistory"}); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := f.a.readForgeHistorySource(f.ctx, id, forgeHistoryStale); err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(0); ops["gh graphql PullHistory"] != 1 {
		t.Fatalf("three reads of one source asked %v; want one request", ops)
	}
}

// waitForHistoryOp waits until the forge has served n requests of op.
func (f workSearchFixture) waitForHistoryOp(t *testing.T, op string, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if ops, _ := f.historyOps(0); ops[op] >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the forge never served %d of %s", n, op)
		}
		time.Sleep(time.Millisecond)
	}
}

// A read its context ends stores nothing: the source keeps its last read
// and no failure.
func TestAForgeHistoryReadCutShortStoresNothing(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	if _, _, err := f.a.syncForgeHistorySources(f.ctx); err != nil {
		t.Fatal(err)
	}
	id := f.sources(t)[0].ID
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullHistory", Status: 502, Hold: true, Body: json.RawMessage(`{"message":"held"}`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.engine.ReleaseRefusal(forgefake.Release{Route: "gh graphql PullHistory"}) })
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.readForgeHistorySource(ctx, id, forgeHistoryStale) }()
	f.waitForHistoryOp(t, "gh graphql PullHistory", 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a read cut short = %v", err)
	}
	if src := f.sources(t)[0]; src.ReadAt != 0 || src.Failure != "" {
		t.Fatalf("a read cut short stored %+v", src)
	}
}

// A source whose walk owes more is read once a round, each read at most
// gitops.PRHistoryRequests requests, at every round until it owes
// nothing; then a round reads it only once its last read is 15 minutes
// old.
func TestARoundReadsAnOwedSourceOnceARound(t *testing.T) {
	t.Parallel()
	var pulls []forgefake.Pull
	for i := range 3000 {
		pulls = append(pulls, historyPull(3000-i, fmt.Sprintf("Change %d", 3000-i), fmt.Sprintf("change-%d", 3000-i), "merged", 3000-i))
	}
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	from, rounds := 0, 0
	for ; rounds < 5; rounds++ {
		if err := f.a.readForgeHistory(f.ctx); err != nil {
			t.Fatal(err)
		}
		ops, last := f.historyOps(from)
		from = last
		n := 0
		for _, c := range ops {
			n += c
		}
		if n == 0 || n > gitops.PRHistoryRequests {
			t.Fatalf("round %d asked %v", rounds+1, ops)
		}
		if !f.sources(t)[0].Owed {
			break
		}
	}
	if src := f.sources(t)[0]; src.Owed || rounds < 1 || src.Reach != 0 || !src.HasReach {
		t.Fatalf("after %d rounds: %+v", rounds+1, src)
	}
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(from); len(ops) != 0 {
		t.Fatalf("a round after the walk finished asked %v", ops)
	}
}

// A round reads nothing while no client is connected.
func TestARoundReadsNothingWithNoClient(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2)}}}})
	f.events.Close()
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(0); len(ops) != 0 {
		t.Fatalf("a round with no client connected asked %v", ops)
	}
}

// Two forks of one repository read it once a round: a repository is one
// source however many projects want it.
func TestTwoForksReadTheirParentOnce(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "github", Project: "team/up"},
		{Forge: "github", Project: "alice/up", Parent: "team/up"},
		{Forge: "github", Project: "bob/up", Parent: "team/up"},
	}})
	f.listWorkBoard(t, forgeProjectRepo(t, f.a, "https://github.com/alice/up.git"))
	f.listWorkBoard(t, forgeProjectRepo(t, f.a, "https://github.com/bob/up.git"))
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	_, from := f.historyOps(0)
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := sourceKeys(f.sources(t)); !slices.Equal(got, []string{"github:alice/up", "github:bob/up", "github:example/web", "github:team/up"}) {
		t.Fatalf("sources = %v", got)
	}
	if ops, _ := f.historyOps(from); ops["gh graphql PullHistory"] != 1 {
		t.Fatalf("the round after the forks' first read asked %v; want team/up read once", ops)
	}
}

// Work lists no pull request of a repository none of Work's projects
// clones, though its history is stored and names the key.
func TestTicketWorkListsOnlyItsRepositoriesHistory(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2)}},
	}})
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	other := store.ForgeHistoryRef{RepoKey: "github:other/web", Forge: "github", Host: "github.com", Namespace: "other", Repo: "web"}
	if _, _, err := f.a.store.SetForgeHistorySources([]store.ForgeHistoryRef{f.sources(t)[0].ForgeHistoryRef, other}, nil); err != nil {
		t.Fatal(err)
	}
	src := f.sources(t)[1]
	now := time.Now()
	if _, err := f.a.store.ApplyForgeHistory(store.ForgeHistoryRead{SourceID: src.ID, Source: other, OwnHead: "R_other", ReadAt: now.UnixMilli(),
		WindowStart: forgeHistoryWindowStart(now).UnixMilli(), PRs: []store.ForgeHistoryPR{{Number: 9, URL: "https://github.com/other/web/pull/9",
			Title: "WEB-1 Elsewhere", HeadRef: "elsewhere", State: "merged", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(), SeenAt: now.UnixMilli(), Keys: []string{"WEB-1"}}}}); err != nil {
		t.Fatal(err)
	}
	work, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := workPulls(work); !slices.Equal(got, []string{"github:example/web:1 merged"}) {
		t.Fatalf("Work = %v; want example/web's alone", got)
	}
}

// Work shows each pull request as it was seen last: a row of the Pull
// requests page's list read after the history's read stands over it, and
// a merge recorded for a linked thread stands over a later open row.
func TestTicketWorkShowsEachPullRequestAsSeenLast(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
		historyPull(1, "WEB-1 Fix the login", "fix-login", "open", 2), historyPull(2, "WEB-1 Cache", "cache", "open", 2),
	}}}})
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	// A list read that started a minute after the history's.
	storePRList(t, f.a, f.project, workSearchRepo, []gitops.PRListRow{
		prListRow(workSearchRepo, 1, func(r *gitops.PRListRow) {
			r.Title, r.HeadRefName, r.Draft = "WEB-1 Fix the login again", "fix-login", true
		}),
	}, -time.Minute)
	f.linkThread(t, "t1", f.project, f.dir)
	if _, err := f.a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{ThreadID: "t1", Forge: "github", Host: "github.com",
		Project: "example/web", Number: 2, URL: "https://github.com/example/web/pull/2", Branch: "cache", WorktreePath: f.dir, SeenOpenAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.store.MarkThreadPullRequestsMerged("github", "github.com", "example/web", 2, 2); err != nil {
		t.Fatal(err)
	}
	work, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(work.PullRequests, func(a, b TicketPullRequest) int { return a.Ref.Number - b.Ref.Number })
	if got := workPulls(work); !slices.Equal(got, []string{"github:example/web:1 " + TicketPRDraft, "github:example/web:2 " + TicketPRMerged}) ||
		work.PullRequests[0].Title != "WEB-1 Fix the login again" || work.PullRequests[1].Title != "WEB-1 Cache" {
		t.Fatalf("Work = %+v", work.PullRequests)
	}
}

// A fork whose parent's read failed shows the parent's failure and is not
// searched.
func TestTicketWorkShowsAForksParentFailureWithoutSearching(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "github", Project: "team/up"},
		{Forge: "github", Project: "alice/up", Parent: "team/up"},
	}})
	fork := forgeProjectRepo(t, f.a, "https://github.com/alice/up.git")
	f.listWorkBoard(t, fork)
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	// The second round reads team/up alone, and the forge refuses it.
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullHistory", Status: 502, Times: 1, Body: json.RawMessage(`{"message":"down"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	_, from := f.historyOps(0)
	work, err := f.a.SearchTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if len(work.ForgeProblems) != 1 || work.ForgeProblems[0].Repo.Key() != "github:team/up" || work.ForgeProblems[0].ProjectID != fork {
		t.Fatalf("problems = %+v; want team/up's failure under the fork", work.ForgeProblems)
	}
	if ops, _ := f.historyOps(from); ops["gh graphql PullsByKey"] != 0 {
		t.Fatalf("SearchTicketWork asked %v; want no search", ops)
	}
}

// A branch of a project is folded into a pull request of the same name
// only when the pull request's head is that project's repository: one
// whose head is a fork leaves the branch listed.
func TestTicketWorkFoldsABranchOnlyIntoItsOwnHead(t *testing.T) {
	t.Parallel()
	fromFork := historyPull(2, "WEB-1 Fix", "WEB-1-fix", "merged", 2)
	fromFork.HeadRepo = "alice/web"
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{fromFork}},
		{Forge: "github", Project: "alice/web", Parent: "example/web"},
	}})
	runGit(t, f.dir, "branch", "WEB-1-fix")
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	work, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := workPulls(work); !slices.Equal(got, []string{"github:example/web:2 merged"}) || len(work.Branches) != 1 || work.Branches[0].Name != "WEB-1-fix" {
		t.Fatalf("Work = %v, branches %+v; want example/web's own WEB-1-fix listed", got, work.Branches)
	}
}

// Work lists a pull request when its newest record names the key: one
// the history read retitled away after the stored list read it is not
// Work's, nor one a list read that started after the history's retitled
// away, and one a linked thread records is Work's whatever it names. (A
// negative age is a list read that started after the history read.)
func TestTicketWorkListsAPullRequestByItsNewestRecord(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name          string
		history, list string
		listAgo       time.Duration
		linked        bool
		want          []string
	}{
		{"the history retitled it", "WEB-2 Fix the login", "WEB-1 Fix the login", 30 * time.Minute, false, nil},
		{"the list retitled it", "WEB-1 Fix the login", "WEB-2 Fix the login", -time.Minute, false, nil},
		{"the list names it again", "WEB-2 Fix the login", "WEB-1 Fix the login", -time.Minute, false, []string{"github:example/web:1 open"}},
		{"a thread records it", "WEB-2 Fix the login", "WEB-1 Fix the login", 30 * time.Minute, true, []string{"github:example/web:1 open"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
				historyPull(1, c.history, "fix-login", "open", 2)}}}})
			if err := f.a.readForgeHistory(f.ctx); err != nil {
				t.Fatal(err)
			}
			storePRList(t, f.a, f.project, workSearchRepo, []gitops.PRListRow{
				prListRow(workSearchRepo, 1, func(r *gitops.PRListRow) { r.Title, r.HeadRefName = c.list, "fix-login" }),
			}, c.listAgo)
			if c.linked {
				f.linkThread(t, "t1", f.project, f.dir)
				if _, err := f.a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{ThreadID: "t1", Forge: "github", Host: "github.com",
					Project: "example/web", Number: 1, URL: "https://github.com/example/web/pull/1", Branch: "fix-login", WorktreePath: f.dir, SeenOpenAt: 1}); err != nil {
					t.Fatal(err)
				}
			}
			work, err := f.a.GetTicketWork(f.ctx, f.conn.ID, "10101")
			if err != nil {
				t.Fatal(err)
			}
			if got := workPulls(work); !slices.Equal(got, c.want) {
				t.Fatalf("WEB-1's Work = %v; want %v", got, c.want)
			}
		})
	}
}

// A fork's parent is read for it, but not the parent's own parent.
func TestForgeHistoryReadsAForksParentButNotItsGrandparent(t *testing.T) {
	t.Parallel()
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web"},
		{Forge: "github", Project: "corp/grand"},
		{Forge: "github", Project: "bob/parent", Parent: "corp/grand"},
		{Forge: "github", Project: "alice/fork", Parent: "bob/parent"},
	}})
	f.listWorkBoard(t, forgeProjectRepo(t, f.a, "https://github.com/alice/fork.git"))
	for range 3 {
		if err := f.a.readForgeHistory(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := sourceKeys(f.sources(t)); !slices.Equal(got, []string{"github:alice/fork", "github:bob/parent", "github:example/web"}) {
		t.Fatalf("sources = %v; want the fork's parent and no grandparent", got)
	}
}

// A round's read that waited for its source's lock reads nothing once no
// client is connected. It counts goroutines across the process to see the
// wait, so it runs alone.
func TestARoundChecksForAClientAfterItsWait(t *testing.T) {
	f := newWorkSearchFixture(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	if _, _, err := f.a.syncForgeHistorySources(f.ctx); err != nil {
		t.Fatal(err)
	}
	id := f.sources(t)[0].ID
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullHistory", Status: 502, Times: 1, Hold: true,
		Body: json.RawMessage(`{"message":"held"}`)}); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- f.a.readForgeHistorySource(f.ctx, id, forgeHistoryStale) }()
	f.waitForHistoryOp(t, "gh graphql PullHistory", 1)
	round := make(chan error, 1)
	go func() { round <- f.a.readForgeHistory(f.ctx) }()
	waitFor(t, "the round's wait for the source's lock", func() bool { return goroutinesIn("lockForgeHistorySource", "[select") > 0 })
	f.events.Close()
	if err := f.engine.ReleaseRefusal(forgefake.Release{Route: "gh graphql PullHistory"}); err != nil {
		t.Fatal(err)
	}
	<-first
	if err := <-round; err != nil {
		t.Fatal(err)
	}
	if ops, _ := f.historyOps(0); ops["gh graphql PullHistory"] != 1 {
		t.Fatalf("a round that waited past the last client asked %v", ops)
	}
}

// goroutinesIn counts the goroutines in state whose stack passes through
// fn.
func goroutinesIn(fn, state string) int {
	buf := make([]byte, 1<<22)
	n := 0
	for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, state) && strings.Contains(g, fn) {
			n++
		}
	}
	return n
}

// boardTotals is the totals revision of the fixture's board.
func (f workSearchFixture) boardTotals(t *testing.T) int64 {
	t.Helper()
	root, ok, err := f.a.store.GetMirrorRoot(f.conn.ID)
	if err != nil || !ok {
		t.Fatalf("the connection's mirror: %v, %v", ok, err)
	}
	counts, err := f.a.store.CountMirrorSections(root, "1", store.MirrorRegroup{})
	if err != nil {
		t.Fatal(err)
	}
	return counts.TotalsRevision
}

// The forge history tells the boards whose credit reads it, through the
// tracker service: a round records which connection's credit reads which
// repository and has its boards count again when that changes; a read
// that changes what a repository covers has them count again; a read
// that changes nothing tells nobody; and a read that adds an approval to
// a pull request names the ticket of its key to the open board.
func TestAForgeHistoryReadTellsTheBoardsWhoseCreditItChanges(t *testing.T) {
	t.Parallel()
	site := trackerBoardSite()
	// WEB-1 is done in the active sprint, so credit counts it.
	site.Issues[0].Status = "10002"
	// The repository keeps its id when it is seeded again with the approval.
	repo := forgefake.Repo{Forge: "github", Project: "example/web", ID: 4242, Pulls: []forgefake.Pull{historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 2)}}
	f := newWorkSearchFixtureOn(t, trackerBoardSetup{site: site}, forgefake.Fixture{Repos: []forgefake.Repo{repo}})
	view, err := f.a.ProjectTrackerSetup(f.ctx, f.project)
	if err != nil {
		t.Fatal(err)
	}
	from := int64(0)
	setup := view.Setup
	setup.Credit = &trackerapp.Credit{On: true, Support: []trackerapp.SiteSupport{},
		Rules: []trackerapp.CreditRules{{From: &from, ReviewShare: 10, ReviewNeed: 1, ReviewActions: []string{"approved"}, HoursPerPoint: 4}}}
	if save, err := f.a.SetProjectTrackerSetup(f.ctx, f.project, view.Revision, setup); err != nil || !save.Saved {
		t.Fatalf("save the credit setup: %+v, %v", save, err)
	}

	totals := f.boardTotals(t)
	if _, _, err := f.a.syncForgeHistorySources(f.ctx); err != nil {
		t.Fatal(err)
	}
	uses, err := f.a.store.ListForgeHistoryUses()
	if want := []store.ForgeHistoryUse{{ConnectionID: f.conn.ID, RepoKey: "github:example/web"}}; err != nil || !slices.Equal(uses, want) {
		t.Fatalf("uses %+v, %v; want %+v", uses, err, want)
	}
	if got := f.boardTotals(t); got != totals+1 {
		t.Fatalf("the connection's first repository moved the board's totals %d -> %d; want one advance", totals, got)
	}
	if _, _, err := f.a.syncForgeHistorySources(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.boardTotals(t); got != totals+1 {
		t.Fatalf("the same repositories again moved the board's totals to %d", got)
	}

	// The first read proves how far the repository is read.
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.boardTotals(t); got != totals+2 {
		t.Fatalf("the first read moved the board's totals %d -> %d; want one more advance", totals, got)
	}
	f.at(16 * time.Minute)
	_, ops := f.historyOps(0)
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if read, _ := f.historyOps(ops); read["gh graphql PullHistory"] != 1 {
		t.Fatalf("the second round asked %v; want one read of the repository", read)
	}
	if got := f.boardTotals(t); got != totals+2 {
		t.Fatalf("a read that changed nothing moved the board's totals to %d", got)
	}

	// The pull request is approved.
	approved := historyPull(1, "WEB-1 Fix the login", "fix-login", "merged", 0)
	approved.Reviews = []forgefake.Review{{Author: "user2", AuthorName: "User 2", State: "APPROVED"}}
	repo.Pulls = []forgefake.Pull{approved}
	if _, err := f.engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{repo}}); err != nil {
		t.Fatal(err)
	}
	f.at(32 * time.Minute)
	// Every event so far is of the reads above.
	for drained := false; !drained; {
		select {
		case <-f.events.Events():
		default:
			drained = true
		}
	}
	if err := f.a.readForgeHistory(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.boardTotals(t); got != totals+3 {
		t.Fatalf("the approval moved the board's totals %d -> %d; want one more advance", totals, got)
	}
	f.boardEvent(t, "naming the ticket whose pull request was approved", func(e trackerapp.BoardEvent) bool { return slices.Contains(e.Changed, "10101") && !e.Recount })
}
