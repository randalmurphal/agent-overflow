package git

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
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
// pull requests its reads answered as the app's store holds them
// (PRHistoryInput.Stored), by repository key and number.
type historyTestForge struct {
	*factsForge
	stored map[string]StoredPR
	// window is the reads' window start, historyNow's when zero.
	window time.Time
}

func historyForge(t *testing.T, fixture forgefake.Fixture) *historyTestForge {
	t.Helper()
	f := newFactsForge(t, fixture)
	f.core.nowFn = func() time.Time { return historyNow }
	return &historyTestForge{factsForge: f, stored: map[string]StoredPR{}}
}

func readHistory(t *testing.T, f *historyTestForge, repo RepoReference, walk []byte) PRHistory {
	t.Helper()
	h, err := readHistoryErr(t, f, repo, walk)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// historyMaxRequests is the most requests one read of repo's forge may
// make.
func historyMaxRequests(repo RepoReference) int {
	if repo.Forge == "gitlab" {
		return gitlabHistoryMaxRequests
	}
	return PRHistoryRequests
}

// githubHistoryMaxPoints is the most GraphQL points one GitHub read
// spends: PRHistoryRequests list pages (TestGitHubHistoryPointsOfOneRead).
const githubHistoryMaxPoints = 90

// githubHistoryPoints is the GraphQL points of a GitHub read's requests.
func githubHistoryPoints(cost map[string]int) int {
	return cost["PullHistory"]*graphQLPoints(githubPullHistoryQuery.Text()) + cost["PullHistoryMore"]*graphQLPoints(githubPullHistoryMoreQuery.Text())
}

// graphQLPoints is a GitHub GraphQL document's rate limit cost as GitHub
// documents it: each connection costs one request per node of the
// connections around it, at their first or last bounds; the sum over
// 100, rounded, is the points, at least 1. A part a directive may skip
// counts.
func graphQLPoints(text string) int {
	bound := regexp.MustCompile(`\b(?:first|last):\s*(\d+)`)
	nodes := []int{1}
	requests, next := 0, 1
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '(':
			end := i + strings.IndexByte(text[i:], ')')
			if m := bound.FindStringSubmatch(text[i:end]); m != nil {
				n, _ := strconv.Atoi(m[1])
				requests += nodes[len(nodes)-1]
				next = n
			}
			i = end
		case '{':
			nodes = append(nodes, nodes[len(nodes)-1]*next)
			next = 1
		case '}':
			nodes = nodes[:len(nodes)-1]
		}
	}
	return max(1, int(math.Round(float64(requests)/100)))
}

