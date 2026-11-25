package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/externalurl"
	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// newTrackerTestApp is an App with a store, a config folder and the
// tracker service, as Start builds it; configure runs before the service
// is built.
func newTrackerTestApp(t *testing.T, configure func(*App)) *App {
	t.Helper()
	a := &App{store: storetest.Clone(t), configDir: t.TempDir(), version: "1.2.3", appCtx: t.Context()}
	if configure != nil {
		configure(a)
	}
	if err := a.initTrackers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.trackers.Stop)
	return a
}

func TestTrackerMethodsCarryTheirScopeAndRoute(t *testing.T) {
	t.Parallel()
	type want struct {
		scope transport.Scope
		route transport.MethodRoute
	}
	// A method that names a connection is routed by its client
	// (family trackerConnection) and generated as home.
	read, operate, host := transport.ScopeTrackerRead, transport.ScopeTrackerOperate, transport.ScopeHost
	for name, w := range map[string]want{
		"TrackerConnections":              {read, transport.RouteSelected},
		"AddTrackerConnection":            {operate, transport.RouteSelected},
		"RenameTrackerConnection":         {operate, transport.RouteHome},
		"SetTrackerConnectionEnabled":     {operate, transport.RouteHome},
		"SetTrackerConnectionTokenExpiry": {operate, transport.RouteHome},
		"RemoveTrackerConnection":         {operate, transport.RouteHome},
		"VerifyTrackerConnection":         {operate, transport.RouteHome},
		"EditTrackerKeyFile":              {host, transport.RouteHome},
		"OpenTrackerTokenPage":            {host, transport.RouteHome},
	} {
		i := slices.IndexFunc(transport.GeneratedMethods, func(m transport.MethodMeta) bool { return m.Name == name })
		if i < 0 {
			t.Errorf("%s is not a generated method", name)
			continue
		}
		m := transport.GeneratedMethods[i]
		if m.Scope != w.scope || m.Route != w.route || m.CallerBound || m.Budget != 0 {
			t.Errorf("%s = %+v, want scope %s, route %s, not caller-bound, the default budget", name, m, w.scope, w.route)
		}
	}
	if transport.ScopeTrackerRead.Tier() != transport.TierObserve || transport.ScopeTrackerOperate.Tier() != transport.TierExecute {
		t.Fatal("tracker:read is not observe or tracker:operate is not execute")
	}
}

// trackerTestToken is the token these tests put in key files: long enough
// for the transport, which refuses a secret no longer than
// secret.Redacted, and recognizable wherever it might leak.
const trackerTestToken = "jira-SECRET-token-0001"

