package git

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/forgefake"
)

var historyNow = time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

// historyAt is a time n hours before historyNow, RFC 3339.
func historyAt(hours int) string {
	return historyNow.Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
}

// historyPulls are n pulls of WEB-<i>, pull i updated i hours before
// historyNow, so the newest is pull 1.
func historyPulls(n int) []forgefake.Pull {
	out := make([]forgefake.Pull, 0, n)
	for i := n; i >= 1; i-- {
		out = append(out, forgefake.Pull{Number: i, Title: fmt.Sprintf("WEB-%d Change", i), HeadRef: fmt.Sprintf("web-%d", i),
			Author: "user1", AuthorName: "User 1", CreatedAt: historyAt(i + 1), UpdatedAt: historyAt(i)})
	}
	return out
}

func historyNumbers(h PRHistory) []int64 {
	var out []int64
	for _, pr := range h.PRs {
		out = append(out, pr.Number)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// historyTestForge is a test forge whose clock is historyNow, with the
// pull requests its reads answered, as the app's store holds them
// (PRHistoryInput.Known).
type historyTestForge struct {
	*factsForge
	stored map[string]bool
	// window is the reads' window start, historyNow's when zero.
	window time.Time
}

func historyForge(t *testing.T, fixture forgefake.Fixture) *historyTestForge {
	t.Helper()
	f := newFactsForge(t, fixture)
	f.core.nowFn = func() time.Time { return historyNow }
	return &historyTestForge{factsForge: f, stored: map[string]bool{}}
}

func readHistory(t *testing.T, f *historyTestForge, repo RepoReference, walk []byte) PRHistory {
	t.Helper()
	h, err := readHistoryErr(t, f, repo, walk)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// readHistoryErr reads repo's history on from walk, at most
// PRHistoryRequests requests, and stores the pull requests it answered,
// a failed read's included.
func readHistoryErr(t *testing.T, f *historyTestForge, repo RepoReference, walk []byte) (PRHistory, error) {
	t.Helper()
	known := func(numbers []int64) (map[int64]bool, error) {
		out := map[int64]bool{}
		for _, n := range numbers {
			out[n] = f.stored[fmt.Sprintf("%s#%d", repo.Key(), n)]
		}
		return out, nil
	}
	before := f.served()
	window := f.window
	if window.IsZero() {
		window = historyNow.AddDate(0, 0, -180)
	}
	h, err := f.core.ReadPRHistory(t.Context(), repo, PRHistoryInput{Walk: walk, WindowStart: window, Known: known})
	if n := f.served() - before; n > PRHistoryRequests {
		t.Fatalf("one read made %d requests, past %d", n, PRHistoryRequests)
	}
	for _, pr := range h.PRs {
		f.stored[fmt.Sprintf("%s#%d", repo.Key(), pr.Number)] = true
	}
	return h, err
}

// served counts every request the forge served.
func (f *factsForge) served() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.seen {
		n += c
	}
	return n
}

// since is the requests by kind after before.
func (f *factsForge) since(before map[string]int) map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for key, n := range f.seen {
		if d := n - before[key]; d != 0 {
			out[key] = d
		}
	}
	return out
}

func (f *factsForge) snapshot() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.seen)
}

// readAll reads until the walk owes nothing, at most limit reads, and
// answers every read.
func readAll(t *testing.T, f *historyTestForge, repo RepoReference, walk []byte, limit int) ([]PRHistory, []byte) {
	t.Helper()
	var reads []PRHistory
	for range limit {
		h := readHistory(t, f, repo, walk)
		reads = append(reads, h)
		walk = h.Walk
		if !h.Owed {
			return reads, walk
		}
	}
	t.Fatalf("still owed after %d reads", limit)
	return nil, nil
}

