package git

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/harness/forgefake"
)

// keySearchFixture holds, on each forge, pull requests that name WEB-1
// in the title or branch, in every state, one a loose search also finds
// (WEB-12), and a fork's of a branch of the same name.
func keySearchFixture() forgefake.Fixture {
	return forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/web", Pulls: []forgefake.Pull{
			{Number: 1, Title: "WEB-1 Fix the login", HeadRef: "fix-login", Author: "user1", AuthorName: "User 1"},
			{Number: 2, Title: "Cache the session", HeadRef: "WEB-1-cache", State: "merged", Author: "user1"},
			{Number: 3, Title: "Move the host", HeadRef: "feature/WEB-1-host", State: "closed", Draft: true},
			{Number: 4, Title: "WEB-12 Other", HeadRef: "other", Draft: true},
			{Number: 5, Title: "Theirs", HeadRef: "feature/WEB-1-host", HeadRepo: "stranger/web"},
		}},
		{Forge: "gitlab", Project: "grp/web", Pulls: []forgefake.Pull{
			{Number: 6, Title: "WEB-1 Fix the login", HeadRef: "fix-login", State: "merged", Author: "user1", AuthorName: "User 1"},
			{Number: 7, Title: "WEB-12 Other", HeadRef: "other", Draft: true},
			{Number: 8, Title: "Move the host", HeadRef: "feature/WEB-1-host", State: "closed"},
		}},
	}}
}

func foundNumbers(rows []PRFound) []int {
	var out []int
	for _, row := range rows {
		out = append(out, row.Ref.Number)
	}
	return out
}

// GitHub's key search is one request of three aliased searches: the
// title by the key's words, the head branch by its prefix and the
// repository's branches by a substring of their name, the last leaving out
// a fork's branch. Each row carries its state, draft, author and branch.
// What the search finds is looser than the key rule (WEB-12), so the
// caller filters.
func TestGitHubKeySearchReadsTitleHeadAndBranchesInOneRequest(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, keySearchFixture())
	repo := RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}
	got, err := f.core.SearchPRsByKey(t.Context(), repo, "WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	if numbers := foundNumbers(got.Rows); !slices.Equal(numbers, []int{1, 2}) || got.Full {
		t.Fatalf("rows = %v, full %v; want 1 by its title and 2 by its branch", numbers, got.Full)
	}
	want := PRFound{Ref: PRReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web", Number: 1},
		URL: "https://github.com/example/web/pull/1", Title: "WEB-1 Fix the login", State: "open",
		Author: PRListAuthor{Login: "user1", Name: "User 1"}, HeadRefName: "fix-login"}
	if got.Rows[0] != want {
		t.Errorf("row = %+v\nwant %+v", got.Rows[0], want)
	}
	if got.Rows[1].State != "merged" || got.Rows[1].HeadRefName != "WEB-1-cache" {
		t.Errorf("merged row = %+v", got.Rows[1])
	}
	if !slices.Equal(got.Branches, []string{"WEB-1-cache", "feature/WEB-1-host"}) {
		t.Errorf("branches = %v; want the repository's two that hold WEB-1", got.Branches)
	}
	if f.seen["PullsByKey"] != 1 || len(f.seen) != 1 {
		t.Errorf("requests = %v; want one PullsByKey", f.seen)
	}
}

// GitLab's key search is two requests: a list of the merge requests in
// every state by title, and the branches by name.
func TestGitLabKeySearchReadsTitlesInEveryState(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, keySearchFixture())
	repo := RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web"}
	got, err := f.core.SearchPRsByKey(t.Context(), repo, "WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	if numbers := foundNumbers(got.Rows); !slices.Equal(numbers, []int{7, 6}) || got.Full {
		t.Fatalf("rows = %v, full %v; want 7 (its title holds the text) and 6", numbers, got.Full)
	}
	if row := got.Rows[1]; row.State != "merged" || row.Author != (PRListAuthor{Login: "user1", Name: "User 1"}) || row.HeadRefName != "fix-login" ||
		row.URL != "https://gitlab.com/grp/web/-/merge_requests/6" || row.Ref.Number != 6 {
		t.Errorf("row = %+v", row)
	}
	if !got.Rows[0].Draft {
		t.Errorf("draft row = %+v", got.Rows[0])
	}
	var paths []string
	for _, inv := range f.engine.Invocations(0).Invocations {
		paths = append(paths, inv.Path)
	}
	slices.Sort(paths)
	if len(paths) != 2 || paths[0] != "projects/grp%2Fweb/merge_requests?in=title&per_page=100&search=WEB-1&state=all" ||
		paths[1] != "projects/grp%2Fweb/repository/branches?per_page=100&search=WEB-1" {
		t.Errorf("requests = %v; want one title search in every state and one branch search", paths)
	}
	if !slices.Equal(got.Branches, []string{"feature/WEB-1-host"}) {
		t.Errorf("branches = %v; want feature/WEB-1-host", got.Branches)
	}
}

