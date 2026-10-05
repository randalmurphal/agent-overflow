//go:build !noremote

package deviceclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer counts the connections it accepts. /hold answers once two
// requests are waiting, so two concurrent ones leave two idle connections;
// /hang answers only when its caller gives up.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var opened atomic.Int32
	var mu sync.Mutex
	waiting, both := 0, make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hold":
			mu.Lock()
			if waiting++; waiting == 2 {
				close(both)
			}
			mu.Unlock()
			<-both
		case "/hang":
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server, &opened
}

func get(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return err
}

func TestPinnedTransportRedialsAfterItsIdleWindow(t *testing.T) {
	previous := pinnedIdleConnTimeout
	pinnedIdleConnTimeout = 100 * time.Millisecond
	t.Cleanup(func() { pinnedIdleConnTimeout = previous })
	server, opened := countingServer(t)
	client := &http.Client{Transport: NewPinnedTransport("")}
	for range 2 {
		if err := get(context.Background(), client, server.URL); err != nil {
			t.Fatal(err)
		}
	}
	if got := opened.Load(); got != 1 {
		t.Fatalf("a burst opened %d connections, want 1", got)
	}
	time.Sleep(3 * pinnedIdleConnTimeout)
	if err := get(context.Background(), client, server.URL); err != nil {
		t.Fatal(err)
	}
	if got := opened.Load(); got != 2 {
		t.Fatalf("opened %d connections, want a new one after the idle window", got)
	}
}

func TestAFailedRequestLeavesItsRouteNoIdleConnection(t *testing.T) {
	server, opened := countingServer(t)
	be := &backend{Server: server, spent: map[string]bool{}, credential: "ao1.issued-0"}
	client, _ := openAgainst(t, be, nil)
	do := func(ctx context.Context, path string) error {
		req, err := client.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		response, err := client.http.Do(req)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return err
	}
	var held sync.WaitGroup
	for range 2 {
		held.Go(func() {
			if err := do(context.Background(), "/hold"); err != nil {
				t.Error(err)
			}
		})
	}
	held.Wait()
	if got := opened.Load(); got != 2 {
		t.Fatalf("opened %d connections, want two idle ones", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := do(ctx, "/hang"); err == nil {
		t.Fatal("the hung request succeeded")
	}
	if err := do(context.Background(), "/ok"); err != nil {
		t.Fatal(err)
	}
	if got := opened.Load(); got != 3 {
		t.Fatalf("opened %d connections, want the request after a failure on a new one", got)
	}
}