func actsOf(reads ...PRHistory) []string {
	var out []string
	for _, h := range reads {
		for _, pr := range h.PRs {
			for _, a := range pr.Acts {
				out = append(out, fmt.Sprintf("%d %v %s %s", a.Number, a.Approved, a.Person.Login, a.Person.Name))
			}
		}
		for _, a := range h.Acts {
			out = append(out, fmt.Sprintf("%d %v %s %s", a.Number, a.Approved, a.Person.Login, a.Person.Name))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

var githubHistoryRepo = RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}

// GitHub's first walk reads the list back from the newest pull request,
// 50 a page, and the review comments back in bands, each read at most
// PRHistoryRequests requests and leaving the rest owed, its reach the
// oldest update it read; a steady read is a page of each stream. Each pull
// request comes with who approved and commented, a bot and a pending or
// dismissed review left out.
func TestGitHubHistoryWalksBackInBoundedReads(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2000)
	pulls[len(pulls)-1].Reviews = []forgefake.Review{
		{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(1)},
		{Author: "user3", AuthorName: "User 3", State: "CHANGES_REQUESTED", SubmittedAt: historyAt(1)},
		{Author: "ci", State: "COMMENTED", SubmittedAt: historyAt(1)},
		{Author: "user4", AuthorName: "User 4", State: "DISMISSED", SubmittedAt: historyAt(1)},
	}
	pulls[len(pulls)-1].Comments = []forgefake.Comment{{Author: "user5", AuthorName: "User 5", Body: "ok", CreatedAt: historyAt(1)}}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)

	// The first read: 15 pages of the list, down to pull request 750; the
	// review comments back to there in 3 bands (7, 14 and the rest of 28
	// days); 12 more pages of the list. The reach is the review comments',
	// which reached least.
	first := readHistory(t, f, githubHistoryRepo, nil)
	if !first.Owed || first.NoReach || len(historyNumbers(first)) != 1350 {
		t.Fatalf("first read: owed %v, no reach %v, %d pull requests", first.Owed, first.NoReach, len(historyNumbers(first)))
	}
	if want := historyNow.Add(-750 * time.Hour); !first.Reach.Equal(want) {
		t.Fatalf("reach = %v, want the review comments' walk's, %v", first.Reach, want)
	}
	newest := first.PRs[0]
	if newest.Number != 1 || newest.State != "open" || newest.Author.Login != "user1" || !newest.ActsWhole || newest.HeadRepo == "" || newest.HeadRepo != first.OwnHead {
		t.Fatalf("pull request 1 = %+v (own head %q)", newest, first.OwnHead)
	}
	if got := actsOf(PRHistory{PRs: first.PRs[:1]}); !slices.Equal(got, []string{"1 false user3 User 3", "1 false user5 User 5", "1 true user2 User 2"}) {
		t.Fatalf("acts = %v", got)
	}
	rest, walk := readAll(t, f, githubHistoryRepo, first.Walk, 5)
	last := rest[len(rest)-1]
	read := map[int64]bool{}
	for _, h := range append(rest, first) {
		for _, n := range historyNumbers(h) {
			read[n] = true
		}
	}
	if len(read) != 2000 || !last.Reach.IsZero() || last.Owed {
		t.Fatalf("after %d more reads: %d pull requests, reach %v, owed %v", len(rest), len(read), last.Reach, last.Owed)
	}
	t.Logf("first walk of 2,000 pull requests and no review comment: %d reads, %v", 1+len(rest), f.snapshot())

	// A steady read: a page of each stream, which finds pull request 300
	// updated since.
	pulls[len(pulls)-300].UpdatedAt = historyNow.Add(time.Minute).Format(time.RFC3339)
	pulls[len(pulls)-300].Title = "WEB-300 Renamed"
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	steady := readHistory(t, f, githubHistoryRepo, walk)
	cost := f.since(before)
	t.Logf("steady read: %v", cost)
	if !maps.Equal(cost, map[string]int{"PullHistory": 1, "REST comments": 1}) || steady.Owed || steady.PRs[0].Number != 300 || steady.PRs[0].Title != "WEB-300 Renamed" {
		t.Fatalf("steady read: %v, owed %v, first %+v", cost, steady.Owed, steady.PRs[0])
	}
}

