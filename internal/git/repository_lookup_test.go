package git

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/testutil/mockexec"
)

func TestRepositoryLookupUsesForgeIdentityAndCaches(t *testing.T) {
	t.Parallel()
	input := func(host string) RepoIdentity {
		return RepoIdentity{Repository: true, RemoteURL: "https://user:SECRET@" + host + "/owner/repo?token=SECRET"}
	}
	t.Run("github.com", func(t *testing.T) {
		t.Parallel()
		c, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if call.Method != http.MethodGet || call.Path != "repos/owner/repo" || call.Host != "github.com" {
				return forgeUnexpected(t, call)
			}
			return forgeAPIAnswer{Body: `{"id":123,"full_name":"owner/repo"}`}
		})
		first := c.ResolveRepository(context.Background(), t.TempDir(), input("github.com"))
		second := c.ResolveRepository(context.Background(), t.TempDir(), input("github.com"))
		if first.RepositoryID != "github:github.com:123" || second.RepositoryID != first.RepositoryID || first.LookupError != "" || first.RemoteURL != "" || first.IdentitySource == "" {
			t.Fatalf("identity %+v %+v", first, second)
		}
		if sent := calls.all(); len(sent) != 1 || strings.Contains(sent[0].Path, "SECRET") {
			t.Fatalf("requests %+v, want one", sent)
		}
	})
	t.Run("gitlab.com", func(t *testing.T) {
		t.Parallel()
		c, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if call.Method != http.MethodGet || call.Path != "projects/owner%2Frepo" || call.Host != "gitlab.com" {
				return forgeUnexpected(t, call)
			}
			return forgeAPIAnswer{Body: `{"id":123,"path_with_namespace":"owner/repo"}`}
		})
		first := c.ResolveRepository(context.Background(), t.TempDir(), input("gitlab.com"))
		second := c.ResolveRepository(context.Background(), t.TempDir(), input("gitlab.com"))
		if first.RepositoryID != "gitlab:gitlab.com:123" || second.RepositoryID != first.RepositoryID || first.LookupError != "" || first.RemoteURL != "" || first.IdentitySource == "" {
			t.Fatalf("identity %+v %+v", first, second)
		}
		if sent := calls.all(); len(sent) != 1 || sent[0].Header.Get("Private-Token") != "test-token" {
			t.Fatalf("requests %+v, want one", sent)
		}
	})
}

// The GitHub identity read is a transport request: a failure is retryable
// unless gh is missing, and its log line carries the status, never the
// response body.
func TestRepositoryLookupGitHubFailures(t *testing.T) {
	var logged strings.Builder
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	c, _ := newForgeAPICore(t, func(forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"BODY"}`}
	})
	failed := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{Repository: true, RemoteURL: "https://github.com/a/b"})
	if failed.LookupError == "" || !failed.LookupRetryable || failed.RepositoryID != "" {
		t.Fatalf("HTTP 500: %+v, want a retryable error", failed)
	}
	if line := logged.String(); strings.Count(line, "\n") != 1 || !strings.Contains(line, "lookup with the gh login on github.com failed: GET") || !strings.Contains(line, "HTTP 500") || strings.Contains(line, "BODY") {
		t.Fatalf("logged %q, want one line with the status and no body", line)
	}

	missing, err := forgeapi.New(forgeapi.Options{Version: "test", TokenSource: missingTokenSource{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(missing.Close)
	result := NewCore(WithForgeAPI(missing)).ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{Repository: true, RemoteURL: "https://github.com/a/b"})
	if result.LookupError == "" || result.LookupRetryable {
		t.Fatalf("missing gh: %+v, want a non-retryable error", result)
	}
}

// missingTokenSource is a forge whose CLI is not installed: the token
// read fails before any request is built.
type missingTokenSource struct{}

func (missingTokenSource) Token(_ context.Context, forge, _ string) ([]byte, forgeapi.SourceInfo, error) {
	return nil, forgeapi.SourceInfo{}, forgeapi.MissingCLIError(forge, nil)
}

// gitlabLookupForge is an httptest GitLab answering the identity read of
// gitlab.com/a/b (and a/c) by answer, counting the reads. While block is
// set a read is held until unblock (or the end of the test).
type gitlabLookupForge struct {
	reads    atomic.Int32
	answer   atomic.Value // forgeAPIAnswer
	block    atomic.Bool
	released chan struct{}
	unblock  func()
}

func newGitLabLookupCore(t *testing.T) (*Core, *gitlabLookupForge) {
	t.Helper()
	forge := &gitlabLookupForge{released: make(chan struct{})}
	forge.unblock = sync.OnceFunc(func() { close(forge.released) })
	forge.answer.Store(forgeAPIAnswer{Body: `{"id":123}`})
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path != "projects/a%2Fb" && call.Path != "projects/a%2Fc" {
			return forgeUnexpected(t, call)
		}
		forge.reads.Add(1)
		if forge.block.Load() {
			<-forge.released
		}
		return forge.answer.Load().(forgeAPIAnswer)
	})
	// Registered after the server, so it runs first: a held read must end
	// before the server can close.
	t.Cleanup(forge.unblock)
	return core, forge
}

func TestRepositoryLookupFailureAndCancellation(t *testing.T) {
	t.Parallel()
	c, forge := newGitLabLookupCore(t)
	forge.answer.Store(forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"https://user:SECRET@gitlab.com/a/b"}`})
	identity := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{RemoteURL: "https://gitlab.com/a/b"})
	if identity.LookupError == "" || strings.Contains(identity.LookupError, "SECRET") || identity.RepositoryID != "" {
		t.Fatalf("failure %+v", identity)
	}
	forge.answer.Store(forgeAPIAnswer{Body: `{"id":123}`})
	forge.block.Store(true)
	delete(c.repositoryLookups.cache, "gitlab:gitlab.com/a/b")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	identity = c.ResolveRepository(ctx, t.TempDir(), RepoIdentity{RemoteURL: "https://gitlab.com/a/b"})
	if time.Since(start) > 3*time.Second || ctx.Err() == nil || identity.RepositoryID != "" {
		t.Fatal("lookup did not cancel")
	}
	forge.block.Store(false)
	if result := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{RemoteURL: "https://gitlab.com/a/b"}); result.RepositoryID == "" {
		t.Fatalf("caller cancellation poisoned the cache: %+v", result)
	}
}

