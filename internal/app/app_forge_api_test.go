package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The git core an isolated App builds carries a forge API transport
// pointed at the fake forge listener SetForgeAPI installed, with its fixed
// token; without one the Core is a construction error, not a Core whose
// transport could reach a real forge.
func TestIsolatedAppPointsTheForgeAPIAtTheFake(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []*http.Request
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(r.Context()))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"ao-viewer"}`))
	}))
	defer fake.Close()

	app := &App{version: "1.2.3"}
	ConfigureIsolation(app, IsolationConfig{})
	SetForgeAPI(app, ForgeAPI{BaseURL: fake.URL, Token: "fake-token-1"})
	core, err := app.newGitCore()
	if err != nil || core.ForgeAPI() == nil {
		t.Fatalf("an isolated git core = %v, %v", core, err)
	}
	svc := core.ForgeAPI()
	defer svc.Close()
	if !svc.Isolated() {
		t.Fatal("an isolated App built a transport that is not isolated")
	}
	if _, err := svc.GitHub("github.com").Do(t.Context(), forgeapi.Request{Path: "user"}); err != nil {
		t.Fatalf("request to the fake: %v", err)
	}
	mu.Lock()
	if len(seen) != 1 || seen[0].URL.Path != "/github/rest/user" || seen[0].Host != "github.com" ||
		seen[0].Header.Get("Authorization") != "Bearer fake-token-1" || seen[0].Header.Get("User-Agent") != "agent-overflow/1.2.3" {
		t.Fatalf("the fake saw %+v", seen)
	}
	mu.Unlock()

	unconfigured := &App{version: "1.2.3"}
	ConfigureIsolation(unconfigured, IsolationConfig{})
	if core, err := unconfigured.newGitCore(); err == nil || core != nil {
		t.Fatalf("an isolated App without a fake forge built a git core: %v, %v", core, err)
	}
}

// A desktop App's git core carries the production transport: CLI-read
// tokens, never the isolated one. Nothing here sends a request, so no
// real gh or glab runs.
func TestDesktopAppBuildsAForgeAPITransport(t *testing.T) {
	t.Parallel()
	core, err := (&App{version: "1.2.3"}).newGitCore()
	if err != nil || core.ForgeAPI() == nil {
		t.Fatalf("a desktop git core = %v, %v", core, err)
	}
	svc := core.ForgeAPI()
	defer svc.Close()
	if svc.Isolated() {
		t.Fatal("a desktop App built an isolated forge API transport")
	}
	if core, err := (&App{}).newGitCore(); err == nil || core != nil {
		t.Fatalf("an App without a version built a git core: %v, %v", core, err)
	}
}

// Shutdown closes the git core's forge API transport: a request after it
// fails without reaching the forge.
func TestShutdownClosesTheForgeAPITransport(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fake.Close()

	app, rec := newFullyWiredTestApp(t)
	app.version = "1.2.3"
	ConfigureIsolation(app, IsolationConfig{})
	SetForgeAPI(app, ForgeAPI{BaseURL: fake.URL, Token: "fake-token-1"})
	core, err := app.newGitCore()
	if err != nil {
		t.Fatal(err)
	}
	app.git = core
	svc := core.ForgeAPI()
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !slices.Contains(rec.snapshot(), "close forge API transport") {
		t.Fatalf("shutdown steps %v lack the forge API transport", rec.snapshot())
	}
	if _, err := svc.GitHub("github.com").Do(t.Context(), forgeapi.Request{Path: "user"}); err == nil {
		t.Fatal("the forge API transport answered after Shutdown")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("the fake forge saw %d requests after Shutdown", requests)
	}
}

// A git core the boot cannot build fails the boot's store phase, with the
// store it opened closed, instead of starting with no forge transport.
func TestInitStoresFailsWhenTheGitCoreCannotBeBuilt(t *testing.T) {
	app := NewApp()
	ConfigureIsolation(app, IsolationConfig{})
	app.dataDirOverride = t.TempDir()
	migratedDatabaseAt(t, filepath.Join(app.dataDirOverride, "agent-overflow", databaseFileName))
	_, st, err := app.initStores(context.Background())
	if err == nil {
		_ = st.Close()
		t.Fatal("initStores succeeded with an isolated boot that has no fake forge")
	}
	if !strings.Contains(err.Error(), "failed to build the git core") || app.git != nil {
		t.Fatalf("initStores = %v (git core %v), want the git core failure", err, app.git)
	}
	if _, readErr := app.store.Identity(); readErr == nil {
		t.Fatal("the store initStores opened is still open after the git core failure")
	}
}

// The Core gitCore builds while a.git is unset carries no forge API
// transport, so a forge read through it fails with ErrNoForgeAPI instead
// of building a transport nothing owns.
func TestGitCoreBeforeStartHasNoForgeAPI(t *testing.T) {
	t.Parallel()
	core := (&App{}).gitCore()
	if core.ForgeAPI() != nil {
		t.Fatalf("gitCore before Start has transport %v; want none", core.ForgeAPI())
	}
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "o", Repo: "r", Number: 1}
	if _, err := core.GetPRDetail(t.Context(), ref); !errors.Is(err, gitops.ErrNoForgeAPI) {
		t.Fatalf("GetPRDetail through the pre-Start Core = %v, want ErrNoForgeAPI", err)
	}
}

// githubAPITestCore is a git core whose forge API transport is pointed, as
// an isolated boot's is, at an httptest GitHub that handler serves.
func githubAPITestCore(t *testing.T, handler http.HandlerFunc) *gitops.Core {
	t.Helper()
	return gitops.NewCore(gitops.WithForgeAPI(githubAPITestService(t, handler)))
}

// githubAPITestService is githubAPITestCore's transport, for a core built
// with the App's own options.
func githubAPITestService(t *testing.T, handler http.HandlerFunc) *forgeapi.Service {
	t.Helper()
	fake := httptest.NewServer(handler)
	t.Cleanup(fake.Close)
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: fake.URL, Token: "test-token"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// githubGraphQLRequest is one GraphQL request a githubAPITestCore served.
type githubGraphQLRequest struct {
	Host          string
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

// readGitHubGraphQL decodes a GraphQL request, or reports nil for any
// other request.
func readGitHubGraphQL(t *testing.T, r *http.Request) *githubGraphQLRequest {
	t.Helper()
	if r.URL.Path != "/github/graphql" {
		return nil
	}
	var req githubGraphQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode GraphQL request: %v", err)
		return nil
	}
	req.Host = r.Host
	return &req
}