// A pull request with more reviews or comments than a page holds reads
// the rest, so its acts are whole.
func TestGitHubHistoryReadsEveryReview(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	for i := range 130 {
		pulls[0].Reviews = append(pulls[0].Reviews, forgefake.Review{Author: fmt.Sprintf("u%d", i), AuthorName: "U", State: "APPROVED", SubmittedAt: historyAt(1)})
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	h := readHistory(t, f, githubHistoryRepo, nil)
	if len(h.PRs) != 1 || len(h.PRs[0].Acts) != 130 || !h.PRs[0].ActsWhole || f.seen["PullHistoryMore"] != 1 {
		t.Fatalf("acts %d, whole %v, follow-ups %d", len(h.PRs[0].Acts), h.PRs[0].ActsWhole, f.seen["PullHistoryMore"])
	}
}

// A page of pull requests that each have more comments than the page
// holds costs no more than a read's requests: the follow-up pages it
// cannot afford are left to the next reads, which finish them, and every
// commenter is read, the 1,051st of one pull request included.
func TestGitHubHistoryFollowsUpWithinTheBudgetAndFinishesLater(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(50)
	for p := range pulls {
		count := 51
		if p == 0 {
			count = 1051
		}
		for i := range count {
			pulls[p].Comments = append(pulls[p].Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(1)})
		}
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	reads, _ := readAll(t, f, githubHistoryRepo, nil, 10)
	if !reads[0].Owed {
		t.Fatal("the first read finished 99 follow-up pages")
	}
	commenters := map[int64]map[string]bool{}
	for _, h := range reads {
		for _, pr := range h.PRs {
			for _, a := range pr.Acts {
				if commenters[a.Number] == nil {
					commenters[a.Number] = map[string]bool{}
				}
				commenters[a.Number][a.Person.Login] = true
			}
		}
		for _, a := range h.Acts {
			if commenters[a.Number] == nil {
				commenters[a.Number] = map[string]bool{}
			}
			commenters[a.Number][a.Person.Login] = true
		}
	}
	for n := int64(1); n <= 50; n++ {
		want := 51
		if n == 50 {
			want = 1051
		}
		if len(commenters[n]) != want {
			t.Fatalf("pull request %d has %d commenters after %d reads; want %d", n, len(commenters[n]), len(reads), want)
		}
	}
	t.Logf("%d reads: %v", len(reads), f.snapshot())
}

// A pull request the list finds unchanged since the last read costs no
// follow-up page, however many comments it has.
func TestGitHubHistoryDoesNotFollowUpAnUnchangedPullRequest(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2)
	for i := range 60 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(3)})
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	first := readHistory(t, f, githubHistoryRepo, nil)
	before := f.snapshot()
	readHistory(t, f, githubHistoryRepo, first.Walk)
	if got := f.since(before); got["PullHistoryMore"] != 0 {
		t.Fatalf("a steady read asked %v; want no follow-up of the unchanged pull request 2", got)
	}
}

// A person who commented on the code counts as commenting, a reply and a
// comment inside an approval included, named by their review or comment
// on the read's pull requests when the comment list names only the login;
// a bot's comment counts for nothing. A comment on a pull request the
// read has not read fetches that pull request.
func TestGitHubHistoryReadsLineComments(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2)
	line := 3
	pulls[1].Reviews = []forgefake.Review{{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(1)}}
	pulls[1].Threads = []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{
		{Author: "user2", AuthorName: "User 2", Body: "nit", CreatedAt: historyAt(1)},
		{Author: "user3", AuthorName: "User 3", Body: "agreed", CreatedAt: historyAt(1)},
		{Author: "linter", Body: "bot", CreatedAt: historyAt(1)},
	}}}
	old := forgefake.Pull{Number: 70, Title: "Old", HeadRef: "old", Author: "user1", AuthorName: "User 1", CreatedAt: historyAt(24 * 300), UpdatedAt: historyAt(24 * 200),
		Threads: []forgefake.Thread{{Path: "b.go", Line: &line, Comments: []forgefake.Comment{{Author: "user6", AuthorName: "User 6", Body: "late", CreatedAt: historyAt(2)}}}}}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: append([]forgefake.Pull{old}, pulls...)}}})
	reads, _ := readAll(t, f, githubHistoryRepo, nil, 3)
	got := actsOf(reads...)
	for _, want := range []string{"1 true user2 User 2", "1 false user2 User 2", "1 false user3 ", "70 false user6 "} {
		if !slices.Contains(got, want) {
			t.Fatalf("acts = %q; want %q among them", got, want)
		}
	}
	if slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "linter") }) {
		t.Fatalf("a bot's comment counted: %v", got)
	}
	fetched := false
	for _, h := range reads {
		fetched = fetched || slices.Contains(historyNumbers(h), 70)
	}
	if !fetched {
		t.Fatal("the comment's pull request 70 was not fetched")
	}
}