// readHistoryErr reads repo's history on from walk, at most
// historyMaxRequests requests and, on GitHub, githubHistoryMaxPoints
// points, and stores the pull requests it answered, a failed read's
// included.
func readHistoryErr(t *testing.T, f *historyTestForge, repo RepoReference, walk []byte) (PRHistory, error) {
	t.Helper()
	key := func(n int64) string { return fmt.Sprintf("%s#%d", repo.Key(), n) }
	stored := func(numbers []int64) (map[int64]StoredPR, error) {
		out := map[int64]StoredPR{}
		for _, n := range numbers {
			if pr, ok := f.stored[key(n)]; ok {
				out[n] = pr
			}
		}
		return out, nil
	}
	before := f.snapshot()
	window := f.window
	if window.IsZero() {
		window = historyNow.AddDate(0, 0, -180)
	}
	h, err := f.core.ReadPRHistory(t.Context(), repo, PRHistoryInput{Walk: walk, WindowStart: window, Stored: stored})
	cost := f.since(before)
	n := 0
	for _, c := range cost {
		n += c
	}
	if n > historyMaxRequests(repo) {
		t.Fatalf("one read made %d requests, past %d", n, historyMaxRequests(repo))
	}
	if p := githubHistoryPoints(cost); repo.Forge == "github" && p > githubHistoryMaxPoints {
		t.Fatalf("one read spent %d points, past %d", p, githubHistoryMaxPoints)
	}
	for _, pr := range h.PRs {
		f.stored[key(pr.Number)] = StoredPR{Updated: pr.Updated, Seen: pr.Seen}
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
		for _, a := range h.Acts {
			out = append(out, fmt.Sprintf("%d %v %s %s", a.Number, a.Approved, a.Person.Login, a.Person.Name))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

var githubHistoryRepo = RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}

// A history page costs GitHub 3 points and a follow-up page 1, so one
// read of at most PRHistoryRequests requests spends at most 90.
func TestGitHubHistoryPointsOfOneRead(t *testing.T) {
	t.Parallel()
	page, more := graphQLPoints(githubPullHistoryQuery.Text()), graphQLPoints(githubPullHistoryMoreQuery.Text())
	if page != 3 || more != 1 || PRHistoryRequests*max(page, more) != githubHistoryMaxPoints {
		t.Fatalf("a page costs %d points and a follow-up %d; one read at most %d, want %d", page, more, PRHistoryRequests*max(page, more), githubHistoryMaxPoints)
	}
}

// GitHub's first walk reads the list back from the newest pull request,
// 50 a page, each read at most PRHistoryRequests requests and leaving the
// rest owed, its reach the oldest update it read; a steady read is one
// page. Each pull request comes with who approved and commented, a bot
// and a pending or dismissed review left out.
func TestGitHubHistoryWalksBackInBoundedReads(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2000)
	pulls[len(pulls)-1].Reviews = []forgefake.Review{
		{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(1)},
		{Author: "user3", AuthorName: "User 3", State: "CHANGES_REQUESTED", SubmittedAt: historyAt(1)},
		{Author: "ci", State: "COMMENTED", SubmittedAt: historyAt(1)},
		{Author: "user4", AuthorName: "User 4", State: "DISMISSED", SubmittedAt: historyAt(1)},
		{Author: "user7", AuthorName: "User 7", State: "PENDING", SubmittedAt: historyAt(1)},
	}
	pulls[len(pulls)-1].Comments = []forgefake.Comment{{Author: "user5", AuthorName: "User 5", Body: "ok", CreatedAt: historyAt(1)}}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)

	// The first read: 30 pages of the list, down to pull request 1,500.
	first := readHistory(t, f, githubHistoryRepo, nil)
	if !first.Owed || !first.HasReach || len(historyNumbers(first)) != 1500 {
		t.Fatalf("first read: owed %v, reach %v, %d pull requests", first.Owed, first.HasReach, len(historyNumbers(first)))
	}
	if want := historyNow.Add(-1500 * time.Hour); !first.Reach.Equal(want) {
		t.Fatalf("reach = %v, want the oldest update read, %v", first.Reach, want)
	}
	newest := first.PRs[0]
	if newest.Number != 1 || newest.State != "open" || newest.Author.Login != "user1" || newest.HeadRepo == "" || newest.HeadRepo != first.OwnHead || !newest.Seen.Equal(historyNow) {
		t.Fatalf("pull request 1 = %+v (own head %q)", newest, first.OwnHead)
	}
	if got := actsOf(first); !slices.Equal(got, []string{"1 false user3 User 3", "1 false user5 User 5", "1 true user2 User 2"}) {
		t.Fatalf("acts = %v", got)
	}
	walked := f.snapshot()
	rest, walk := readAll(t, f, githubHistoryRepo, first.Walk, 5)
	last := rest[len(rest)-1]
	read := map[int64]bool{}
	for _, h := range append(rest, first) {
		for _, n := range historyNumbers(h) {
			read[n] = true
		}
	}
	if len(read) != 2000 || !last.HasReach || !last.Reach.IsZero() || last.Owed {
		t.Fatalf("after %d more reads: %d pull requests, reach %v %v, owed %v", len(rest), len(read), last.HasReach, last.Reach, last.Owed)
	}
	whole := f.snapshot()
	t.Logf("first walk of 2,000 pull requests: %d reads, %v, %d points; the first read %d points",
		1+len(rest), whole, githubHistoryPoints(whole), githubHistoryPoints(walked))

	// A steady read: one page, which finds pull request 300 updated since.
	pulls[len(pulls)-300].UpdatedAt = historyNow.Add(time.Minute).Format(time.RFC3339)
	pulls[len(pulls)-300].Title = "WEB-300 Renamed"
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	steady := readHistory(t, f, githubHistoryRepo, walk)
	cost := f.since(before)
	t.Logf("steady read: %v, %d points", cost, githubHistoryPoints(cost))
	if !maps.Equal(cost, map[string]int{"PullHistory": 1}) || steady.Owed || steady.PRs[0].Number != 300 || steady.PRs[0].Title != "WEB-300 Renamed" {
		t.Fatalf("steady read: %v, owed %v, first %+v", cost, steady.Owed, steady.PRs[0])
	}
}

