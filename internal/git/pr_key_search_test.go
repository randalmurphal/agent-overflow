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

// GitHub's key search is one request of two aliased searches: the title
// by the key's words and the head branch by its prefix. Each row carries
// its state, draft, author and branch. What the search finds is looser than the key rule (WEB-12), so
// the caller filters.
func TestGitHubKeySearchReadsTitleAndHeadInOneRequest(t *testing.T) {
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
	if f.seen["PullsByKey"] != 1 || len(f.seen) != 1 {
		t.Errorf("requests = %v; want one PullsByKey", f.seen)
	}
}

// GitLab's key search is one list request of the title in every state.
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
	if len(paths) != 1 || !strings.Contains(paths[0], "search=WEB-1") || !strings.Contains(paths[0], "in=title") || !strings.Contains(paths[0], "state=all") {
		t.Errorf("requests = %v; want one title search in every state", paths)
	}
}

// A search past its one page of 100 says so on both forges.
func TestKeySearchSaysWhenItsPageIsFull(t *testing.T) {
	t.Parallel()
	var github, gitlab []forgefake.Pull
	for i := 1; i <= 101; i++ {
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
		if err != nil || len(got.Rows) != 100 || !got.Full {
			t.Errorf("%s: %d rows, full %v, %v; want the first 100 and full", repo.Forge, len(got.Rows), got.Full, err)
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