// A search reads on past a page of matches that only look like the key
// (WEB-1000 for WEB-1) to the ones that name it, on both forges: an
// older pull request behind 120 newer loose matches is found.
func TestKeySearchReadsPastLooseMatches(t *testing.T) {
	t.Parallel()
	var github, gitlab []forgefake.Pull
	github = append(github, forgefake.Pull{Number: 1, Title: "Old work", HeadRef: "WEB-1-old"})
	gitlab = append(gitlab, forgefake.Pull{Number: 1, Title: "WEB-1 Old work", HeadRef: "old"})
	for i := 2; i <= 121; i++ {
		github = append(github, forgefake.Pull{Number: i, Title: fmt.Sprintf("Part %d", i), HeadRef: fmt.Sprintf("WEB-1%03d-part", i)})
		gitlab = append(gitlab, forgefake.Pull{Number: i, Title: fmt.Sprintf("WEB-1%03d part", i), HeadRef: fmt.Sprintf("part-%d", i)})
	}
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/many", Pulls: github},
		{Forge: "gitlab", Project: "grp/many", Pulls: gitlab},
	}})
	for _, repo := range []RepoReference{
		{Forge: "github", Host: "github.com", Namespace: "example", Repo: "many"},
		{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "many"},
	} {
		got, err := f.core.SearchPRsByKey(t.Context(), repo, "WEB-1")
		if err != nil || !slices.Contains(foundNumbers(got.Rows), 1) || got.Full {
			t.Errorf("%s: %d rows, 1 among them %v, full %v, %v; want the old one found and the search whole", repo.Forge, len(got.Rows),
				slices.Contains(foundNumbers(got.Rows), 1), got.Full, err)
		}
	}
}

// A search stops after keySearchMaxPages pages and says so on both forges.
func TestKeySearchSaysWhenItStoppedShort(t *testing.T) {
	t.Parallel()
	var github, gitlab []forgefake.Pull
	for i := 1; i <= keySearchMaxPages*100+1; i++ {
		github = append(github, forgefake.Pull{Number: i, Title: fmt.Sprintf("WEB-1 part %d", i), HeadRef: fmt.Sprintf("part-%d", i)})
		gitlab = append(gitlab, forgefake.Pull{Number: i, Title: fmt.Sprintf("WEB-1 part %d", i), HeadRef: fmt.Sprintf("part-%d", i)})
	}
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/many", Pulls: github},
		{Forge: "gitlab", Project: "grp/many", Pulls: gitlab},
	}})
	for _, repo := range []RepoReference{
		{Forge: "github", Host: "github.com", Namespace: "example", Repo: "many"},
		{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "many"},
	} {
		got, err := f.core.SearchPRsByKey(t.Context(), repo, "WEB-1")
		if err != nil || len(got.Rows) != keySearchMaxPages*100 || !got.Full {
			t.Errorf("%s: %d rows, full %v, %v; want %d and full", repo.Forge, len(got.Rows), got.Full, err, keySearchMaxPages*100)
		}
	}
}

// A key that could change the query is refused before any request.
func TestKeySearchRefusesWhatIsNoKey(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, keySearchFixture())
	repo := RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}
	for _, key := range []string{"", `WEB-1" repo:other/x`, "WEB-1 OR x"} {
		if _, err := f.core.SearchPRsByKey(t.Context(), repo, key); err == nil || !strings.Contains(err.Error(), "is not a ticket key") {
			t.Errorf("SearchPRsByKey(%q) = %v; want it refused", key, err)
		}
	}
	if len(f.seen) != 0 || len(f.engine.Invocations(0).Invocations) != 0 {
		t.Errorf("requests = %v", f.seen)
	}
}

// The lookup by branch reads every state, and leaves out another fork's
// branch of the same name, on both forges.
func TestPRsByHeadReadsEveryStateOfTheOriginsBranch(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, keySearchFixture())
	github, gitlab := t.TempDir(), t.TempDir()
	seedForgeCacheGitHubOrigin(t, f.core, github, "https://github.com/example/web.git")
	if forge := f.core.recordOrigin(gitlab, originIdentity{url: "https://gitlab.com/grp/web.git", known: true}, f.core.nowFn()); forge != "gitlab" {
		t.Fatalf("seeded GitLab origin classified as %q", forge)
	}
	for cwd, want := range map[string]PRFound{
		github: {Ref: PRReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web", Number: 3},
			URL: "https://github.com/example/web/pull/3", Title: "Move the host", State: "closed", Draft: true,
			Author: PRListAuthor{Login: "ao-author"}, HeadRefName: "feature/WEB-1-host"},
		gitlab: {Ref: PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web", Number: 8},
			URL: "https://gitlab.com/grp/web/-/merge_requests/8", Title: "Move the host", State: "closed",
			Author: PRListAuthor{Login: "ao-author"}, HeadRefName: "feature/WEB-1-host"},
	} {
		got, err := f.core.ListPRsByHead(t.Context(), cwd, "feature/WEB-1-host")
		if err != nil || len(got) != 1 || got[0] != want {
			t.Errorf("%s: ListPRsByHead = %+v, %v\nwant %+v", want.Ref.Forge, got, err, want)
		}
	}
	open, err := f.core.ListOpenPRs(t.Context(), github, "feature/WEB-1-host")
	if err != nil || len(open) != 0 {
		t.Errorf("ListOpenPRs = %+v, %v; want none (the open one is a fork's)", open, err)
	}
}

