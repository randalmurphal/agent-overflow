//go:build !windows

package supervise_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/localcontrol"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"

	_ "modernc.org/sqlite"
)

// TestProductionServiceArtifact is an opt-in gate over two actual builds.
// Ordinary go-test never starts a production app. Both versions run against
// the same disposable database and an empty home, with mocked startup probes.
func TestProductionServiceArtifact(t *testing.T) {
	baseline := os.Getenv("AO_SERVICE_SMOKE_BASELINE")
	candidate := os.Getenv("AO_SERVICE_SMOKE_CANDIDATE")
	if baseline == "" && candidate == "" {
		t.Skip("set AO_SERVICE_SMOKE_BASELINE and AO_SERVICE_SMOKE_CANDIDATE to two production artifacts")
	}
	if !filepath.IsAbs(baseline) || !filepath.IsAbs(candidate) {
		t.Fatal("both artifact paths must be absolute")
	}
	isolation := kerneltest.IsolateSpawns(t)
	// No inherited developer PATH, provider homes, agents, tokens or proxy.
	guardBin := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		if err := os.Symlink(isolation.PoisonedBinary, filepath.Join(guardBin, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Shell PATH synchronization is part of a real boot. Execute its probe
	// without sourcing system login profiles that could restore a real CLI.
	shell := filepath.Join(guardBin, "isolated-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec /bin/sh -c \"$2\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	guardPath := guardBin + ":/usr/bin:/bin"
	t.Setenv("PATH", guardPath)
	t.Setenv("SHELL", shell)
	env := []string{"PATH=" + guardPath, "SHELL=" + shell, "HOME=" + isolation.Home, "USERPROFILE=" + isolation.Home,
		"XDG_CONFIG_HOME=" + isolation.Home, "XDG_DATA_HOME=" + isolation.Home, "XDG_CACHE_HOME=" + isolation.Home,
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost"}
	dataRoot := t.TempDir()
	// --data-dir overrides the config ROOT; the app adds agent-overflow.
	data := filepath.Join(dataRoot, appdirs.DirName)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	providerSettings := kerneltest.ProviderBinarySettings(isolation.PoisonedBinary)
	// Production boot probes Claude discovery and the Codex model catalog.
	// Mock those explicit boundaries; PATH remains poisoned for lost settings.
	providerSettings["claudeBinaryPath"] = testutil.WriteMockClaudeInit(t, t.TempDir(), "")
	providerSettings["codexBinaryPath"] = testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		"initialize": `{"jsonrpc":"2.0","id":%d,"result":{}}`,
		"model/list": `{"jsonrpc":"2.0","id":%d,"result":{"data":[]}}`,
		"":           `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"not a startup probe"}}`,
	})
	settings, err := json.Marshal(providerSettings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	layout, err := supervise.NewLayout(data)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion := stageProductionArtifact(t, layout, baseline)
	newVersion := stageProductionArtifact(t, layout, candidate)
	if oldVersion == newVersion {
		t.Fatal("the artifacts must report distinct versions")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	log, err := os.CreateTemp(t.TempDir(), "service-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	t.Cleanup(func() {
		if t.Failed() {
			body, _ := os.ReadFile(log.Name())
			t.Logf("isolated backend log:\n%s", body)
		}
	})
	oldBinary, err := layout.VersionBinary(oldVersion)
	if err != nil {
		t.Fatal(err)
	}
	config := supervise.Config{DataDir: data, SelfExecutable: oldBinary, SelfVersion: oldVersion,
		ChildArgs: []string{"serve", "--data-dir", dataRoot, "--listen", address}, Env: env,
		Stdout: log, Stderr: log, LegacyTrialBudget: 30 * time.Second, StopTimeout: 10 * time.Second,
		// The transitions, including which judge each trial had.
		Log: t.Logf}
	start := func(wantVersion string) (string, func()) {
		t.Helper()
		supervisor, err := supervise.New(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- supervisor.Run(ctx) }()
		stopped := false
		stop := func() {
			t.Helper()
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("supervisor: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("supervisor did not stop its child")
			}
		}
		t.Cleanup(stop)
		identity, err := awaitProductionBackend(address, data, wantVersion, time.Now().Add(productionArtifactWait))
		if err != nil {
			t.Fatal(err)
		}
		return identity, stop
	}
	identity, stop := start(oldVersion)
	stop()
	// A real SQLite row proves the update preserves existing data; the
	// backend identity above independently proves the install is retained.
	database := filepath.Join(data, "agent-overflow.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE service_smoke_marker (value TEXT NOT NULL); INSERT INTO service_smoke_marker VALUES ('retained across trial')")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := supervise.LoadState(layout)
	if err != nil || !found {
		t.Fatalf("baseline state: found=%v, err=%v", found, err)
	}
	state, err = state.Begin("production-artifact-smoke", newVersion, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := supervise.SaveState(layout, state); err != nil {
		t.Fatal(err)
	}
	updatedIdentity, stop := start(newVersion)
	productionArtifactEventually(t, func() bool {
		state, found, err := supervise.LoadState(layout)
		return err == nil && found && state.Update != nil && state.Update.State == supervise.UpdateCommitted
	}, "durable trial commit")
	stop()
	restartedIdentity, stop := start(newVersion)
	stop()
	if updatedIdentity != identity || restartedIdentity != identity {
		t.Fatal("updating or restarting changed the backend identity")
	}
	db, err = sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var marker string
	if err := db.QueryRow("SELECT value FROM service_smoke_marker").Scan(&marker); err != nil || marker != "retained across trial" {
		t.Fatalf("persisted row after update and restart: %q, %v", marker, err)
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("database integrity after update: %q, %v", integrity, err)
	}
	t.Logf("production artifacts: %s → %s; trial committed, restart retained identity and SQLite data", oldVersion, newVersion)
}

func stageProductionArtifact(t *testing.T, layout supervise.Layout, source string) string {
	t.Helper()
	// Work on a copy: extraction must not create files next to the caller's
	// release artifact, which may live in a read-only download directory.
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	copyPath := filepath.Join(t.TempDir(), filepath.Base(source))
	out, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), in)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	assetName := filepath.Base(source)
	if strings.HasSuffix(assetName, ".zip") {
		assetName = "agent-overflow-darwin-" + runtime.GOARCH + ".zip"
	}
	artifact, err := supervise.PrepareArtifact(context.Background(), copyPath, assetName, hex.EncodeToString(hash.Sum(nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	answer, err := supervise.PreflightBinary(context.Background(), artifact.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervise.CheckPreflight(answer); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Stage(layout, answer.Version); err != nil {
		t.Fatal(err)
	}
	return answer.Version
}

const (
	productionArtifactWait = 40 * time.Second
	productionArtifactPoll = 50 * time.Millisecond
)

func productionArtifactEventually(t *testing.T, predicate func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(productionArtifactWait)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(productionArtifactPoll)
	}
	t.Fatal("timed out waiting for " + description)
}

// awaitProductionBackend waits until the backend at address runs
// wantVersion and is ready, and returns its identity. /healthz is liveness:
// it answers, with the identity, while the backend migrates. Readiness is
// /bootstrap.json answering 200, which it does once its start finishes and
// answers 503 with the startup progress before. The request presents the
// launch credential the backend publishes in its control file.
func awaitProductionBackend(address, data, wantVersion string, deadline time.Time) (string, error) {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	last := errors.New("no answer yet")
	for time.Now().Before(deadline) {
		identity, err := productionBackendReady(client, address, data, wantVersion)
		if err == nil {
			return identity, nil
		}
		last = err
		time.Sleep(productionArtifactPoll)
	}
	return "", errors.New("timed out waiting for a ready production backend " + wantVersion + ": " + last.Error())
}

func productionBackendReady(client *http.Client, address, data, wantVersion string) (string, error) {
	var health transport.Health
	if err := productionGet(client, "http://"+address+transport.HealthPath, "", &health); err != nil {
		return "", err
	}
	if health.Version != wantVersion {
		return "", errors.New("/healthz names version " + health.Version)
	}
	// The control file of a launch that has finished its start. An earlier
	// launch's file holds a token this backend refuses.
	endpoint, err := localcontrol.Read(data)
	if err != nil {
		return "", err
	}
	var manifest transport.Bootstrap
	if err := productionGet(client, "http://"+address+transport.BootstrapPath, endpoint.Token, &manifest); err != nil {
		return "", err
	}
	if manifest.BackendID == "" {
		return "", errors.New("/bootstrap.json names no backend")
	}
	return manifest.BackendID, nil
}

func productionGet(client *http.Client, url, token string, into any) error {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New(url + " answered " + response.Status)
	}
	return json.NewDecoder(response.Body).Decode(into)
}

// TestAwaitProductionBackendWaitsForReadiness: the smoke stops each backend
// once it counts as up, so one that has not finished starting must not
// count. /healthz answers throughout, as liveness does: first without an
// identity, while the migrations run, then with one once the store opens.
// /bootstrap.json meanwhile refuses the previous launch's token, then
// answers 503 with the startup progress, then 200.
func TestAwaitProductionBackendWaitsForReadiness(t *testing.T) {
	data := t.TempDir()
	const version, identity = "0.0.900", "backend-1"
	const (
		migrating = iota
		storeOpen
		ready
	)
	var (
		phase       atomic.Int32
		held        atomic.Int32
		heldEnough  = make(chan struct{})
		stale, live = "previous-launch", "this-launch"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transport.HealthPath:
			health := transport.Health{Version: version}
			if phase.Load() != migrating {
				health.BackendID = identity
			}
			_ = json.NewEncoder(w).Encode(health)
		case transport.BootstrapPath:
			if r.Header.Get("Authorization") != "Bearer "+live {
				http.NotFound(w, r)
				return
			}
			if phase.Load() != ready {
				if held.Add(1) == 3 {
					close(heldEnough)
				}
				startupprogress.Write(w, startupprogress.Progress{Phase: "app.init_subsystems", Detail: "Starting"})
				return
			}
			_ = json.NewEncoder(w).Encode(transport.Bootstrap{BackendID: identity})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	address := server.Listener.Addr().String()
	if err := localcontrol.Publish(data, address, stale); err != nil {
		t.Fatal(err)
	}
	type result struct {
		identity string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		id, err := awaitProductionBackend(address, data, version, time.Now().Add(30*time.Second))
		done <- result{id, err}
	}()
	// Migrating, with the previous launch's control file.
	select {
	case got := <-done:
		t.Fatalf("the wait ended while the migrations run: %q, %v", got.identity, got.err)
	case <-time.After(10 * productionArtifactPoll):
	}
	// The store is open and /healthz names the identity; this launch's token
	// gets 503 while the start continues.
	phase.Store(storeOpen)
	if err := localcontrol.Publish(data, address, live); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		t.Fatalf("the wait ended before the backend was ready: %q, %v", got.identity, got.err)
	case <-heldEnough:
	}
	select {
	case got := <-done:
		t.Fatalf("the wait ended before the backend was ready: %q, %v", got.identity, got.err)
	default:
	}
	phase.Store(ready)
	select {
	case got := <-done:
		if got.err != nil || got.identity != identity {
			t.Fatalf("a ready backend: %q, %v", got.identity, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the smoke did not count a ready backend up")
	}
	// Ready, but not the version the smoke waits for.
	if id, err := awaitProductionBackend(address, data, "0.0.901", time.Now().Add(5*productionArtifactPoll)); err == nil {
		t.Fatalf("the smoke counted another version up: %q", id)
	}
}
