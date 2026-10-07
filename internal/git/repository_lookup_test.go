package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/testutil/mockexec"
)

func TestRepositoryLookupUsesForgeIdentityAndCaches(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"github.com", "gitlab.com"} {
		t.Run(host, func(t *testing.T) {
			dir := t.TempDir()
			fake := filepath.Join(dir, "forge")
			mockexec.Write(t, fake, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AO_LOOKUP_LOG\"\nprintf '{\"id\":123}'\n")
			log := filepath.Join(dir, "log")
			c := NewCore(WithIsolatedForgeCLIs(fake, []string{"AO_LOOKUP_LOG=" + log}))
			input := RepoIdentity{Repository: true, RemoteURL: "https://user:SECRET@" + host + "/owner/repo?token=SECRET"}
			first := c.ResolveRepository(context.Background(), dir, input)
			second := c.ResolveRepository(context.Background(), dir, input)
			if first.RepositoryID == "" || second.RepositoryID != first.RepositoryID || first.LookupError != "" || first.RemoteURL != "" || first.IdentitySource == "" {
				t.Fatalf("identity %+v %+v", first, second)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "SECRET") || strings.Count(string(calls), "\n") != 1 || !strings.Contains(string(calls), "--hostname "+host) {
				t.Fatalf("calls %s", calls)
			}
		})
	}
}

func TestRepositoryLookupFailureAndCancellation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "forge")
	mockexec.Write(t, fake, "#!/bin/sh\necho 'https://user:SECRET@github.com/a/b' >&2\nexit 1\n")
	c := NewCore(WithIsolatedForgeCLIs(fake, nil))
	identity := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: "https://github.com/a/b"})
	if identity.LookupError == "" || strings.Contains(identity.LookupError, "SECRET") || identity.RepositoryID != "" {
		t.Fatalf("failure %+v", identity)
	}
	mockexec.Write(t, fake, "#!/bin/sh\nexec sleep 30\n")
	delete(c.repositoryLookups.cache, "github:github.com/a/b")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	identity = c.ResolveRepository(ctx, dir, RepoIdentity{RemoteURL: "https://github.com/a/b"})
	if time.Since(start) > 3*time.Second || ctx.Err() == nil || identity.RepositoryID != "" {
		t.Fatal("lookup did not cancel")
	}
	mockexec.Write(t, fake, "#!/bin/sh\nprintf '{\"id\":123}'\n")
	if result := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: "https://github.com/a/b"}); result.RepositoryID == "" {
		t.Fatalf("caller cancellation poisoned the cache: %+v", result)
	}
}

func TestRepositoryLookupResolvesSSHHostOnOwningComputer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "tool")
	mockexec.Write(t, fake, "#!/bin/sh\nif [ \"$AO_FORGE_CLI\" = ssh ]; then printf 'hostname github.com\\n'; else printf '{\"id\":345}'; fi\n")
	c := NewCore(WithIsolatedForgeCLIs(fake, nil))
	result := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: "git@github-work:Owner/Repo.git"})
	if result.RepositoryID != "github:github.com:345" || result.RemoteURL != "" || result.LookupError != "" {
		t.Fatalf("alias: %+v", result)
	}
}

func TestRepositoryLookupConcurrentCallsAndBoundedCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake, log := filepath.Join(dir, "forge"), filepath.Join(dir, "calls")
	mockexec.Write(t, fake, "#!/bin/sh\nprintf 'call\\n' >> \"$AO_LOOKUP_LOG\"\nsleep 0.1\nprintf '{\"id\":789}'\n")
	c := NewCore(WithIsolatedForgeCLIs(fake, []string{"AO_LOOKUP_LOG=" + log}))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: "https://github.com/a/b"})
			if result.RepositoryID != "github:github.com:789" {
				t.Errorf("lookup: %+v", result)
			}
		}()
	}
	wg.Wait()
	calls, err := os.ReadFile(log)
	if err != nil || strings.Count(string(calls), "\n") != 1 {
		t.Fatalf("calls %q: %v", calls, err)
	}
	for i := range 128 {
		c.repositoryLookups.cache[fmt.Sprint(i)] = repositoryLookupResult{id: "old", until: time.Now().Add(time.Minute)}
	}
	// Keep the fixture at the production bound before inserting another key.
	delete(c.repositoryLookups.cache, "github:github.com/a/b")
	result := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: "https://github.com/a/c"})
	if result.RepositoryID == "" || len(c.repositoryLookups.cache) != 128 {
		t.Fatalf("cache size %d: %+v", len(c.repositoryLookups.cache), result)
	}
}

