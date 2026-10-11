package harnessrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/forgefake"
)

func TestHarnessForgeSeedIsStrictAndPositioned(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	_, err := h.HarnessForgeSeed(json.RawMessage("{\n  \"repos\": [{\"forge\": \"github\", \"project\": \"a/b\", \"pullz\": []}]\n}"))
	if err == nil || !strings.Contains(err.Error(), "pullz") || !strings.Contains(err.Error(), "at line ") {
		t.Fatalf("unknown field error = %v, want the field and its position", err)
	}
	if _, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"github","project":"a/b"}]} {}`)); err == nil {
		t.Fatal("a trailing document was accepted")
	}
	if _, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"svn","project":"a/b"}]}`)); err == nil {
		t.Fatal("an invalid fixture was accepted")
	}
}

// The whole harness-side path: a forwarded call reaches the engine
// through the control server StartControl builds, is recorded, fans out
// as harness:forge, and HarnessReset clears both the state and the log.
func TestForgeCallsReachTheSeededFixtureThroughControl(t *testing.T) {
	h, host := newHarnessTestHost(t)
	var mu sync.Mutex
	var events []forgefake.Invocation
	host.emit = func(channel eventchan.Channel, data any) {
		if channel == eventchan.HarnessForge {
			mu.Lock()
			events = append(events, data.(forgefake.Invocation))
			mu.Unlock()
		}
	}
	controlServer, env, err := StartControl(h)
	if err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	t.Cleanup(controlServer.Shutdown)
	t.Setenv(control.EnvAddr, env[control.EnvAddr])
	t.Setenv(control.EnvToken, env[control.EnvToken])
	client, ok := control.FromEnv()
	if !ok {
		t.Fatal("no control env")
	}

	seeded, err := h.HarnessForgeSeed(json.RawMessage(`{"sshHosts":{"work":"gitlab.com"},"repos":[{"forge":"gitlab","project":"acme/forge-rpc",
		"pulls":[{"number":4,"title":"Seeded","comments":[{"body":"hi"}]}]}]}`))
	if err != nil {
		t.Fatalf("HarnessForgeSeed: %v", err)
	}
	if seeded.Repos[0].Pulls[0].Comments[0].ID == 0 {
		t.Fatal("seed did not return the generated comment id")
	}

	resolve := []string{"-G", "work"}
	result, err := client.Forge(control.ForgeCall{CLI: "ssh", Args: resolve, Cwd: "/ws"})
	if err != nil || result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "hostname gitlab.com\n") {
		t.Fatalf("forge call = %+v, %v", result, err)
	}
	log := h.HarnessForgeInvocations(0)
	if len(log.Invocations) != 1 || log.Invocations[0].Route != "ssh" || log.Invocations[0].Cwd != "/ws" {
		t.Fatalf("invocations = %+v", log)
	}
	mu.Lock()
	if len(events) != 1 || events[0].Seq != log.Invocations[0].Seq {
		t.Fatalf("harness:forge events = %+v", events)
	}
	mu.Unlock()

	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	if log := h.HarnessForgeInvocations(0); len(log.Invocations) != 0 {
		t.Fatalf("reset kept the invocation log: %+v", log)
	}
	result, err = client.Forge(control.ForgeCall{CLI: "ssh", Args: resolve})
	if err != nil || result.ExitCode != 0 || !strings.Contains(string(result.Stdout), "hostname work\n") {
		t.Fatalf("the seeded ssh host survived reset: %+v, %v", result, err)
	}
}