// GitHub documents neither behavior the history relies on for comments on
// the code, so each is pinned here as the fake models it
// (forge-transport.md#pull-request-history): a line comment outside an
// approval, a reply included, arrives as its own COMMENTED review by its
// writer, which is a comment by them; an approval whose line comments
// count above zero is an approval and a comment, both at its submit time.
// An approval with none is no comment.
func TestGitHubHistoryReadsCommentsOnTheCodeFromTheReviews(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	line := 3
	pulls[0].Reviews = []forgefake.Review{
		{ID: 7001, Author: "alice", AuthorName: "Alice", State: "APPROVED", SubmittedAt: historyAt(3)},
		{ID: 7002, Author: "bob", AuthorName: "Bob", State: "COMMENTED", SubmittedAt: historyAt(2)},
		{ID: 7003, Author: "carol", AuthorName: "Carol", State: "APPROVED", SubmittedAt: historyAt(1)},
	}
	pulls[0].Threads = []forgefake.Thread{{Path: "a.go", Line: &line, Comments: []forgefake.Comment{
		{Author: "alice", AuthorName: "Alice", Body: "nit", CreatedAt: historyAt(3), Review: 7001},
		{Author: "bob", AuthorName: "Bob", Body: "agreed", CreatedAt: historyAt(2), Review: 7002},
	}}}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	h := readHistory(t, f, githubHistoryRepo, nil)
	var got []string
	for _, a := range h.Acts {
		got = append(got, fmt.Sprintf("%v %s %s", a.Approved, a.Person.Login, a.At.Format(time.RFC3339)))
	}
	slices.Sort(got)
	want := []string{"false alice " + historyAt(3), "false bob " + historyAt(2), "true alice " + historyAt(3), "true carol " + historyAt(1)}
	if !slices.Equal(got, want) {
		t.Fatalf("acts = %q; want %q", got, want)
	}
}