// A search whose request fails answers what it read before the failure
// beside the error: GitHub the pages before the one that failed, GitLab
// the title search beside its failed branch search.
func TestKeySearchKeepsWhatItReadBeforeAFailure(t *testing.T) {
	t.Parallel()
	var github []forgefake.Pull
	for i := 1; i <= 150; i++ {
		github = append(github, forgefake.Pull{Number: i, Title: fmt.Sprintf("WEB-1 part %d", i), HeadRef: fmt.Sprintf("part-%d", i)})
	}
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "example/many", Pulls: github},
		{Forge: "gitlab", Project: "grp/web", Pulls: []forgefake.Pull{{Number: 1, Title: "WEB-1 Fix the API", HeadRef: "fix-api"}}},
	}})
	f.mu.Lock()
	f.failAt["PullsByKey"] = 2
	f.mu.Unlock()
	got, err := f.core.SearchPRsByKey(t.Context(), RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "many"}, "WEB-1")
	if err == nil || len(got.Rows) != 100 {
		t.Errorf("github: %d rows, %v; want the first page's 100 beside the second page's failure", len(got.Rows), err)
	}
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "glab api branch list", Status: 403, Body: []byte(`{"message":"403 Forbidden"}`)}); err != nil {
		t.Fatal(err)
	}
	got, err = f.core.SearchPRsByKey(t.Context(), RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "web"}, "WEB-1")
	if err == nil || !slices.Equal(foundNumbers(got.Rows), []int{1}) {
		t.Errorf("gitlab: rows %v, %v; want 1 beside the branch search's failure", foundNumbers(got.Rows), err)
	}
}

// A GitHub answer whose error names one of its searches keeps the others
// of the same answer: the title search answered, the branch read was
// refused.
func TestKeySearchKeepsTheSearchesBesideAFieldError(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "example/web"}}})
	empty := `{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}`
	body := `{"data":{"byTitle":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"url":"https://github.com/example/web/pull/1","number":1,` +
		`"title":"WEB-1 Fix the login","state":"OPEN","isDraft":false,"headRefName":"fix-login","author":{"login":"user1","name":"User 1"}}]},` +
		`"byHead":` + empty + `,"repository":null},` +
		`"errors":[{"type":"FORBIDDEN","path":["repository"],"message":"Resource not accessible by personal access token"}]}`
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql PullsByKey", Status: 200, Body: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	got, err := f.core.SearchPRsByKey(t.Context(), RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}, "WEB-1")
	if err == nil || !slices.Equal(foundNumbers(got.Rows), []int{1}) {
		t.Errorf("rows %v, %v; want 1 beside the branch read's refusal", foundNumbers(got.Rows), err)
	}
}

// A lookup by branch whose parent's read fails answers the origin's pull
// requests beside the error, on both forges.
func TestPRsByHeadKeepsTheOriginsBesideTheParentsFailure(t *testing.T) {
	t.Parallel()
	f := newFactsForge(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "team/up", Pulls: []forgefake.Pull{{Number: 2, Title: "Fix it", HeadRef: "WEB-1", HeadRepo: "alice/up"}}},
		{Forge: "github", Project: "alice/up", Parent: "team/up", Pulls: []forgefake.Pull{{Number: 1, Title: "Try it", HeadRef: "WEB-1", State: "closed"}}},
		{Forge: "gitlab", Project: "grp/up", Pulls: []forgefake.Pull{{Number: 2, Title: "Fix it", HeadRef: "WEB-1", HeadRepo: "alice/up"}}},
		{Forge: "gitlab", Project: "alice/up", Parent: "grp/up", Pulls: []forgefake.Pull{{Number: 1, Title: "Try it", HeadRef: "WEB-1", State: "merged"}}},
	}})
	github, gitlab := t.TempDir(), t.TempDir()
	seedForgeCacheGitHubOrigin(t, f.core, github, "https://github.com/alice/up.git")
	if forge := f.core.recordOrigin(gitlab, originIdentity{url: "https://gitlab.com/alice/up.git", known: true}, f.core.nowFn()); forge != "gitlab" {
		t.Fatalf("seeded GitLab origin classified as %q", forge)
	}
	f.mu.Lock()
	f.failAt["PRsByHead"] = 2
	f.mu.Unlock()
	if err := f.engine.SetRefusal(forgefake.Refusal{Route: "glab repository identity", Status: 403, Body: []byte(`{"message":"403 Forbidden"}`)}); err != nil {
		t.Fatal(err)
	}
	for forge, cwd := range map[string]string{"github": github, "gitlab": gitlab} {
		got, err := f.core.ListPRsByHead(t.Context(), cwd, "WEB-1")
		if err == nil || len(got) != 1 || got[0].Ref.Namespace != "alice" || got[0].Ref.Number != 1 {
			t.Errorf("%s: ListPRsByHead = %+v, %v; want alice/up's 1 beside the parent's failure", forge, got, err)
		}
	}
}