// While the review comments changed since the last read are more than a
// read holds, the stream has read nothing back to any time: the read says
// so (NoReach) and owes the rest, which the next read reads.
func TestGitHubHistoryClaimsNoReachUntilTheCommentsReachThePresent(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	_, walk := readAll(t, f, githubHistoryRepo, nil, 3)
	line := 1
	thread := forgefake.Thread{Path: "a.go", Line: &line}
	for i := range 3100 {
		thread.Comments = append(thread.Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x",
			CreatedAt: historyNow.Add(time.Duration(i) * time.Second).Format(time.RFC3339)})
	}
	fixture.Repos[0].Pulls[0].Threads = []forgefake.Thread{thread}
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	busy := readHistory(t, f, githubHistoryRepo, walk)
	if !busy.NoReach || !busy.Owed {
		t.Fatalf("a read of 3,100 new review comments: no reach %v, owed %v", busy.NoReach, busy.Owed)
	}
	next := readHistory(t, f, githubHistoryRepo, busy.Walk)
	if next.NoReach || next.Owed || len(actsOf(busy, next)) != 3100 {
		t.Fatalf("the read after: no reach %v, owed %v, %d acts", next.NoReach, next.Owed, len(actsOf(busy, next)))
	}
}

// The review comments' walk back reaches a time only once it has read
// every comment updated since: a band the read could not finish holds
// the reach at its top, and the next read finishes it from where it
// stopped.
func TestGitHubHistoryCommentWalkHoldsItsReachUntilABandIsRead(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	line := 1
	thread := forgefake.Thread{Path: "a.go", Line: &line}
	for i := range 3000 {
		thread.Comments = append(thread.Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x",
			CreatedAt: historyNow.AddDate(0, 0, -60).Add(time.Duration(i) * time.Second).Format(time.RFC3339)})
	}
	pulls[0].Threads = []forgefake.Thread{thread}
	pulls[0].UpdatedAt = historyAt(1)
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	first := readHistory(t, f, githubHistoryRepo, nil)
	// Bands of 7, 14 and 28 days down to 49 days back, then 56 days
	// whose 3,000 comments the read cannot finish.
	top := historyNow.Add(-githubCommentsLag).AddDate(0, 0, -49)
	if !first.Owed || first.NoReach || !first.Reach.Equal(top) {
		t.Fatalf("first read: owed %v, no reach %v, reach %v; want %v", first.Owed, first.NoReach, first.Reach, top)
	}
	reads, _ := readAll(t, f, githubHistoryRepo, first.Walk, 5)
	if last := reads[len(reads)-1]; !last.Reach.IsZero() || len(actsOf(append(reads, first)...)) != 3000 {
		t.Fatalf("after %d reads: reach %v, %d acts", len(reads), last.Reach, len(actsOf(append(reads, first)...)))
	}
}

// More review comments sharing one update second than a page holds stop
// the read with a failure that says so, rather than reads that never
// move.
func TestGitHubHistoryRefusesAPageOfCommentsOfOneSecond(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	_, walk := readAll(t, f, githubHistoryRepo, nil, 3)
	line := 1
	thread := forgefake.Thread{Path: "a.go", Line: &line}
	for i := range 101 {
		thread.Comments = append(thread.Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyNow.Format(time.RFC3339)})
	}
	fixture.Repos[0].Pulls[0].Threads = []forgefake.Thread{thread}
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := readHistoryErr(t, f, githubHistoryRepo, walk); err == nil || !strings.Contains(err.Error(), "share the update time") {
		t.Fatalf("err = %v", err)
	}
}