// A pull request with more approvals than a page holds reads the rest.
func TestGitHubHistoryReadsEveryApproval(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	for i := range 130 {
		pulls[0].Reviews = append(pulls[0].Reviews, forgefake.Review{Author: fmt.Sprintf("u%d", i), AuthorName: "U", State: "APPROVED", SubmittedAt: historyAt(1)})
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	h := readHistory(t, f, githubHistoryRepo, nil)
	if len(h.Acts) != 130 || h.Owed || f.seen["PullHistoryMore"] != 2 {
		t.Fatalf("acts %d, owed %v, follow-ups %d", len(h.Acts), h.Owed, f.seen["PullHistoryMore"])
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
		t.Fatal("the first read finished 59 follow-up pages")
	}
	commenters := map[int64]map[string]bool{}
	for _, h := range reads {
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

// A read that fails keeps the follow-ups the walk it began from left: they
// leave only as their pages are read.
func TestGitHubHistoryAFailedReadKeepsTheFollowUps(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(50)
	for p := range pulls {
		for i := range 51 {
			pulls[p].Comments = append(pulls[p].Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(1)})
		}
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	first := readHistory(t, f, githubHistoryRepo, nil)
	pending := githubFollowUps(t, first.Walk)
	if len(pending) == 0 {
		t.Fatal("the first read left no follow-up")
	}
	f.failAt["PullHistory"] = f.seen["PullHistory"] + 1
	second, err := readHistoryErr(t, f, githubHistoryRepo, first.Walk)
	if err == nil {
		t.Fatal("the read did not fail")
	}
	if got := githubFollowUps(t, second.Walk); !slices.Equal(got, pending) {
		t.Fatalf("the first read left %d follow-ups; the failed read's walk keeps %d", len(pending), len(got))
	}
}

// githubFollowUps are the stretches of acts walk leaves to read.
func githubFollowUps(t *testing.T, walk []byte) []githubMoreWalk {
	t.Helper()
	var w githubHistoryWalk
	if err := json.Unmarshal(walk, &w); err != nil {
		t.Fatal(err)
	}
	return w.More
}

// A pull request the list finds unchanged since the last read costs no
// follow-up page, however many approvals and comments it has: the newest
// pull request, which every steady read's page holds, has more approvals
// than a page reads and is read on only by the first read.
func TestGitHubHistoryDoesNotFollowUpAnUnchangedPullRequest(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2)
	newest := &pulls[1]
	for i := range 5 {
		newest.Reviews = append(newest.Reviews, forgefake.Review{Author: fmt.Sprintf("a%d", i), AuthorName: "A", State: "APPROVED", SubmittedAt: historyAt(3)})
	}
	for i := range 60 {
		newest.Comments = append(newest.Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(3)})
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	first := readHistory(t, f, githubHistoryRepo, nil)
	if got := len(actsOf(first)); got != 65 || first.Owed || f.seen["PullHistoryMore"] != 2 {
		t.Fatalf("the first read: %d acts, owed %v, %d follow-ups; want pull request 1's 5 approvals and 60 comments in 2", got, first.Owed, f.seen["PullHistoryMore"])
	}
	before := f.snapshot()
	steady := readHistory(t, f, githubHistoryRepo, first.Walk)
	if got := f.since(before); !maps.Equal(got, map[string]int{"PullHistory": 1}) || !slices.Equal(historyNumbers(steady), []int64{1}) {
		t.Fatalf("a steady read asked %v and read %v; want one page holding the unchanged pull request 1 and no follow-up", got, historyNumbers(steady))
	}
}

// A read reads the follow-up pages its walk holds before it walks the
// list's gap on, and spends what they leave on the gap: of 2,000 pull
// requests whose newest 50 each leave one follow-up page, the first read
// walks 30 pages of the list, the second reads 29 follow-ups beside its
// page of the newest, and the third the 21 left and 8 pages of the gap.
func TestGitHubHistoryFollowsUpBeforeItWalksOn(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(2000)
	for p := len(pulls) - 50; p < len(pulls); p++ {
		for i := range 51 {
			pulls[p].Comments = append(pulls[p].Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(1)})
		}
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	var walk []byte
	for i, want := range []struct {
		cost    map[string]int
		left    int
		reached int
	}{
		{map[string]int{"PullHistory": 30}, 50, 1500},
		{map[string]int{"PullHistory": 1, "PullHistoryMore": 29}, 21, 1500},
		{map[string]int{"PullHistory": 9, "PullHistoryMore": 21}, 0, 1900},
	} {
		before := f.snapshot()
		h := readHistory(t, f, githubHistoryRepo, walk)
		walk = h.Walk
		reach := historyNow.Add(-time.Duration(want.reached) * time.Hour)
		if cost, left := f.since(before), len(githubFollowUps(t, walk)); !maps.Equal(cost, want.cost) || left != want.left || !h.Reach.Equal(reach) || !h.Owed {
			t.Fatalf("read %d asked %v, left %d follow-ups and reached %v; want %v, %d and pull request %d's update, %v",
				i+1, cost, left, h.Reach, want.cost, want.left, want.reached, reach)
		}
	}
}

// A pull request that changed reads its comments back only to a little
// before the read that last saw it: an old long pull request with one new
// comment costs one page.
func TestGitHubHistoryReadsAChangedPullRequestBackToItsLastRead(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	for i := range 200 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(3)})
	}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	_, walk := readAll(t, f, githubHistoryRepo, nil, 2)
	f.core.nowFn = func() time.Time { return historyNow.Add(15 * time.Minute) }
	newAt := historyNow.Add(time.Minute).Format(time.RFC3339)
	fixture.Repos[0].Pulls[0].Comments = append(fixture.Repos[0].Pulls[0].Comments, forgefake.Comment{Author: "late", AuthorName: "L", Body: "x", CreatedAt: newAt})
	fixture.Repos[0].Pulls[0].UpdatedAt = newAt
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	h := readHistory(t, f, githubHistoryRepo, walk)
	if cost := f.since(before); !maps.Equal(cost, map[string]int{"PullHistory": 1}) || !slices.Contains(actsOf(h), "1 false late L") || h.Owed {
		t.Fatalf("a read of one new comment: %v, acts %v, owed %v", cost, actsOf(h), h.Owed)
	}
}