func TestRepositoryLookupFailureCanRecover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "forge")
	mockexec.Write(t, fake, "#!/bin/sh\nexit 1\n")
	c := NewCore(WithIsolatedForgeCLIs(fake, nil))
	input := RepoIdentity{RemoteURL: "https://github.com/a/b"}
	if result := c.ResolveRepository(context.Background(), dir, input); result.LookupError == "" {
		t.Fatal("missing error")
	}
	mockexec.Write(t, fake, "#!/bin/sh\nprintf '{\"id\":123}'\n")
	if result := c.ResolveRepository(context.Background(), dir, input); result.LookupError == "" {
		t.Fatal("failure was not shared with the next reader")
	}
	// Expire the short failure cache without waiting on a wall-clock sleep.
	c.repositoryLookups.mu.Lock()
	c.repositoryLookups.cache["github:github.com/a/b"] = repositoryLookupResult{until: time.Now().Add(-time.Second)}
	c.repositoryLookups.mu.Unlock()
	if result := c.ResolveRepository(context.Background(), dir, input); result.LookupError != "" || result.RepositoryID == "" {
		t.Fatalf("recovery: %+v", result)
	}
}

func TestRepositoryLookupSharesFailuresAcrossConcurrentReaders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake, log := filepath.Join(dir, "forge"), filepath.Join(dir, "calls")
	mockexec.Write(t, fake, "#!/bin/sh\nprintf 'call\\n' >> \"$AO_LOOKUP_LOG\"\nsleep 0.1\nexit 1\n")
	c := NewCore(WithIsolatedForgeCLIs(fake, []string{"AO_LOOKUP_LOG=" + log}))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: "https://github.com/a/b"})
			if result.LookupError == "" || result.RepositoryID != "" {
				t.Errorf("failure lost: %+v", result)
			}
		}()
	}
	wg.Wait()
	calls, err := os.ReadFile(log)
	if err != nil || strings.Count(string(calls), "\n") != 1 {
		t.Fatalf("failing lookup repeated: %q, %v", calls, err)
	}
}

func TestRepositoryLookupSharesItsOwnTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake, log := filepath.Join(dir, "forge"), filepath.Join(dir, "calls")
	mockexec.Write(t, fake, "#!/bin/sh\nprintf 'call\\n' >> \"$AO_LOOKUP_LOG\"\nexec sleep 30\n")
	c := NewCore(WithIsolatedForgeCLIs(fake, []string{"AO_LOOKUP_LOG=" + log}))
	input := RepoIdentity{RemoteURL: "https://github.com/a/b"}
	if result := c.ResolveRepository(context.Background(), dir, input); result.LookupError == "" {
		t.Fatal("timed out lookup lost its diagnostic")
	}
	c.repositoryLookups.mu.Lock()
	cached := c.repositoryLookups.cache["github:github.com/a/b"]
	c.repositoryLookups.mu.Unlock()
	if cached.problem == "" || !time.Now().Before(cached.until) {
		t.Fatal("internal timeout was not cached for the next reader")
	}
	if result := c.ResolveRepository(context.Background(), dir, input); result.LookupError != cached.problem {
		t.Fatalf("timeout not shared: %+v", result)
	}
	calls, err := os.ReadFile(log)
	if err != nil || strings.Count(string(calls), "\n") != 1 {
		t.Fatalf("timed out lookup repeated: %q, %v", calls, err)
	}
}

func TestRepositoryLookupRejectsNonRepositoryAPIPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake, log := filepath.Join(dir, "forge"), filepath.Join(dir, "calls")
	mockexec.Write(t, fake, "#!/bin/sh\nprintf call >> \"$AO_LOOKUP_LOG\"\nprintf '{\"id\":123}'\n")
	c := NewCore(WithIsolatedForgeCLIs(fake, []string{"AO_LOOKUP_LOG=" + log}))
	for _, remote := range []string{"https://github.com/../../user", "https://github.com/a/b/issues/1", "https://github.com/%2e%2e/user", "https://gitlab.com/group/../repo"} {
		result := c.ResolveRepository(context.Background(), dir, RepoIdentity{RemoteURL: remote})
		if result.RepositoryID != "" || result.LookupError == "" {
			t.Errorf("non-repository URL accepted: %+v", result)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("invalid path reached forge CLI: %v", err)
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
			mockexec.Write(t, fake, "#!/bin/sh\nif [ \"$AO_FORGE_CLI\" = ssh ]; then printf 'hostname github.com\\n'; else printf '{\"id\":345}'; fi\n")
			c := NewCore(WithIsolatedForgeCLIs(fake, nil))
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