func TestRepositoryLookupResolvesSSHHostOnOwningComputer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "tool")
	mockexec.Write(t, fake, "#!/bin/sh\nif [ \"$AO_FORGE_CLI\" = ssh ]; then printf 'hostname github.com\\n'; else exit 1; fi\n")
	api, _ := newForgeAPIService(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path != "repos/owner/repo" || call.Host != "github.com" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: `{"id":345}`}
	})
	c := NewCore(WithIsolatedForgeCLIs(fake, nil), WithForgeAPI(api))
	result := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: "git@github-work:Owner/Repo.git"})
	if result.RepositoryID != "github:github.com:345" || result.RemoteURL != "" || result.LookupError != "" {
		t.Fatalf("alias: %+v", result)
	}
}
func TestRepositoryLookupConcurrentCallsAndBoundedCache(t *testing.T) {
	t.Parallel()
	c, forge := newGitLabLookupCore(t)
	forge.answer.Store(forgeAPIAnswer{Body: `{"id":789}`})
	forge.block.Store(true)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{RemoteURL: "https://gitlab.com/a/b"})
			if result.RepositoryID != "gitlab:gitlab.com:789" {
				t.Errorf("lookup: %+v", result)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	forge.unblock()
	wg.Wait()
	if n := forge.reads.Load(); n != 1 {
		t.Fatalf("reads = %d, want one shared read", n)
	}
	for i := range 128 {
		c.repositoryLookups.cache[fmt.Sprint(i)] = repositoryLookupResult{id: "old", until: time.Now().Add(time.Minute)}
	}
	// Keep the fixture at the production bound before inserting another key.
	delete(c.repositoryLookups.cache, "gitlab:gitlab.com/a/b")
	result := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{RemoteURL: "https://gitlab.com/a/c"})
	if result.RepositoryID == "" || len(c.repositoryLookups.cache) != 128 {
		t.Fatalf("cache size %d: %+v", len(c.repositoryLookups.cache), result)
	}
}

