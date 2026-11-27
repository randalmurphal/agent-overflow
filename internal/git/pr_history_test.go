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

func readHistory(t *testing.T, f *factsForge, repo RepoReference, walk []byte, known func([]int64) (map[int64]bool, error)) PRHistory {
	t.Helper()
	if known == nil {
		known = func([]int64) (map[int64]bool, error) { return map[int64]bool{}, nil }
	}
	h, err := f.core.ReadPRHistory(t.Context(), repo, PRHistoryInput{Walk: walk, WindowStart: historyNow.AddDate(0, 0, -180), Known: known})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

var githubHistoryRepo = RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}

// GitHub's first read walks back from the newest pull request, 50 a
// page, at most PRHistoryPages pages; the next read reads down to the
// newest it had read and walks on, and the read is whole once it reaches
// the window's start or the oldest pull request. Each pull request comes
// with who approved and commented, a bot and a pending review left out.
func TestGitHubHistoryWalksBackInBoundedReads(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(620)
	pulls[len(pulls)-1].Reviews = []forgefake.Review{
		{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(1)},
		{Author: "user3", AuthorName: "User 3", State: "CHANGES_REQUESTED", SubmittedAt: historyAt(1)},
		{Author: "ci", State: "COMMENTED", SubmittedAt: historyAt(1)},
		{Author: "user4", AuthorName: "User 4", State: "DISMISSED", SubmittedAt: historyAt(1)},
	}
	pulls[len(pulls)-1].Comments = []forgefake.Comment{{Author: "user5", AuthorName: "User 5", Body: "ok", CreatedAt: historyAt(1)}}
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})

	first := readHistory(t, f, githubHistoryRepo, nil, nil)
	if f.seen["PullHistory"] != PRHistoryPages || first.Walked || len(historyNumbers(first)) != 500 {
		t.Fatalf("first read: %d pages, walked %v, %d pull requests", f.seen["PullHistory"], first.Walked, len(historyNumbers(first)))
	}
	if want := historyNow.Add(-500 * time.Hour); !first.Reach.Equal(want) {
		t.Fatalf("reach = %v, want the oldest update read, %v", first.Reach, want)
	}
	newest := first.PRs[0]
	if newest.Number != 1 || newest.State != "open" || newest.Author.Login != "user1" || !newest.ActsWhole || newest.HeadRepo == "" || newest.HeadRepo != first.OwnHead {
		t.Fatalf("pull request 1 = %+v (own head %q)", newest, first.OwnHead)
	}
	var acts []string
	for _, a := range newest.Acts {
		acts = append(acts, fmt.Sprintf("%v %s", a.Approved, a.Person.Login))
	}
	if !slices.Equal(acts, []string{"true user2", "false user3", "false user5"}) {
		t.Fatalf("acts = %v", acts)
	}

	second := readHistory(t, f, githubHistoryRepo, first.Walk, nil)
	if got := historyNumbers(second); !second.Walked || !second.Reach.IsZero() || len(got) < 120 || got[len(got)-1] != 620 {
		t.Fatalf("second read: walked %v, reach %v, %d pull requests", second.Walked, second.Reach, len(got))
	}
	// One page down to the newest read, then the gap's three.
	if f.seen["PullHistory"] != PRHistoryPages+4 {
		t.Fatalf("pages after two reads = %d", f.seen["PullHistory"])
	}

	// A steady read: one page, which finds pull request 300 updated since.
	pulls[len(pulls)-300].UpdatedAt = historyNow.Add(time.Minute).Format(time.RFC3339)
	pulls[len(pulls)-300].Title = "WEB-300 Renamed"
	if _, err := f.engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}}); err != nil {
		t.Fatal(err)
	}
	third := readHistory(t, f, githubHistoryRepo, second.Walk, nil)
	if f.seen["PullHistory"] != PRHistoryPages+5 || !third.Walked || third.PRs[0].Number != 300 || third.PRs[0].Title != "WEB-300 Renamed" {
		t.Fatalf("steady read: %d pages, first %+v", f.seen["PullHistory"], third.PRs[0])
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
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web", Pulls: pulls}}})
	h := readHistory(t, f, githubHistoryRepo, nil, nil)
	if len(h.PRs) != 1 || len(h.PRs[0].Acts) != 130 || !h.PRs[0].ActsWhole || f.seen["PullHistoryMore"] != 1 {
		t.Fatalf("acts %d, whole %v, follow-ups %d", len(h.PRs[0].Acts), h.PRs[0].ActsWhole, f.seen["PullHistoryMore"])
	}
}