// A fork's history names its parent, and a parent's pull request names
// the fork as its head.
func TestGitHubHistoryNamesTheParentAndTheHead(t *testing.T) {
	t.Parallel()
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Parent: "up/web", Pulls: historyPulls(1)},
		{Forge: "github", Project: "up/web", Pulls: []forgefake.Pull{
			{Number: 9, Title: "WEB-1 From the fork", HeadRef: "web-1", HeadRepo: "example/web", UpdatedAt: historyAt(1), CreatedAt: historyAt(2)},
			{Number: 10, Title: "Theirs", HeadRef: "web-1", UpdatedAt: historyAt(1), CreatedAt: historyAt(2)},
		}},
	}})
	fork := readHistory(t, f, githubHistoryRepo, nil)
	if fork.Parent == nil || fork.Parent.Key() != "github:up/web" {
		t.Fatalf("parent = %+v", fork.Parent)
	}
	parent := readHistory(t, f, *fork.Parent, nil)
	heads := map[int64]string{}
	for _, pr := range parent.PRs {
		heads[pr.Number] = pr.HeadRepo
	}
	if heads[9] != fork.OwnHead || heads[10] == fork.OwnHead {
		t.Fatalf("heads = %v, fork %q", heads, fork.OwnHead)
	}
}

// newerPulls are n pulls numbered from first, updated a minute apart
// after historyNow, the newest last.
func newerPulls(first, n int) []forgefake.Pull {
	out := make([]forgefake.Pull, 0, n)
	for i := range n {
		at := historyNow.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)
		out = append(out, forgefake.Pull{Number: first + i, Title: fmt.Sprintf("WEB-%d Change", first+i), HeadRef: fmt.Sprintf("web-%d", first+i),
			Author: "user1", AuthorName: "User 1", CreatedAt: at, UpdatedAt: at})
	}
	return out
}

// A GitHub read that finds more changed since the last read than it can
// read leaves the rest as the gap; the next read reads on and none of
// the changes is missed.
func TestGitHubHistoryLeavesWhatChangedPastOneReadAsTheGap(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(100)
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	_, walk := readAll(t, f, githubHistoryRepo, nil, 3)
	fixture.Repos[0].Pulls = append(pulls, newerPulls(1000, 2000)...)
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	// The list's 40 new pages take the read's requests, so it reads no
	// review comment and claims no reach.
	busy := readHistory(t, f, githubHistoryRepo, walk)
	if !busy.Owed || !busy.NoReach {
		t.Fatalf("a read of 2,000 changes: owed %v, no reach %v", busy.Owed, busy.NoReach)
	}
	reads, _ := readAll(t, f, githubHistoryRepo, busy.Walk, 5)
	read := map[int64]bool{}
	for _, h := range append(reads, busy) {
		for _, n := range historyNumbers(h) {
			read[n] = true
		}
	}
	missing := 0
	for i := 1000; i < 3000; i++ {
		if !read[int64(i)] {
			missing++
		}
	}
	if last := reads[len(reads)-1]; !last.Reach.IsZero() || missing != 0 {
		t.Fatalf("after %d reads: reach %v, %d of the 2,000 changes unread", len(reads), last.Reach, missing)
	}
}

var gitlabHistoryRepo = RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web"}

