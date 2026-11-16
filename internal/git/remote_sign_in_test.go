package git

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// The failures are the messages git 2.52's git-remote-http, OpenSSH 9.9 and
// Git Credential Manager (AuthenticationBase.ThrowIfUserInteractionDisabled)
// print.
func TestRemoteSignInErrorNamesOnlyAMissingLogin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		failure string
		want    string
	}{
		{"https without a login", "git fetch --quiet origin exited with code 128: fatal: could not read Username for 'https://token:hunter2@gitlab.example.com/group/repo.git': terminal prompts disabled",
			"git fetch --quiet origin exited with code 128: fatal: could not read Username for '[remote]': terminal prompts disabled"},
		{"https with a rejected login", "git fetch --quiet origin exited with code 128: fatal: Authentication failed for 'https://gitlab.example.com/group/repo.git/'",
			"git fetch --quiet origin exited with code 128: fatal: Authentication failed for '[remote]'"},
		{"ssh key refused", "git fetch --quiet origin exited with code 128: git@gitlab.example.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", "x"},
		{"ssh unknown host", "git fetch --quiet origin exited with code 128: Host key verification failed.\nfatal: Could not read from remote repository.", "x"},
		{"credential manager told not to ask", "git fetch --quiet origin exited with code 128: fatal: Cannot prompt because user interactivity has been disabled.", "x"},
		{"https forbidden", "git fetch --quiet origin exited with code 128: fatal: unable to access 'https://gitlab.example.com/group/repo.git/': The requested URL returned error: 403", "x"},
		{"network down", "git fetch --quiet origin exited with code 128: fatal: unable to access 'https://gitlab.example.com/group/repo.git/': Could not resolve host: gitlab.example.com", ""},
		{"timed out", "git fetch --quiet origin timed out after 45s", ""},
		{"repository gone", "git fetch --quiet origin exited with code 128: fatal: repository 'https://gitlab.example.com/group/repo.git/' not found", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, _ := repoWithOrigin(t)
			core := NewCore()
			now := time.Unix(10_000, 0)
			failure := errors.New(tc.failure)
			failingFetch(core, &now, &failure)
			if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
				t.Fatal("expected the fetch to fail")
			}
			got := core.RemoteSignInError(repo)
			switch {
			case tc.want == "x":
				if got == "" {
					t.Fatalf("RemoteSignInError is empty for %q", tc.failure)
				}
			case got != tc.want:
				t.Fatalf("RemoteSignInError = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "hunter2") || strings.Contains(got, "gitlab.example.com/group") {
				t.Fatalf("RemoteSignInError carries the remote URL: %q", got)
			}
			status, err := core.StatusFast(repo)
			if err != nil {
				t.Fatal(err)
			}
			if status.RemoteSignInError != got {
				t.Fatalf("status.RemoteSignInError = %q, want %q", status.RemoteSignInError, got)
			}
		})
	}
}

// The observer hears each change of the answer once: when a login starts
// to be missing and when a fetch succeeds again, not on every failed retry.
func TestRemoteSignInObserverHearsChangesOnly(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	signIn := errors.New("fatal: could not read Username for 'https://example.com': terminal prompts disabled")
	failure := signIn
	failingFetch(core, &now, &failure)
	commonDir := mustCommonDir(t, core, repo)
	var mu sync.Mutex
	var heard []string
	core.SetRemoteSignInObserver(func(dir string) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, dir+"|"+core.RemoteSignInError(repo))
	})
	step := func(next error, wantHeard int) {
		t.Helper()
		failure = next
		_, _ = core.FetchRemotesBackground(t.Context(), repo)
		now = now.Add(fetchRetryMax)
		mu.Lock()
		defer mu.Unlock()
		if len(heard) != wantHeard {
			t.Fatalf("observer heard %d changes, want %d: %q", len(heard), wantHeard, heard)
		}
	}
	step(signIn, 1)
	step(signIn, 1)
	step(errors.New("fatal: Could not resolve host: example.com"), 2)
	step(signIn, 3)
	step(nil, 4)
	step(nil, 4)
	if want := commonDir + "|"; heard[3] != want || heard[1] != want {
		t.Fatalf("clears heard as %q and %q, want %q", heard[1], heard[3], want)
	}
	if !strings.HasPrefix(heard[0], commonDir+"|fatal: could not read Username") {
		t.Fatalf("first change heard as %q", heard[0])
	}

	core.SetRemoteSignInObserver(nil)
	step(signIn, 4)
}