// A fork's history names its parent, and a parent's pull request names
// the fork as its head.
func TestGitHubHistoryNamesTheParentAndTheHead(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Parent: "up/web", Pulls: historyPulls(1)},
		{Forge: "github", Project: "up/web", Pulls: []forgefake.Pull{
			{Number: 9, Title: "WEB-1 From the fork", HeadRef: "web-1", HeadRepo: "example/web", UpdatedAt: historyAt(1), CreatedAt: historyAt(2)},
			{Number: 10, Title: "Theirs", HeadRef: "web-1", UpdatedAt: historyAt(1), CreatedAt: historyAt(2)},
		}},
	}})
	fork := readHistory(t, f, githubHistoryRepo, nil, nil)
	if fork.Parent == nil || fork.Parent.Key() != "github:up/web" {
		t.Fatalf("parent = %+v", fork.Parent)
	}
	parent := readHistory(t, f, *fork.Parent, nil, nil)
	heads := map[int64]string{}
	for _, pr := range parent.PRs {
		heads[pr.Number] = pr.HeadRepo
	}
	if heads[9] != fork.OwnHead || heads[10] == fork.OwnHead {
		t.Fatalf("heads = %v, fork %q", heads, fork.OwnHead)
	}
}

var gitlabHistoryRepo = RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web"}

// GitLab's first read walks the merge request list and both events feeds
// back from the newest, and fetches a merge request an event names that
// the list did not reach: one approved now but last updated before the
// window. A steady read is one page of each stream, which finds a new
// approval that did not move the merge request's update time.
func TestGitLabHistoryReadsTheListAndTheEvents(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(3)
	pulls[2].Reviews = []forgefake.Review{{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(1)}}
	pulls[2].Comments = []forgefake.Comment{{Author: "user3", AuthorName: "User 3", Body: "ok", CreatedAt: historyAt(2)}}
	old := forgefake.Pull{Number: 50, Title: "WEB-50 Old", HeadRef: "web-50", CreatedAt: historyAt(24 * 300), UpdatedAt: historyAt(24 * 200),
		Reviews: []forgefake.Review{{Author: "user2", AuthorName: "User 2", State: "APPROVED", SubmittedAt: historyAt(3)}}}
	pulls = append([]forgefake.Pull{old}, pulls...)
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}}
	f := newFactsForge(t, fixture)
	f.core.nowFn = func() time.Time { return historyNow }

	first := readHistory(t, f, gitlabHistoryRepo, nil, nil)
	if got := historyNumbers(first); !slices.Equal(got, []int64{1, 2, 3, 50}) || !first.Walked || !first.Reach.IsZero() {
		t.Fatalf("first read: %v, walked %v, reach %v", got, first.Walked, first.Reach)
	}
	var acts []string
	for _, a := range first.Acts {
		acts = append(acts, fmt.Sprintf("%d %v %s", a.Number, a.Approved, a.Person.Login))
	}
	slices.Sort(acts)
	if !slices.Equal(acts, []string{"1 false user3", "1 true user2", "50 true user2"}) {
		t.Fatalf("acts = %v", acts)
	}
	firstSeen := maps.Clone(f.seen)

	pulls[3].Reviews = append(pulls[3].Reviews, forgefake.Review{Author: "user4", AuthorName: "User 4", State: "APPROVED", SubmittedAt: historyAt(0)})
	if _, err := f.engine.Seed(fixture); err != nil {
		t.Fatal(err)
	}
	second := readHistory(t, f, gitlabHistoryRepo, first.Walk, func([]int64) (map[int64]bool, error) { return map[int64]bool{1: true}, nil })
	acts = nil
	for _, a := range second.Acts {
		acts = append(acts, fmt.Sprintf("%d %v %s", a.Number, a.Approved, a.Person.Login))
	}
	if !slices.Contains(acts, "1 true user4") {
		t.Fatalf("steady read acts = %v", acts)
	}
	steady := map[string]int{}
	for key, n := range f.seen {
		steady[key] = n - firstSeen[key]
	}
	if steady["REST merge_requests"] != 1 || steady["REST events"] != 2 {
		t.Fatalf("steady read requests = %v", steady)
	}
}

// More merge requests sharing one update time than a page holds stop the
// read with a failure that says so, rather than a read that never moves.
func TestGitLabHistoryRefusesAPageOfOneUpdateTime(t *testing.T) {
	t.Parallel()
	pulls := historyPulls(101)
	for i := range pulls {
		pulls[i].UpdatedAt = historyAt(1)
	}
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/web", Pulls: pulls}}})
	f.core.nowFn = func() time.Time { return historyNow }
	_, err := f.core.ReadPRHistory(t.Context(), gitlabHistoryRepo, PRHistoryInput{WindowStart: historyNow.AddDate(0, 0, -180),
		Known: func([]int64) (map[int64]bool, error) { return nil, nil }})
	if err == nil || !strings.Contains(err.Error(), "share the update time") {
		t.Fatalf("err = %v", err)
	}
}