// The newest pull request, with 3,100 comments and a new one on every
// read, is read whole: each read's page of it adds the newest stretch,
// and the stretch older reads left goes on.
func TestGitHubHistoryReadsALongPullRequestThatChangesOnEveryRead(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	for i := range 3100 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(2)})
	}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	var walk []byte
	var reads []PRHistory
	for i := range 4 {
		now := historyNow.Add(time.Duration(i) * 15 * time.Minute)
		f.core.nowFn = func() time.Time { return now }
		at := now.Add(-time.Minute).Format(time.RFC3339)
		fixture.Repos[0].Pulls[0].Comments = append(fixture.Repos[0].Pulls[0].Comments, forgefake.Comment{Author: fmt.Sprintf("new%d", i), AuthorName: "N", Body: "x", CreatedAt: at})
		fixture.Repos[0].Pulls[0].UpdatedAt = at
		if _, err := f.engine.Seed(fixture); err != nil {
			t.Fatal(err)
		}
		h := readHistory(t, f, githubHistoryRepo, walk)
		reads, walk = append(reads, h), h.Walk
	}
	if got := len(actsOf(reads...)); got != 3104 || reads[3].Owed {
		t.Fatalf("after 4 reads: %d commenters of 3,104, owed %v", got, reads[3].Owed)
	}
}

