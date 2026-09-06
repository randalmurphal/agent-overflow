//go:build !nogui

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"agent-overflow/internal/clientmode"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/kerneltest"
)

func TestFrontendRestartsWithoutALaunchComputer(t *testing.T) {
	kerneltest.IsolateSpawns(t)
	oldRoot := dataDirRoot
	dataDirRoot = t.TempDir()
	t.Cleanup(func() { dataDirRoot = oldRoot })
	args := frontendRelaunchArgs(dataDirRoot)
	flags, err := parseFlags(args)
	if err != nil || !flags.frontend || flags.connect != "" || flags.dataDir != dataDirRoot {
		t.Fatalf("restart is not an isolated frontend: %+v %v", flags, err)
	}
	if !slices.Equal(frontendRelaunchArgs(""), []string{"--frontend"}) {
		t.Fatal("ordinary frontend relaunch must not name an expired invitation or removed computer")
	}
	s, err := serveClientWindow(clientmode.Config{Assets: fstest.MapFS{"index.html": {Data: []byte("frontend")}}}, clientWindowHooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	page, err := url.Parse(s.AppURL())
	if err != nil || page.Query().Get("mode") != "frontend" || page.Query().Get("computer") != "" {
		t.Fatalf("frontend without a launch computer: %v %v", page, err)
	}
}

func TestPairedFrontendBootIsOfflineAndKeepsItsOriginAcrossLaunches(t *testing.T) {
	kerneltest.IsolateSpawns(t)
	oldRoot := dataDirRoot
	dataDirRoot = t.TempDir()
	t.Cleanup(func() { dataDirRoot = oldRoot })
	var requests atomic.Int32
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	profiles, err := deviceProfileDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deviceclient.EnrollDeviceKey(profiles); err != nil {
		t.Fatal(err)
	}
	const id = "33333333-3333-4333-8333-333333333333"
	if err := deviceclient.SaveSession(profiles, deviceclient.Session{BackendID: id, Endpoint: remote.URL, SessionID: "fixture-session", Credential: "fixture-credential"}); err != nil {
		t.Fatal(err)
	}
	var origin string
	var frontendID string
	desktopID := ensureClientID()
	for range 2 {
		cfg, err := resolveConnection(t.Context(), profiles, id)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ClientID = "test-frontend"
		cfg.Assets = fstest.MapFS{"index.html": {Data: []byte("frontend")}}
		s, err := serveClientWindow(cfg, clientWindowHooks{})
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" && s.Addr() != origin {
			t.Errorf("frontend origin moved: %q to %q", origin, s.Addr())
		}
		origin = s.Addr()
		pageURL, err := url.Parse(s.AppURL())
		if err != nil {
			t.Fatal(err)
		}
		id := pageURL.Query().Get("cid")
		if id == "" || id == desktopID || (frontendID != "" && id != frontendID) {
			t.Errorf("frontend identity must be independent and stable: %q", id)
		}
		frontendID = id
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err = s.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("frontend boot made %d upstream requests", requests.Load())
	}
}