// Turning the cadence off ends every record: the marks go, each observer
// hears its own, and a repository that failed for another reason says nothing.
func TestForgetFetchFailuresClearsEveryMark(t *testing.T) {
	t.Parallel()
	signedOut, _ := repoWithOrigin(t)
	offline, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
	calls := failingFetch(core, &now, &failure)
	_, _ = core.FetchRemotesBackground(t.Context(), signedOut)
	failure = errors.New("fatal: Could not resolve host: example.com")
	_, _ = core.FetchRemotesBackground(t.Context(), offline)

	var mu sync.Mutex
	var heard []string
	core.SetRemoteSignInObserver(func(dir string) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, dir)
	})
	core.ForgetFetchFailures()
	core.ForgetFetchFailures()
	if got := core.RemoteSignInError(signedOut); got != "" {
		t.Fatalf("RemoteSignInError = %q after ForgetFetchFailures, want none", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{mustCommonDir(t, core, signedOut)}; !slices.Equal(heard, want) {
		t.Fatalf("observer heard %q, want %q", heard, want)
	}
	before := calls.Load()
	_, _ = core.FetchRemotesBackground(t.Context(), offline)
	if calls.Load() != before+1 {
		t.Fatal("a forgotten failure still held the backoff")
	}
}

// Real git against a remote that demands a login: the unattended fetch
// marks the repository without asking anyone, the mark is the same from
// every worktree, and the fetch a user asks for is the one that asks.
func TestUnattendedFetchMarksARemoteThatWantsALogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script askpass helper is unix-only")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	// A translated git must still be recognized.
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")

	repo, bare := repoWithOrigin(t)
	testutil.RunGit(t, repo, "branch", "feature/linked")
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, repo, "worktree", "add", "-q", linked, "feature/linked")
	testutil.RunGit(t, repo, "remote", "set-url", "origin", server.URL+"/secret-group/repo.git")

	askpassLog := filepath.Join(t.TempDir(), "askpass.log")
	askpass := filepath.Join(t.TempDir(), "askpass.sh")
	mockexec.Write(t, askpass, "#!/bin/sh\necho \"asked $*\" >> "+askpassLog+"\necho someone\n")
	t.Setenv("GIT_ASKPASS", askpass)

	core := NewCore()
	if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
		t.Fatal("the unattended fetch of a remote that wants a login should fail")
	}
	if _, err := os.Stat(askpassLog); !os.IsNotExist(err) {
		t.Fatalf("the unattended fetch asked for credentials (stat: %v)", err)
	}
	mark := core.RemoteSignInError(linked)
	if !strings.Contains(mark, "terminal prompts disabled") {
		t.Fatalf("RemoteSignInError from the linked worktree = %q, want git's prompt diagnosis", mark)
	}
	if strings.Contains(mark, "secret-group") {
		t.Fatalf("RemoteSignInError carries the remote URL: %q", mark)
	}
	for _, cwd := range []string{repo, linked} {
		status, err := core.StatusFast(cwd)
		if err != nil {
			t.Fatal(err)
		}
		if status.RemoteSignInError != mark {
			t.Fatalf("status of %s carries RemoteSignInError %q, want %q", cwd, status.RemoteSignInError, mark)
		}
	}

	// The fetch a user asks for asks, and its failure leaves the mark.
	if err := core.FetchOrigin(t.Context(), repo); err == nil {
		t.Fatal("FetchOrigin against a remote that rejects every login should fail")
	}
	if _, err := os.Stat(askpassLog); err != nil {
		t.Fatalf("FetchOrigin never asked for credentials: %v", err)
	}
	if got := core.RemoteSignInError(repo); got != mark {
		t.Fatalf("RemoteSignInError = %q after a failed FetchOrigin, want it kept", got)
	}

	// Signed in (here: a remote that needs no login): the asked-for fetch
	// clears the mark for every worktree at once.
	testutil.RunGit(t, repo, "remote", "set-url", "origin", bare)
	if err := core.FetchOrigin(t.Context(), linked); err != nil {
		t.Fatalf("FetchOrigin: %v", err)
	}
	if got := core.RemoteSignInError(repo); got != "" {
		t.Fatalf("RemoteSignInError = %q after a successful FetchOrigin, want none", got)
	}
	if fetched, err := core.FetchRemotesBackground(t.Context(), repo); err != nil || fetched {
		t.Fatalf("FetchRemotesBackground right after FetchOrigin = %v, %v; want the fresh window to skip it", fetched, err)
	}
}

// A repository with no origin has nothing to sign in to, whatever an
// earlier remote left behind.
func TestStatusWithoutOriginCarriesNoSignInError(t *testing.T) {
	t.Parallel()
	repo, _ := repoWithOrigin(t)
	core := NewCore()
	now := time.Unix(10_000, 0)
	failure := errors.New("fatal: Authentication failed for 'https://example.com/'")
	failingFetch(core, &now, &failure)
	if _, err := core.FetchRemotesBackground(t.Context(), repo); err == nil {
		t.Fatal("expected the fetch to fail")
	}
	testutil.RunGit(t, repo, "remote", "remove", "origin")
	now = now.Add(time.Hour)
	status, err := core.StatusFast(repo)
	if err != nil {
		t.Fatal(err)
	}
	if status.HasOriginRemote || status.RemoteSignInError != "" {
		t.Fatalf("status = origin %v, RemoteSignInError %q; want neither", status.HasOriginRemote, status.RemoteSignInError)
	}
}