// A follow-up of a pull request the store no longer holds, which the read
// has not read, is dropped: its acts would name a pull request the store
// cannot keep them on.
func TestGitHubHistoryDropsTheFollowUpOfAPullRequestTheStoreLeft(t *testing.T) {
	t.Parallel()
	pulls := append(historyPulls(1), forgefake.Pull{Number: 77, Title: "Old", HeadRef: "old", Author: "user1", AuthorName: "User 1",
		CreatedAt: historyAt(24 * 300), UpdatedAt: historyAt(24 * 200)})
	for i := range 60 {
		pulls[1].Comments = append(pulls[1].Comments, forgefake.Comment{Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x", CreatedAt: historyAt(24 * 200)})
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	_, walk := readAll(t, f, githubHistoryRepo, nil, 2)
	var w githubHistoryWalk
	if err := json.Unmarshal(walk, &w); err != nil {
		t.Fatal(err)
	}
	w.More = []githubMoreWalk{{Number: 77, Kind: githubActsComments, Cursor: "cursor:10"}}
	walk, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	h := readHistory(t, f, githubHistoryRepo, walk)
	if cost := f.since(before); cost["PullHistoryMore"] != 0 || len(h.Acts) != 0 || len(githubFollowUps(t, h.Walk)) != 0 || h.Owed {
		t.Fatalf("a follow-up of pull request 77, which the store left: %v, acts %v, walk %s", cost, actsOf(h), h.Walk)
	}
}

// A pull request already waiting for its approvals, which changed, keeps
// one follow-up of them: the one further on.
func TestGitHubHistoryKeepsOneApprovalsFollowUpPerPullRequest(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	for i := range 10 {
		pulls[0].Reviews = append(pulls[0].Reviews, forgefake.Review{Author: fmt.Sprintf("u%d", i), AuthorName: "U", State: "APPROVED", SubmittedAt: historyAt(1)})
	}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	_, walk := readAll(t, f, githubHistoryRepo, nil, 2)
	var w githubHistoryWalk
	if err := json.Unmarshal(walk, &w); err != nil {
		t.Fatal(err)
	}
	w.More = []githubMoreWalk{{Number: 1, Kind: githubActsApprovals, Cursor: "cursor:3"}}
	walk, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Repos[0].Pulls[0].UpdatedAt = historyNow.Add(time.Minute).Format(time.RFC3339)
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	readHistory(t, f, githubHistoryRepo, walk)
	if cost := f.since(before); cost["PullHistoryMore"] != 1 {
		t.Fatalf("a changed pull request waiting for its approvals: %v; want one follow-up", cost)
	}
}

// A read claims a reach only back to what it read: a first read reads its
// newest pull request's acts of a minute ago, and one that fails before
// its first page claims none.
func TestGitHubHistoryClaimsAReachOnlyForWhatItRead(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	pulls[0].Reviews = []forgefake.Review{{Author: "late", AuthorName: "L", State: "COMMENTED", SubmittedAt: historyNow.Add(-time.Minute).Format(time.RFC3339)}}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	f.failAt["PullHistory"] = 1
	failed, err := readHistoryErr(t, f, githubHistoryRepo, nil)
	if err == nil || failed.HasReach || !failed.Owed {
		t.Fatalf("a first read that failed: err %v, reach %v, owed %v", err, failed.HasReach, failed.Owed)
	}
	h := readHistory(t, f, githubHistoryRepo, failed.Walk)
	if !h.HasReach || !h.Reach.IsZero() || !slices.Contains(actsOf(h), "1 false late L") {
		t.Fatalf("the first read that landed: reach %v %v, acts %v", h.HasReach, h.Reach, actsOf(h))
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
// read leaves the rest as the gap, and its reach stays above it; the next
// read reads on and none of the changes is missed.
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
	busy := readHistory(t, f, githubHistoryRepo, walk)
	if !busy.Owed || !busy.HasReach || !busy.Reach.After(historyNow) {
		t.Fatalf("a read of 2,000 changes: owed %v, reach %v %v", busy.Owed, busy.HasReach, busy.Reach)
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
	if got := historyNumbers(first); !slices.Equal(got, []int64{1, 2, 3, 50}) || first.Owed || !first.HasReach || !first.Reach.IsZero() {
		t.Fatalf("first read: %v, owed %v, reach %v %v", got, first.Owed, first.HasReach, first.Reach)
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
	if !first.Owed || first.HasReach && first.Reach.IsZero() {
		t.Fatalf("first read: owed %v, reach %v %v", first.Owed, first.HasReach, first.Reach)
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

// A steady read reads every event whose id is past the highest the last
// read read: a current comment beneath 100 newer ids imported with times
// hours back is read.
func TestGitLabHistorySteadyReadReadsEveryNewEventID(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}}
	f := historyForge(t, fixture)
	_, walk := readAll(t, f, gitlabHistoryRepo, nil, 4)
	f.core.nowFn = func() time.Time { return historyNow.Add(15 * time.Minute) }
	pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: 5000, Author: "current", AuthorName: "C", Body: "x", CreatedAt: historyNow.Add(time.Minute).Format(time.RFC3339)})
	for i := range 100 {
		pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: int64(6000 + i), Author: fmt.Sprintf("imp%d", i), AuthorName: "I", Body: "x",
			CreatedAt: historyNow.Add(-4 * time.Hour).Format(time.RFC3339)})
	}
	fixture.Repos[0].Pulls = pulls
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	h := readHistory(t, f, gitlabHistoryRepo, walk)
	if !slices.Contains(actsOf(h), "1 false current C") || len(actsOf(h)) != 101 {
		t.Fatalf("the steady read: %d acts, current's among them %v, owed %v", len(actsOf(h)), slices.Contains(actsOf(h), "1 false current C"), h.Owed)
	}
}

// No GitLab read passes gitlabHistoryMaxRequests, a band of one day
// included, however busy the days are (readHistoryErr), and the walk
// reads every commenter.
func TestGitLabHistoryBandsKeepTheBudget(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(1)
	for d := range 12 {
		for i := range 750 {
			pulls[0].Comments = append(pulls[0].Comments, forgefake.Comment{ID: int64(10_000 + d*1000 + i), Author: fmt.Sprintf("u%d", i), AuthorName: "U", Body: "x",
				CreatedAt: historyNow.Add(-time.Duration(d)*24*time.Hour - time.Duration(i)*time.Minute).Format(time.RFC3339)})
		}
	}
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}})
	reads, _ := readAll(t, f, gitlabHistoryRepo, nil, 20)
	if got := len(actsOf(reads...)); got != 750 {
		t.Fatalf("after %d reads: %d commenters of 750", len(reads), got)
	}
	t.Logf("first walk of 9,000 comment events over 12 days: %d reads, %v", len(reads), f.snapshot())
}