// GitLab's first walk reads the merge request list and both events feeds
// back from the newest, and fetches a merge request an event names that
// the list did not reach: one approved now but last updated before the
// window. Every kind of note on a merge request is a comment: a
// conversation comment (Note), a reply (DiscussionNote) and a comment on
// the code (DiffNote). A steady read is one page of each stream, which
// finds a new approval that did not move the merge request's update
// time.
func TestGitLabHistoryReadsTheListAndTheEvents(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(3)
	line := 1
	pulls[2].Reviews = []forgefake.Review{{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(1)}}
	pulls[2].Comments = []forgefake.Comment{{Author: "user3", AuthorName: "User 3", Body: "ok", CreatedAt: historyAt(2),
		Replies: []forgefake.Comment{{Author: "user5", AuthorName: "User 5", Body: "yes", CreatedAt: historyAt(2)}}}}
	pulls[2].Threads = []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{{Author: "user6", AuthorName: "User 6", Body: "nit", CreatedAt: historyAt(2)}}}}
	old := forgefake.Pull{Number: 50, Title: "WEB-50 Old", HeadRef: "web-50", CreatedAt: historyAt(24 * 300), UpdatedAt: historyAt(24 * 200),
		Reviews: []forgefake.Review{{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(3)}}}
	pulls = append([]forgefake.Pull{old}, pulls...)
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}}
	f := historyForge(t, fixture)

	first := readHistory(t, f, gitlabHistoryRepo, nil)
	if got := historyNumbers(first); !slices.Equal(got, []int64{1, 2, 3, 50}) || first.Owed || !first.Reach.IsZero() {
		t.Fatalf("first read: %v, owed %v, reach %v", got, first.Owed, first.Reach)
	}
	if got := actsOf(PRHistory{Acts: first.Acts}); !slices.Equal(got, []string{"1 false user3 User 3", "1 false user5 User 5", "1 false user6 User 6", "1 true user2 User 2", "50 true user2 User 2"}) {
		t.Fatalf("acts = %v", got)
	}
	firstSeen := f.snapshot()
	t.Logf("first full read of 4 merge requests and 5 events: %v", firstSeen)

	f.core.nowFn = func() time.Time { return historyNow.Add(15 * time.Minute) }
	pulls[3].Reviews = append(pulls[3].Reviews, forgefake.Review{Author: "user4", AuthorName: "User 4", State: "APPROVED", SubmittedAt: historyAt(0)})
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	second := readHistory(t, f, gitlabHistoryRepo, first.Walk)
	if got := actsOf(PRHistory{Acts: second.Acts}); !slices.Contains(got, "1 true user4 User 4") {
		t.Fatalf("steady read acts = %v", got)
	}
	steady := f.since(firstSeen)
	t.Logf("steady read: %v", steady)
	if !maps.Equal(steady, map[string]int{"REST web": 1, "REST merge_requests": 1, "REST events": 2}) {
		t.Fatalf("steady read requests = %v", steady)
	}
}

// A busy project's steady read: about 190 comment events a day for a
// week, read every 15 minutes, costs the project, a page of the list,
// a page of approvals, and two pages of comments: the one with the new
// comments and the one that shows the read went past an hour before the
// last read.
func TestGitLabHistorySteadyReadOfABusyProject(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	pulls[0].UpdatedAt = historyAt(24 * 8)
	for i := range 190 * 7 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: int64(10_000 + i), Author: fmt.Sprintf("u%d", i%40), AuthorName: "U", Body: "x",
			CreatedAt: historyNow.Add(-time.Duration(190*7-i) * (24 * time.Hour) / 190).Format(time.RFC3339)})
	}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	_, walk := readAll(t, f, gitlabHistoryRepo, nil, 5)
	walked := f.snapshot()
	t.Logf("first walk of 1,330 comment events: %v", walked)
	f.core.nowFn = func() time.Time { return historyNow.Add(15 * time.Minute) }
	for i := range 3 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: int64(90_000 + i), Author: "late", AuthorName: "L", Body: "x",
			CreatedAt: historyNow.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)})
	}
	fixture.Repos[0].Pulls = pulls
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	h := readHistory(t, f, gitlabHistoryRepo, walk)
	steady := f.since(walked)
	t.Logf("steady read 15 minutes later: %v", steady)
	if !maps.Equal(steady, map[string]int{"REST web": 1, "REST merge_requests": 1, "REST events": 3}) || !slices.Contains(actsOf(h), "1 false late L") {
		t.Fatalf("steady read: %v, acts %v", steady, actsOf(h))
	}
}

// More merge requests sharing one update time than a page holds stop the
// read with a failure that says so, rather than a read that never moves.
func TestGitLabHistoryRefusesAPageOfOneUpdateTime(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(201)
	for i := range pulls {
		if i < 101 {
			pulls[i].UpdatedAt = historyAt(1)
		}
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}})
	_, err := readHistoryErr(t, f, gitlabHistoryRepo, nil)
	if err == nil || !strings.Contains(err.Error(), "share the update time") {
		t.Fatalf("err = %v", err)
	}
}