func TestRepositoryLookupFailureCanRecover(t *testing.T) {
	t.Parallel()
	c, forge := newGitLabLookupCore(t)
	forge.answer.Store(forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{}`})
	input := RepoIdentity{RemoteURL: "https://gitlab.com/a/b"}
	if result := c.ResolveRepository(context.Background(), t.TempDir(), input); result.LookupError == "" {
		t.Fatal("missing error")
	}
	forge.answer.Store(forgeAPIAnswer{Body: `{"id":123}`})
	if result := c.ResolveRepository(context.Background(), t.TempDir(), input); result.LookupError == "" {
		t.Fatal("failure was not shared with the next reader")
	}
	// Expire the short failure cache without waiting on a wall-clock sleep.
	c.repositoryLookups.mu.Lock()
	c.repositoryLookups.cache["gitlab:gitlab.com/a/b"] = repositoryLookupResult{until: time.Now().Add(-time.Second)}
	c.repositoryLookups.mu.Unlock()
	if result := c.ResolveRepository(context.Background(), t.TempDir(), input); result.LookupError != "" || result.RepositoryID == "" {
		t.Fatalf("recovery: %+v", result)
	}
}

func TestRepositoryLookupSharesFailuresAcrossConcurrentReaders(t *testing.T) {
	t.Parallel()
	c, forge := newGitLabLookupCore(t)
	forge.answer.Store(forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{}`})
	forge.block.Store(true)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{RemoteURL: "https://gitlab.com/a/b"})
			if result.LookupError == "" || result.RepositoryID != "" {
				t.Errorf("failure lost: %+v", result)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	forge.unblock()
	wg.Wait()
	if n := forge.reads.Load(); n != 1 {
		t.Fatalf("failing lookup repeated: %d reads", n)
	}
}

func TestRepositoryLookupSharesItsOwnTimeout(t *testing.T) {
	t.Parallel()
	c, forge := newGitLabLookupCore(t)
	forge.block.Store(true)
	input := RepoIdentity{RemoteURL: "https://gitlab.com/a/b"}
	if result := c.ResolveRepository(context.Background(), t.TempDir(), input); result.LookupError == "" {
		t.Fatal("timed out lookup lost its diagnostic")
	}
	c.repositoryLookups.mu.Lock()
	cached := c.repositoryLookups.cache["gitlab:gitlab.com/a/b"]
	c.repositoryLookups.mu.Unlock()
	if cached.problem == "" || !time.Now().Before(cached.until) {
		t.Fatal("internal timeout was not cached for the next reader")
	}
	if result := c.ResolveRepository(context.Background(), t.TempDir(), input); result.LookupError != cached.problem {
		t.Fatalf("timeout not shared: %+v", result)
	}
	if n := forge.reads.Load(); n != 1 {
		t.Fatalf("timed out lookup repeated: %d reads", n)
	}
}

func TestRepositoryLookupRejectsNonRepositoryAPIPaths(t *testing.T) {
	t.Parallel()
	c, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer { return forgeUnexpected(t, call) })
	for _, remote := range []string{"https://github.com/../../user", "https://github.com/a/b/issues/1", "https://github.com/%2e%2e/user", "https://gitlab.com/group/../repo"} {
		result := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{RemoteURL: remote})
		if result.RepositoryID != "" || result.LookupError == "" {
			t.Errorf("non-repository URL accepted: %+v", result)
		}
	}
	if sent := calls.all(); len(sent) != 0 {
		t.Fatalf("invalid path reached the forge API: %+v", sent)
	}
}

func TestLocalRepositoryIdentityDoesNotWaitForForgeLookups(t *testing.T) {
	t.Parallel()
	c := NewCore(WithIsolatedForgeCLIs("", nil))
	for range cap(c.repositoryLookupSlots) {
		c.repositoryLookupSlots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// No network identity exists for these remotes, even while every lookup slot
	// is busy. They return an explicit unavailable identity without forge admission.
	for _, remote := range []string{"", "/srv/local", "https://unsupported.example/a/b"} {
		result := c.ResolveRepository(ctx, t.TempDir(), RepoIdentity{Repository: true, RemoteURL: remote})
		if result.LookupError == "" || result.RepositoryID != "" {
			t.Fatalf("local identity waited on forge: %+v", result)
		}
	}
}

func TestRepositoryLookupStampSurvivesAdmissionAndSSHFailures(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"https://github.com/a/b", "git@work-github:a/b.git"} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			fake := filepath.Join(dir, "tool")
			mockexec.Write(t, fake, "#!/bin/sh\nif [ \"$AO_FORGE_CLI\" = ssh ]; then printf 'hostname github.com\\n'; else exit 1; fi\n")
			api, _ := newForgeAPIService(t, func(forgeAPICall) forgeAPIAnswer { return forgeAPIAnswer{Body: `{"id":345}`} })
			c := NewCore(WithIsolatedForgeCLIs(fake, nil), WithForgeAPI(api))
			input := RepoIdentity{Repository: true, RemoteURL: raw}
			verified := c.ResolveRepository(context.Background(), dir, input)
			if verified.RepositoryID == "" || verified.IdentitySource == "" {
				t.Fatalf("initial verification: %+v", verified)
			}
			for range cap(c.repositoryLookupSlots) {
				c.repositoryLookupSlots <- struct{}{}
			}
			blocked := c.ResolveRepository(context.Background(), dir, input)
			if blocked.LookupError == "" || blocked.IdentitySource != verified.IdentitySource || blocked.RemoteURL != "" {
				t.Fatalf("admission failure lost stamp: %+v", blocked)
			}
			for range cap(c.repositoryLookupSlots) {
				<-c.repositoryLookupSlots
			}
			if isSSHRemote(raw) {
				mockexec.Write(t, fake, "#!/bin/sh\nexit 1\n")
				failed := c.ResolveRepository(context.Background(), dir, input)
				if failed.LookupError == "" || failed.IdentitySource != verified.IdentitySource || failed.RemoteURL != "" {
					t.Fatalf("SSH failure lost stamp: %+v", failed)
				}
			}
		})
	}
}