// A GitLab read claims a reach only once every stream has one: a first
// read whose comment events failed claims none.
func TestGitLabHistoryClaimsNoReachUntilEveryStreamHasOne(t *testing.T) {
	t.Parallel()
	f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: historyPulls(3)}}})
	f.failAt["REST events"] = 2
	h, err := readHistoryErr(t, f, gitlabHistoryRepo, nil)
	if err == nil || h.HasReach {
		t.Fatalf("a first read whose comment events failed: err %v, reach %v", err, h.HasReach)
	}
	if h = readHistory(t, f, gitlabHistoryRepo, h.Walk); !h.HasReach {
		t.Fatal("the read after claims no reach")
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

// Each pull request carries the start of the request that read it, on
// both forges: with a clock that moves on a minute with each list
// request, a first page's pull requests carry the time before it.
func TestHistoryStampsEachPullRequestAtItsRequestsStart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		repo RepoReference
		list string
	}{{githubHistoryRepo, "PullHistory"}, {gitlabHistoryRepo, "REST merge_requests"}} {
		t.Run(c.repo.Forge, func(t *testing.T) {
			t.Parallel()
			f := historyForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: c.repo.Forge, Project: c.repo.Project(), Pulls: historyPulls(3)}}})
			f.core.nowFn = func() time.Time {
				f.mu.Lock()
				defer f.mu.Unlock()
				return historyNow.Add(time.Duration(f.seen[c.list]) * time.Minute)
			}
			h := readHistory(t, f, c.repo, nil)
			if len(h.PRs) != 3 || f.seen[c.list] != 1 {
				t.Fatalf("%d pull requests in %d list requests", len(h.PRs), f.seen[c.list])
			}
			for _, pr := range h.PRs {
				if !pr.Seen.Equal(historyNow) {
					t.Fatalf("pull request %d seen at %v; want its request's start, %v", pr.Number, pr.Seen, historyNow)
				}
			}
		})
	}
}
