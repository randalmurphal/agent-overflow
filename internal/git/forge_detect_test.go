package git

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

func TestClassifyOriginURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		// GitHub — recognised.
		{"github https", "https://github.com/owner/repo", "github"},
		{"github https with .git", "https://github.com/owner/repo.git", "github"},
		{"github http", "http://github.com/owner/repo", "github"},
		{"github ssh alias", "git@github.com:owner/repo.git", "github"},
		{"github ssh url", "ssh://git@github.com/owner/repo.git", "github"},
		{"github ssh url with port", "ssh://git@github.com:22/owner/repo.git", "github"},
		{"github git protocol", "git://github.com/owner/repo.git", "github"},
		{"github mixed case", "https://GitHub.com/owner/repo", "github"},

		// GitLab — recognised.
		{"gitlab https", "https://gitlab.com/group/repo", "gitlab"},
		{"gitlab https subgroup", "https://gitlab.com/group/sub/repo", "gitlab"},
		{"gitlab https with .git", "https://gitlab.com/group/repo.git", "gitlab"},
		{"gitlab ssh alias", "git@gitlab.com:group/repo.git", "gitlab"},
		{"gitlab ssh alias subgroup", "git@gitlab.com:group/sub/repo.git", "gitlab"},
		{"gitlab ssh url", "ssh://git@gitlab.com/group/repo.git", "gitlab"},

		// Unrecognised → "".
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"bitbucket", "https://bitbucket.org/owner/repo", ""},
		{"gitea self-hosted", "https://git.example.com/owner/repo", ""},
		{"github subdomain", "https://api.github.com/owner/repo", ""},
		{"gitlab subdomain", "https://about.gitlab.com/group/repo", ""},
		{"plain ssh server", "user@example.com:repo.git", ""},
		{"local bare path", "/var/lib/git/repo.git", ""},

		// Lookalike-host rejections (security regression guards).
		{"github lookalike domain", "https://evilgithub.com/owner/repo", ""},
		{"github subdomain attacker", "https://github.com.attacker.com/owner/repo", ""},
		{"gitlab lookalike domain", "https://evilgitlab.com/group/repo", ""},
		// `@` in path must NOT be mistaken for userinfo when the path is
		// inside the host segment. https://github.com/foo@bar/repo is a
		// valid github URL with `foo@bar` as the namespace; the host
		// must remain github.com.
		{"github with at-sign in path", "https://github.com/foo@bar/repo", "github"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOriginURL(tc.url); got != tc.want {
				t.Errorf("classifyOriginURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestExtractRemoteHost(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://github.com/owner/repo", "github.com"},
		{"https://user@github.com/owner/repo", "github.com"},
		{"https://github.com:443/owner/repo", "github.com"},
		{"git@github.com:owner/repo.git", "github.com"},
		{"ssh://git@github.com:22/owner/repo.git", "github.com"},
		{"git://github.com/owner/repo", "github.com"},
		{"https://GitLab.com/group/repo", "gitlab.com"},
		{"  https://github.com/owner/repo  ", "github.com"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := extractRemoteHost(tc.input); got != tc.want {
			t.Errorf("extractRemoteHost(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDetectForgeReadsOriginURL(t *testing.T) {
	cases := []struct {
		name      string
		originURL string
		want      string
	}{
		{"github", "https://github.com/owner/repo.git", "github"},
		{"gitlab", "git@gitlab.com:group/repo.git", "gitlab"},
		{"self-hosted", "https://git.example.com/owner/repo", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.InitGitRepo(t)
			testutil.RunGit(t, repo, "remote", "add", "origin", tc.originURL)

			core := NewCore()
			if got := core.DetectForge(repo); got != tc.want {
				t.Errorf("DetectForge() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectForgeReturnsEmptyForNoOrigin(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	if got := core.DetectForge(repo); got != "" {
		t.Errorf("DetectForge() with no origin = %q, want empty", got)
	}
}

func TestDetectForgeReturnsEmptyForNonRepo(t *testing.T) {
	core := NewCore()
	if got := core.DetectForge(t.TempDir()); got != "" {
		t.Errorf("DetectForge() on non-repo = %q, want empty", got)
	}
}

func TestDetectForgeReturnsEmptyForEmptyCwd(t *testing.T) {
	core := NewCore()
	if got := core.DetectForge(""); got != "" {
		t.Errorf("DetectForge(\"\") = %q, want empty", got)
	}
}

// TestDetectForgeCachesResults pins the perf optimisation that repeated
// DetectForge calls within forgeDetectionTTL do NOT shell out for
// `git remote get-url origin` again. The cache mirrors prCache's
// discipline — same TTL pattern, same nowFn override for tests.
func TestDetectForgeCachesResults(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")

	core := NewCore()
	cwd, err := filepath.Abs(repo)
	if err != nil {
		t.Fatalf("Abs(): %v", err)
	}

	// First call: cold cache → shell out + cache.
	if got := core.DetectForge(cwd); got != "github" {
		t.Fatalf("first DetectForge = %q, want github", got)
	}

	// Mutate the origin URL on disk; the cache must still return the
	// previously-classified value because the TTL hasn't elapsed.
	testutil.RunGit(t, repo, "remote", "set-url", "origin", "https://gitlab.com/owner/repo.git")
	if got := core.DetectForge(cwd); got != "github" {
		t.Errorf("warm DetectForge after URL change = %q, want github (cached)", got)
	}

	// Drive nowFn past the TTL → cache miss → re-shell-out → new value.
	core.nowFn = func() time.Time { return time.Now().Add(forgeDetectionTTL + time.Second) }
	if got := core.DetectForge(cwd); got != "gitlab" {
		t.Errorf("post-TTL DetectForge = %q, want gitlab", got)
	}
}

func TestStoreForgeCacheReplacesExisting(t *testing.T) {
	core := NewCore()
	cwd := t.TempDir()
	now := core.nowFn()

	core.storeForgeCache(cwd, "github", now)
	if got := core.DetectForge(cwd); got != "github" {
		t.Fatalf("DetectForge after seed = %q, want github", got)
	}

	core.storeForgeCache(cwd, "gitlab", now)
	if got := core.DetectForge(cwd); got != "gitlab" {
		t.Fatalf("DetectForge after re-seed = %q, want gitlab", got)
	}
}

// TestInvalidateForgeCache pins the contract that the public
// invalidation API drops the cached entry for cwd, so the next
// DetectForge call re-runs origin URL classification.
func TestInvalidateForgeCache(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")

	core := NewCore()
	if got := core.DetectForge(repo); got != "github" {
		t.Fatalf("first DetectForge = %q, want github", got)
	}

	// Mutate origin behind DetectForge's cache.
	testutil.RunGit(t, repo, "remote", "set-url", "origin", "https://gitlab.com/owner/repo.git")

	// Without invalidation the cache wins (verified in
	// TestDetectForgeCachesResults). With invalidation the next call
	// re-classifies.
	core.InvalidateForgeCache(repo)
	if got := core.DetectForge(repo); got != "gitlab" {
		t.Errorf("DetectForge after InvalidateForgeCache = %q, want gitlab", got)
	}
}

// TestInvalidateForgeCacheIsScopedToCwd confirms invalidation only
// drops the targeted cwd's entry.
func TestInvalidateForgeCacheIsScopedToCwd(t *testing.T) {
	core := NewCore()
	now := core.nowFn()
	core.storeForgeCache("/repo/a", "github", now)
	core.storeForgeCache("/repo/b", "gitlab", now)

	core.InvalidateForgeCache("/repo/a")

	core.forgeCacheMu.RLock()
	_, hasA := core.forgeCache["/repo/a"]
	entryB, hasB := core.forgeCache["/repo/b"]
	core.forgeCacheMu.RUnlock()

	if hasA {
		t.Error("InvalidateForgeCache did not drop /repo/a")
	}
	if !hasB || entryB.forge != "gitlab" {
		t.Error("InvalidateForgeCache should not have touched /repo/b")
	}
}

// TestForgeForReturnsNullForUnsupported verifies the dispatch contract
// that drives the "self-hosted GitLab" UX: Core.forgeFor returns a
// nullForge whose every operation surfaces ErrUnsupportedForge.
func TestForgeForReturnsNullForUnsupported(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://git.example.com/owner/repo.git")

	core := NewCore()
	f := core.forgeFor(repo)
	if f.ID() != "" {
		t.Errorf("ID() = %q, want empty (nullForge)", f.ID())
	}
}

func TestForgeForReturnsGitHubForGitHubOrigin(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")

	core := NewCore()
	if got := core.forgeFor(repo).ID(); got != "github" {
		t.Errorf("forgeFor() = %q, want github", got)
	}
}