// Only the forge or its login being unavailable is retryable: a later
// lookup can clear it with no change to the checkout. The failure logs one
// redacted line with the transport's reason, never the response body.
func TestRepositoryLookupMarksForgeUnavailabilityRetryableAndLogsWhy(t *testing.T) {
	var logged strings.Builder
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	c, forge := newGitLabLookupCore(t)
	forge.answer.Store(forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"BODY https://user:SECRET@gitlab.com"}`})
	input := RepoIdentity{Repository: true, RemoteURL: "https://gitlab.com/a/b"}
	failed := c.ResolveRepository(context.Background(), t.TempDir(), input)
	if failed.LookupError == "" || !failed.LookupRetryable {
		t.Fatalf("forge failure: %+v, want a retryable error", failed)
	}
	if shared := c.ResolveRepository(context.Background(), t.TempDir(), input); !shared.LookupRetryable {
		t.Fatalf("cached failure lost retryability: %+v", shared)
	}
	line := logged.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "lookup with the glab login on gitlab.com failed: GET") || !strings.Contains(line, "HTTP 502") {
		t.Fatalf("logged %q, want one line with the request and its status", line)
	}
	for _, leaked := range []string{"SECRET", "BODY"} {
		if strings.Contains(line, leaked) {
			t.Fatalf("logged %q, leaked %q", line, leaked)
		}
	}

	logged.Reset()
	forge.answer.Store(forgeAPIAnswer{Body: "not json"})
	invalid := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{Repository: true, RemoteURL: "https://gitlab.com/a/c"})
	if invalid.LookupError == "" || invalid.LookupRetryable {
		t.Fatalf("invalid forge answer: %+v, want a non-retryable error", invalid)
	}
	unsupported := c.ResolveRepository(context.Background(), t.TempDir(), RepoIdentity{Repository: true, RemoteURL: "https://unsupported.example/a/b"})
	if unsupported.LookupError == "" || unsupported.LookupRetryable {
		t.Fatalf("unsupported forge: %+v, want a non-retryable error", unsupported)
	}
	if logged.Len() != 0 {
		t.Fatalf("a forge that answered logged %q", logged.String())
	}

	// A CLI this computer cannot run is not retried until someone installs it.
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", TokenSource: missingTokenSource{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	missing := NewCore(WithForgeAPI(svc)).ResolveRepository(context.Background(), t.TempDir(), input)
	if missing.LookupError == "" || missing.LookupRetryable {
		t.Fatalf("missing CLI: %+v, want a non-retryable error", missing)
	}
	if !strings.Contains(logged.String(), "lookup with the glab login on gitlab.com failed") {
		t.Fatalf("missing CLI logged %q, want its failure", logged.String())
	}
}

func TestLookupFailureDetailIsBounded(t *testing.T) {
	t.Parallel()
	detail := boundLookupDetail("lookup https://user:SECRET@gitlab.com/a/b " + strings.Repeat("é", 1000))
	if got := len([]rune(detail)); got != lookupFailureDetailRunes+3 || strings.Contains(detail, "SECRET") {
		t.Fatalf("detail %d runes: %q", got, detail)
	}
}