// A GitLab read reads on from the newest merge request while its first
// walk back is still going, so one changed meanwhile is read at once.
func TestGitLabHistoryReadsOnFromTheNewestWhileItWalksBack(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(3000)
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	first := readHistory(t, f, gitlabHistoryRepo, nil)
	if !first.Owed || first.Reach.IsZero() {
		t.Fatalf("first read: owed %v, reach %v", first.Owed, first.Reach)
	}
	fixture.Repos[0].Pulls = append(slices.Clone(pulls), newerPulls(5000, 1)...)
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	second := readHistory(t, f, gitlabHistoryRepo, first.Walk)
	if !slices.Contains(historyNumbers(second), 5000) {
		t.Fatalf("second read: %d merge requests, no 5000", len(second.PRs))
	}
}

// An approval whose merge request the read could not fetch is not moved
// past: the next read reads it again, with the merge request.
func TestGitLabHistoryKeepsAnEventWhoseMergeRequestFailedToFetch(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	old := forgefake.Pull{Number: 50, Title: "WEB-50 Old", HeadRef: "web-50", CreatedAt: historyAt(24 * 300), UpdatedAt: historyAt(24 * 200)}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: append([]forgefake.Pull{old}, pulls...)}}}
	f := historyForge(t, fixture)
	first := readHistory(t, f, gitlabHistoryRepo, nil)
	f.core.nowFn = func() time.Time { return historyNow.Add(15 * time.Minute) }
	fixture.Repos[0].Pulls[0].Reviews = []forgefake.Review{{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyNow.Add(time.Minute).Format(time.RFC3339)}}
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	f.failAt["REST merge_requests"] = f.seen["REST merge_requests"] + 2
	second, err := readHistoryErr(t, f, gitlabHistoryRepo, first.Walk)
	if err == nil {
		t.Fatal("the lookup did not fail")
	}
	if len(second.Acts) != 0 {
		t.Fatalf("a read whose lookup failed answered acts %v", second.Acts)
	}
	third := readHistory(t, f, gitlabHistoryRepo, second.Walk)
	if !slices.Contains(historyNumbers(third), 50) || !slices.Contains(actsOf(third), "50 true user2 User 2") {
		t.Fatalf("the read after: rows %v, acts %v", historyNumbers(third), actsOf(third))
	}
}

// A deletion of events the last read read moves nothing past a new one:
// no read keeps a page number.
func TestGitLabHistoryADeletionSkipsNoNewComment(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	for i := range 101 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: int64(1000 + i), Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x",
			CreatedAt: historyNow.Add(-time.Duration(200-i) * time.Minute).Format(time.RFC3339)})
	}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	first := readHistory(t, f, gitlabHistoryRepo, nil)
	f.core.nowFn = func() time.Time { return historyNow.Add(15 * time.Minute) }
	pulls[0].Comments = append(pulls[0].Comments[2:], forgefake.Comment{ID: 5000, Author: "newcomer", AuthorName: "N", Body: "x", CreatedAt: historyNow.Add(time.Minute).Format(time.RFC3339)})
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	second := readHistory(t, f, gitlabHistoryRepo, first.Walk)
	if !slices.Contains(actsOf(second), "1 false newcomer N") {
		t.Fatalf("the read after two deletions = %v", actsOf(second))
	}
}

// The feed is sorted by event id, not time: an event whose id is newer
// than its time (one imported, or committed late) neither ends the walk
// back early nor is lost.
func TestGitLabHistoryWalksEventsOutOfTimeOrder(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	pulls[0].UpdatedAt = historyAt(24 * 100)
	pulls[0].CreatedAt = historyAt(24 * 101)
	for i := range 5 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: int64(100 + i), Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x",
			CreatedAt: historyAt(24 * (10 + 20*i))})
	}
	// The newest id, created 170 days back.
	pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: 999, Author: "imported", AuthorName: "I", Body: "x", CreatedAt: historyAt(24 * 170)})
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}})
	reads, _ := readAll(t, f, gitlabHistoryRepo, nil, 5)
	if got := actsOf(reads...); len(got) != 6 {
		t.Fatalf("acts = %v; want all six", got)
	}
}

