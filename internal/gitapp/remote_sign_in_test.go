package gitapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// A background fetch that fails for want of a login writes nothing a
// watcher sees, so the service refreshes the status stream of every
// subscribed workspace of that repository: the root and a linked worktree
// both carry RemoteSignInError, the cadence logs it once, and the fetch a
// user asks for clears it on both streams.
func TestBackgroundFetchSignInFailureReachesEveryWorkspaceStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	project, bare := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, project, "branch", "feature/linked")
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, project, "worktree", "add", "-q", linked, "feature/linked")
	testutil.RunGit(t, project, "remote", "set-url", "origin", server.URL+"/repo.git")

	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Now().UnixMilli()
	if _, err := database.CreateProject(store.Project{ID: "project", Path: project, Name: "project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	core := gitops.NewCore()
	watch := gitwatch.NewManager(gitwatch.ManagerConfig{
		StatusFn:     core.Status,
		FastStatusFn: func(_ context.Context, cwd string) (gitops.GitStatus, error) { return core.StatusFast(cwd) },
	})
	var enabled atomic.Bool
	enabled.Store(true)
	var mu sync.Mutex
	var logs []string
	events := make(chan StatusEvent, 64)
	service := New(Deps{
		Store: database, Core: core, Watch: watch,
		BackgroundFetchEnabled: enabled.Load,
		EmitStatus:             func(event StatusEvent) { events <- event },
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	t.Cleanup(service.CloseStatus)

	root, err := service.Subscribe(t.Context(), WorkspaceRef{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Subscribe root: %v", err)
	}
	defer service.Unsubscribe(root.ID)
	worktree, err := service.Subscribe(t.Context(), WorkspaceRef{ProjectID: "project", WorkspacePath: linked})
	if err != nil {
		t.Fatalf("Subscribe worktree: %v", err)
	}
	defer service.Unsubscribe(worktree.ID)
	if root.Status.RemoteSignInError != "" || worktree.Status.RemoteSignInError != "" {
		t.Fatalf("a sign-in error before any fetch: %q, %q", root.Status.RemoteSignInError, worktree.Status.RemoteSignInError)
	}

	// awaitBoth waits until both streams' latest status satisfies want.
	awaitBoth := func(what string, want func(gitops.GitStatus) bool) {
		t.Helper()
		seen := map[string]bool{}
		deadline := time.After(15 * time.Second)
		for !seen[root.Cwd] || !seen[worktree.Cwd] {
			select {
			case event := <-events:
				seen[event.Cwd] = want(event.Status)
			case <-deadline:
				t.Fatalf("%s: streams that got there: %v", what, seen)
			}
		}
	}

	service.RunBackgroundFetchPass(t.Context())
	awaitBoth("no stream reported the missing login", func(status gitops.GitStatus) bool {
		return strings.Contains(status.RemoteSignInError, "terminal prompts disabled")
	})

	// Later ticks inside the backoff run no git and log nothing new.
	service.RunBackgroundFetchPass(t.Context())
	service.RunBackgroundFetchPass(t.Context())
	mu.Lock()
	fetchLogs := 0
	for _, line := range logs {
		if strings.HasPrefix(line, "git background fetch:") {
			fetchLogs++
		}
	}
	mu.Unlock()
	if fetchLogs != 1 {
		t.Fatalf("the failing repository was logged %d times across three passes, want 1: %q", fetchLogs, logs)
	}

	// Turned off, nothing would retry the fetch: the next tick ends the
	// mark. Turned on again, the first tick fetches and marks again.
	enabled.Store(false)
	service.RunBackgroundFetchPass(t.Context())
	awaitBoth("a stream kept the sign-in error with background fetch turned off", func(status gitops.GitStatus) bool {
		return status.RemoteSignInError == ""
	})
	enabled.Store(true)
	service.RunBackgroundFetchPass(t.Context())
	awaitBoth("no stream reported the missing login after background fetch was turned on again", func(status gitops.GitStatus) bool {
		return strings.Contains(status.RemoteSignInError, "terminal prompts disabled")
	})

	testutil.RunGit(t, project, "remote", "set-url", "origin", bare)
	result, err := service.FetchOrigin(t.Context(), WorkspaceRef{ProjectID: "project", WorkspacePath: linked})
	if err != nil {
		t.Fatalf("FetchOrigin: %v", err)
	}
	if result.Action != "fetch" || result.Branch != "feature/linked" {
		t.Fatalf("FetchOrigin answered %+v", result)
	}
	awaitBoth("a stream kept the sign-in error after a successful fetch", func(status gitops.GitStatus) bool {
		return status.RemoteSignInError == ""
	})
}