// Every bound method reaches the service, and every change reaches an
// event bus subscriber on tracker:connections_updated.
func TestTrackerMethodsReachTheService(t *testing.T) {
	t.Parallel()
	const token = trackerTestToken
	fake := jiratest.NewServer(t)
	fake.On("GET", "/rest/api/3/myself", func(w http.ResponseWriter, r *http.Request, _ []byte) {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user1@example.com:"+token))
		if r.Header.Get("Authorization") != want {
			t.Errorf("the fake Jira saw %q", r.Header.Get("Authorization"))
		}
		jiratest.Reply(http.StatusOK, `{"accountId":"acc-user1","displayName":"User 1","active":true}`)(w, r, nil)
	})
	a := newTrackerTestApp(t, func(a *App) {
		ConfigureIsolation(a, IsolationConfig{TrackerOrigin: func(string, string) (string, error) { return fake.URL(), nil }})
	})
	bus := transport.NewEventBus(16)
	t.Cleanup(bus.Close)
	a.SetEventBus(bus)
	sub := bus.Subscribe()
	t.Cleanup(sub.Close)
	announced := func(what string) {
		t.Helper()
		select {
		case e := <-sub.Events():
			if e.Channel != string(eventchan.TrackerConnectionsUpdated) {
				t.Fatalf("%s announced on %s", what, e.Channel)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not announced", what)
		}
	}
	ctx := context.Background()

	conn, err := a.AddTrackerConnection(ctx, NewTrackerConnection{Kind: "jira", Site: "example.atlassian.net", Account: "user1@example.com"})
	if err != nil || conn.Site != "https://example.atlassian.net" || conn.KeyFileState != keyfile.StateEmpty {
		t.Fatalf("AddTrackerConnection = %+v, %v", conn, err)
	}
	announced("Add")
	if conn, err = a.RenameTrackerConnection(ctx, conn.ID, "Work"); err != nil || conn.Label != "Work" {
		t.Fatalf("RenameTrackerConnection = %+v, %v", conn, err)
	}
	announced("Rename")
	if conn, err = a.SetTrackerConnectionTokenExpiry(ctx, conn.ID, 1_900_000_000_000); err != nil || conn.TokenExpiresAt != 1_900_000_000_000 {
		t.Fatalf("SetTrackerConnectionTokenExpiry = %+v, %v", conn, err)
	}
	announced("SetTokenExpiry")
	if err := os.WriteFile(conn.KeyFilePath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	if conn, err = a.VerifyTrackerConnection(ctx, conn.ID); err != nil || conn.Problem != nil || conn.VerifiedAccountID != "acc-user1" {
		t.Fatalf("VerifyTrackerConnection = %+v, %v", conn, err)
	}
	announced("Verify")
	list, err := a.TrackerConnections(ctx)
	if err != nil || len(list) != 1 || list[0].KeyFileState != keyfile.StatePresent || list[0].VerifiedName != "User 1" {
		t.Fatalf("TrackerConnections = %+v, %v", list, err)
	}
	if conn, err = a.SetTrackerConnectionEnabled(ctx, conn.ID, false); err != nil || conn.Enabled {
		t.Fatalf("SetTrackerConnectionEnabled = %+v, %v", conn, err)
	}
	announced("SetEnabled")
	// An isolated App opens no browser: the page the service named is
	// checked and refused at the opener.
	if err := a.OpenTrackerTokenPage(ctx, conn.ID); !errors.Is(err, externalurl.ErrNoOpener) {
		t.Fatalf("OpenTrackerTokenPage = %v, want the isolated opener's refusal", err)
	}
	if _, err := a.EditTrackerKeyFile(ctx, "missing"); !errors.Is(err, trackerapp.ErrNotFound) {
		t.Fatalf("EditTrackerKeyFile of a missing connection = %v", err)
	}
	if err := a.RemoveTrackerConnection(ctx, conn.ID); err != nil {
		t.Fatal(err)
	}
	announced("Remove")
	if list, err := a.TrackerConnections(ctx); err != nil || len(list) != 0 {
		t.Fatalf("TrackerConnections after Remove = %+v, %v", list, err)
	}
	if strings.Contains(conn.KeyFilePath, token) {
		t.Fatal("the key file path names the token")
	}
}

// A test App reaches no tracker: without an isolated boot's fake, every
// connection is refused before anything is dialed, whether or not the
// App went through ConfigureIsolation.
func TestATestAppCannotReachARealTracker(t *testing.T) {
	t.Parallel()
	var dialed atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dialed.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	for name, configure := range map[string]func(*App){
		"a test App":       nil,
		"an isolated boot": func(a *App) { ConfigureIsolation(a, IsolationConfig{}) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newTrackerTestApp(t, configure)
			// The site names the loopback server as a real site would name
			// its host: the production origin would dial it.
			conn, err := a.AddTrackerConnection(context.Background(), NewTrackerConnection{
				Kind: store.TrackerKindJira, Site: server.URL, Account: "user1@example.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(conn.KeyFilePath, []byte(trackerTestToken), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := a.VerifyTrackerConnection(context.Background(), conn.ID)
			if err != nil || got.Problem == nil || got.Problem.Kind != trackerapp.FailureSetup ||
				!strings.Contains(got.Problem.Message, errTrackersIsolated.Error()) {
				t.Fatalf("VerifyTrackerConnection = %+v, %v", got.Problem, err)
			}
		})
	}
	t.Cleanup(func() {
		if n := dialed.Load(); n != 0 {
			t.Errorf("a test App dialed the tracker %d times", n)
		}
	})
}

func TestTrackerMethodsBeforeStart(t *testing.T) {
	t.Parallel()
	a := &App{}
	if _, err := a.TrackerConnections(context.Background()); err == nil {
		t.Fatal("TrackerConnections answered before Start")
	}
	if err := a.RemoveTrackerConnection(context.Background(), "x"); err == nil {
		t.Fatal("RemoveTrackerConnection answered before Start")
	}
}

// Shutdown stops the tracker service before the store closes: a call
// after it is refused.
func TestShutdownStopsTheTrackerService(t *testing.T) {
	a, rec := newFullyWiredTestApp(t)
	a.configDir = t.TempDir()
	a.version = "1.2.3"
	if err := a.initTrackers(); err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	steps := rec.snapshot()
	stop, closeStore := slices.Index(steps, "stop tracker connections"), slices.Index(steps, "close store")
	if stop < 0 || closeStore < 0 || stop > closeStore {
		t.Fatalf("shutdown steps %v do not stop the tracker service before the store closes", steps)
	}
	if _, err := a.TrackerConnections(context.Background()); !errors.Is(err, trackerapp.ErrStopped) {
		t.Fatalf("TrackerConnections after Shutdown = %v", err)
	}
}
