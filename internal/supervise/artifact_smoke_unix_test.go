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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/supervise"
	"agent-overflow/internal/testutil"

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
		Stdout: log, Stderr: log, TrialBudget: 30 * time.Second, StopTimeout: 10 * time.Second}
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
		client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
		defer client.CloseIdleConnections()
		var identity string
		productionArtifactEventually(t, func() bool {
			response, err := client.Get("http://" + address + "/healthz")
			if err != nil {
				return false
			}
			defer response.Body.Close()
			var health struct{ Version, BackendID string }
			if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&health) != nil {
				return false
			}
			identity = health.BackendID
			return health.Version == wantVersion && identity != ""
		}, "healthy production backend "+wantVersion)
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

func productionArtifactEventually(t *testing.T, predicate func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + description)
}
