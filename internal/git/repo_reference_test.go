package git

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestRepoReferenceKeyIsThePRKeyWithoutItsNumber(t *testing.T) {
	t.Parallel()
	for _, pr := range []PRReference{
		{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 5},
		{Forge: "gitlab", Host: "gitlab.com", Namespace: "group/sub", Repo: "repo", Number: 3},
		{Forge: "github", Host: "ghe.example.com", Namespace: "owner", Repo: "repo", Number: 5},
		{Forge: "gitlab", Host: "gitlab.example.com:8443", Namespace: "group/sub", Repo: "repo", Number: 3},
		{Forge: "gitlab", Host: "github.com", Namespace: "group", Repo: "repo", Number: 1},
	} {
		repo := pr.Repository()
		if repo != (RepoReference{Forge: pr.Forge, Host: pr.Host, Namespace: pr.Namespace, Repo: pr.Repo}) {
			t.Fatalf("Repository(%+v) = %+v", pr, repo)
		}
		want := strings.TrimSuffix(pr.Key(), ":"+strconv.Itoa(pr.Number))
		if got := repo.Key(); got != want {
			t.Errorf("Key(%+v) = %q, want %q", repo, got, want)
		}
		if repo.Project() != pr.Project() {
			t.Errorf("Project(%+v) = %q, want %q", repo, repo.Project(), pr.Project())
		}
	}
	if got := (RepoReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo"}).Key(); got != "github:owner/repo" {
		t.Errorf("public GitHub key = %q", got)
	}
}

func TestRepoReferenceValidate(t *testing.T) {
	t.Parallel()
	good := []RepoReference{
		{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo"},
		{Forge: "gitlab", Host: "gitlab.example.com:8443", Namespace: "group/sub", Repo: "repo"},
	}
	for _, repo := range good {
		if err := repo.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", repo, err)
		}
	}
	bad := map[string]RepoReference{
		"unknown forge":    {Forge: "bitbucket", Host: "bitbucket.org", Namespace: "o", Repo: "r"},
		"no host":          {Forge: "github", Namespace: "o", Repo: "r"},
		"uppercase host":   {Forge: "github", Host: "GitHub.com", Namespace: "o", Repo: "r"},
		"github subgroups": {Forge: "github", Host: "github.com", Namespace: "o/sub", Repo: "r"},
		"no namespace":     {Forge: "gitlab", Host: "gitlab.com", Repo: "r"},
		"colon segment":    {Forge: "gitlab", Host: "gitlab.com", Namespace: "g:x", Repo: "r"},
	}
	for name, repo := range bad {
		if err := repo.Validate(); err == nil {
			t.Errorf("%s: Validate(%+v) accepted it", name, repo)
		}
	}
	if err := bad["unknown forge"].Validate(); !errors.Is(err, ErrUnsupportedForge) {
		t.Errorf("unknown forge error = %v, want ErrUnsupportedForge", err)
	}
}

func TestOriginRepositoryReadsTheCheckoutsOrigin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		origin string
		want   RepoReference
	}{
		{"https://github.com/Owner/Repo.git", RepoReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo"}},
		{"git@gitlab.com:group/sub/tool.git", RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "group/sub", Repo: "tool"}},
		{"ssh://git@ssh.github.com:443/acme/widgets", RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets"}},
	}
	for _, tc := range cases {
		repo := initGitRepo(t)
		testutil.RunGit(t, repo, "remote", "add", "origin", tc.origin)
		got, err := NewCore().OriginRepository(t.Context(), repo)
		if err != nil || got != tc.want {
			t.Errorf("OriginRepository(%s) = %+v, %v; want %+v", tc.origin, got, err, tc.want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("OriginRepository(%s) answered an invalid reference: %v", tc.origin, err)
		}
	}
}

func TestOriginRepositoryRefusesACheckoutWithNoForgeOrigin(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"not a forge":        "https://example.com/acme/repo.git",
		"no repository path": "https://github.com/acme",
		"local path":         "/srv/git/repo.git",
	}
	for name, origin := range cases {
		repo := initGitRepo(t)
		testutil.RunGit(t, repo, "remote", "add", "origin", origin)
		_, err := NewCore().OriginRepository(t.Context(), repo)
		var unknown *OriginUnknownError
		if !errors.Is(err, ErrOriginNotForge) || !errors.As(err, &unknown) || unknown.Cwd != repo {
			t.Errorf("%s: OriginRepository = %v, want an *OriginUnknownError matching ErrOriginNotForge", name, err)
		}
	}
	noOrigin := initGitRepo(t)
	if _, err := NewCore().OriginRepository(t.Context(), noOrigin); !errors.Is(err, ErrOriginNotForge) {
		t.Errorf("no origin: OriginRepository = %v, want ErrOriginNotForge", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewCore().OriginRepository(ctx, noOrigin); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: OriginRepository = %v, want context.Canceled", err)
	}
}