// A band of review comments a read could not finish is finished from
// where it stopped, down to the lower end it started with, though the
// list's walk has moved past it meanwhile: no comment below where the
// band stopped is skipped.
func TestGitHubHistoryFinishesACommentBandWhereItBegan(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2000)
	line := 1
	busy := forgefake.Thread{Path: "a.go", Line: &line}
	for i := range 1500 {
		busy.Comments = append(busy.Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x",
			CreatedAt: historyNow.Add(-600 * time.Hour).Add(time.Duration(i) * time.Second).Format(time.RFC3339)})
	}
	pulls[len(pulls)-600].Threads = []forgefake.Thread{busy}
	pulls[len(pulls)-900].Threads = []forgefake.Thread{{Path: "b.go", Line: &line, Comments: []forgefake.Comment{
		{Author: "early", AuthorName: "E", Body: "x", CreatedAt: historyNow.Add(-900 * time.Hour).Format(time.RFC3339)}}}}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	reads, _ := readAll(t, f, githubHistoryRepo, nil, 8)
	if got := actsOf(reads...); !slices.Contains(got, "900 false early ") || len(got) != 1501 {
		t.Fatalf("after %d reads, %d acts; want 1,501 with early's on pull request 900", len(reads), len(got))
	}
}

// The first walk of a repository of 10,000 review comments over the
// window and 2,000 pull requests, each read at most PRHistoryRequests
// requests: the reads it takes, and the read by which a ticket created a
// week, a month and 90 days back is covered. (At 100,000 comments, 30 s
// of the fake's time: 39 reads, a week covered after the 4th, a month
// after the 8th, 90 days after the 21st.)
func TestGitHubHistoryWalksTenThousandReviewComments(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2000)
	line := 1
	window := 180 * day
	for p := range 100 {
		thread := forgefake.Thread{Path: "a.go", Line: &line}
		for i := range 100 {
			at := historyNow.Add(-githubCommentsLag - window + time.Duration(p*100+i)*(window/10_000))
			thread.Comments = append(thread.Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: at.Format(time.RFC3339)})
		}
		pulls[p].Threads = []forgefake.Thread{thread}
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	covered := map[int]int{}
	var walk []byte
	reads := 0
	for reads < 100 {
		h := readHistory(t, f, githubHistoryRepo, walk)
		reads++
		walk = h.Walk
		for _, days := range []int{7, 30, 90} {
			created := historyNow.AddDate(0, 0, -days)
			if covered[days] == 0 && !h.NoReach && !h.Reach.After(created) {
				covered[days] = reads
			}
		}
		if !h.Owed {
			break
		}
	}
	t.Logf("%d reads (%v); a ticket created 7 days back is covered after read %d, 30 days after read %d, 90 days after read %d",
		reads, f.snapshot(), covered[7], covered[30], covered[90])
	if covered[90] == 0 || reads == 100 {
		t.Fatalf("the walk did not finish in %d reads", reads)
	}
}

// The window's start moves on with each read: an act stream's walk back,
// opened down to the first read's window start, is done once it reaches
// a later read's, on both forges, and the reads after owe nothing.
func TestHistoryWalkBackEndsAtAMovingWindowStart(t *testing.T) {
	t.Parallel()
	for _, repo := range []RepoReference{githubHistoryRepo, gitlabHistoryRepo} {
		t.Run(repo.Forge, func(t *testing.T) {
			t.Parallel()
			f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: repo.Forge, Project: repo.Project(), Pulls: historyPulls(3000)}}})
			var walk []byte
			owed, reads := true, 0
			for ; owed && reads < 8; reads++ {
				f.window = historyNow.AddDate(0, 0, -180).Add(time.Duration(reads) * time.Minute)
				h := readHistory(t, f, repo, walk)
				walk, owed = h.Walk, h.Owed
			}
			t.Logf("first walk of 3,000 pull requests: %d reads, %v", reads, f.snapshot())
			if owed {
				t.Fatalf("still owed after 8 reads: %s", walk)
			}
		})
	}
}