// The forge API path end to end on the harness side: the app's isolated
// transport (forgeapi.Options.Isolated, as newGitCore builds it) reaches
// the seeded fixture through the listener StartForgeAPI owns, and the
// request is recorded and emitted as an http invocation.
func TestForgeAPIRequestsReachTheSeededFixture(t *testing.T) {
	h, host := newHarnessTestHost(t)
	var mu sync.Mutex
	var events []forgefake.Invocation
	host.emit = func(channel eventchan.Channel, data any) {
		if channel == eventchan.HarnessForge {
			mu.Lock()
			events = append(events, data.(forgefake.Invocation))
			mu.Unlock()
		}
	}
	server, endpoint, err := StartForgeAPI(h)
	if err != nil {
		t.Fatalf("StartForgeAPI: %v", err)
	}
	t.Cleanup(server.Shutdown)
	if endpoint.Token == "" || !strings.HasPrefix(endpoint.BaseURL, "http://[::1]:") {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if _, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"gitlab","project":"grp/forge-api","pulls":[{"number":5,"title":"Over HTTP"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: endpoint.BaseURL, Token: endpoint.Token}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	var mr struct {
		IID   int    `json:"iid"`
		Title string `json:"title"`
	}
	if _, err := svc.GitLab("gitlab.com").JSON(t.Context(), forgeapi.Request{Path: "projects/grp%2Fforge-api/merge_requests/5"}, &mr); err != nil || mr.IID != 5 || mr.Title != "Over HTTP" {
		t.Fatalf("merge request = %+v, %v", mr, err)
	}
	log := h.HarnessForgeInvocations(0).Invocations
	if len(log) != 1 || log[0].Via != forgefake.ViaHTTP || log[0].Route != "glab api merge request" || log[0].Status != 200 ||
		log[0].Path != "projects/grp%2Fforge-api/merge_requests/5" || log[0].Host != "gitlab.com" {
		t.Fatalf("invocations = %+v", log)
	}
	mu.Lock()
	if len(events) != 1 || events[0].Seq != log[0].Seq || events[0].Via != forgefake.ViaHTTP {
		t.Fatalf("harness:forge events = %+v", events)
	}
	mu.Unlock()

	wrong, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: endpoint.BaseURL, Token: "fake-token-2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	_, err = wrong.GitLab("gitlab.com").Do(t.Context(), forgeapi.Request{Path: "projects/grp%2Fforge-api/merge_requests/5"})
	if _, setup := errors.AsType[*forgeapi.SetupError](err); !setup {
		t.Fatalf("a wrong token = %v, want the fake's 401 as a setup error", err)
	}
}

// HarnessForgeRateLimit takes the wire's {forge, pool, remaining, reset}
// and the app's transport reads the limited pool as rate limited.
func TestHarnessForgeRateLimitReachesTheTransport(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	server, endpoint, err := StartForgeAPI(h)
	if err != nil {
		t.Fatalf("StartForgeAPI: %v", err)
	}
	t.Cleanup(server.Shutdown)
	if _, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"gitlab","project":"grp/limited","pulls":[{"number":5,"title":"Limited"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).Unix()
	var limit forgefake.RateLimit
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"forge":"gitlab","pool":"throttle_authenticated_api","remaining":0,"reset":%d}`, reset)), &limit); err != nil {
		t.Fatal(err)
	}
	if err := h.HarnessForgeRateLimit(limit); err != nil {
		t.Fatalf("HarnessForgeRateLimit: %v", err)
	}
	if err := h.HarnessForgeRateLimit(forgefake.RateLimit{Forge: "gitlab", Pool: "core", Reset: reset}); err == nil {
		t.Fatal("a pool GitLab does not have was accepted")
	}
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: endpoint.BaseURL, Token: endpoint.Token}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	_, err = svc.GitLab("gitlab.com").Do(t.Context(), forgeapi.Request{Path: "projects/grp%2Flimited/merge_requests/5"})
	limited, ok := errors.AsType[*forgeapi.RateLimitedError](err)
	if !ok || limited.Reserve || limited.Until.Unix() < reset-1 || limited.Until.Unix() > reset+1 {
		t.Fatalf("a request to the limited pool = %v", err)
	}
	if log := h.HarnessForgeInvocations(0).Invocations; len(log) != 1 || log[0].Route != "rate limited" || log[0].Status != 429 || log[0].Unhandled {
		t.Fatalf("invocations = %+v", log)
	}
}

// Offline drops every pooled connection and records each request as
// route "offline" without a reply: the next request on a client holding a
// keep-alive connection dials afresh and fails instead of getting a reply,
// so every retry the app makes is observable. Coming back, and
// HarnessReset, answer on the same port again.
func TestForgeAPIOfflineDropsKeepAliveConnections(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	server, endpoint, err := StartForgeAPI(h)
	if err != nil {
		t.Fatalf("StartForgeAPI: %v", err)
	}
	t.Cleanup(server.Shutdown)
	if _, err := h.HarnessForgeSeed(json.RawMessage(`{"repos":[{"forge":"github","project":"a/b"}]}`)); err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	var dials atomic.Int32
	get := func() (int, error) {
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
			ConnectStart: func(string, string) { dials.Add(1) },
		}), http.MethodGet, endpoint.BaseURL+"/github/rest/repos/a/b", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "github.com"
		req.Header.Set("Authorization", "Bearer "+endpoint.Token)
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return 0, err
		}
		return resp.StatusCode, nil
	}
	for range 2 {
		if status, err := get(); err != nil || status != 200 {
			t.Fatalf("online GET = %d, %v", status, err)
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want one kept-alive connection", dials.Load())
	}

	if err := h.HarnessForgeOffline(true); err != nil {
		t.Fatal(err)
	}
	before := len(h.HarnessForgeInvocations(0).Invocations)
	for range 2 {
		if status, err := get(); err == nil {
			t.Fatalf("offline GET = %d; want no reply", status)
		}
	}
	if dials.Load() != 3 {
		t.Fatalf("dials = %d, want one fresh connection per offline request", dials.Load())
	}
	offline := h.HarnessForgeInvocations(0).Invocations[before:]
	if len(offline) != 2 || offline[0].Route != "offline" || offline[1].Route != "offline" || offline[0].Via != "http" {
		t.Fatalf("offline invocations = %+v, want two http requests recorded as offline", offline)
	}
	if err := h.HarnessForgeOffline(true); err != nil {
		t.Fatalf("repeating offline: %v", err)
	}

	if err := h.HarnessForgeOffline(false); err != nil {
		t.Fatal(err)
	}
	if status, err := get(); err != nil || status != 200 {
		t.Fatalf("GET after coming back = %d, %v", status, err)
	}
	if err := h.HarnessForgeOffline(true); err != nil {
		t.Fatal(err)
	}
	if err := h.HarnessReset(); err != nil {
		t.Fatalf("HarnessReset: %v", err)
	}
	// Back online with the fixture cleared: a reply, the forge's 404.
	if status, err := get(); err != nil || status != 404 {
		t.Fatalf("GET after HarnessReset = %d, %v", status, err)
	}
}
