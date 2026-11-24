package backendproxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNew_RefusesNeitherCredentialAndBoth — a carrier with no credential
// reaches nothing, and one holding both has two answers to "whose request
// is this" and no rule for which wins.
func TestNew_RefusesNeitherCredentialAndBoth(t *testing.T) {
	if _, err := New(Config{WSURL: "ws://example.test/ws"}); err == nil {
		t.Error("a carrier with neither credential was built")
	}
	if _, err := New(Config{WSURL: "ws://example.test/ws", Token: "t", Paired: nil}); err != nil {
		t.Errorf("a token-only carrier was refused: %v", err)
	}
}

// TestCarryTransfer_StripsThePerBackendPrefix — the mount point is this
// process's own addressing. What crosses is the route the far side
// published, and the ticket on the query is its whole admission.
func TestCarryTransfer_StripsThePerBackendPrefix(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	carrier, err := New(Config{
		WSURL:          "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws",
		Token:          "upstream-launch-token",
		TransferPrefix: "/backend/mini",
	})
	if err != nil {
		t.Fatalf("new carrier: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/backend/mini/attachments/thread-1/att-2?ticket=single-use", nil)
	// A local caller that is not a browser could put a credential on the
	// request; nothing it holds means anything upstream.
	req.Header.Set("Authorization", "Bearer this-listeners-cookie")
	rec := httptest.NewRecorder()
	carrier.CarryTransfer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the upstream's 200", rec.Code)
	}
	if gotPath != "/attachments/thread-1/att-2" {
		t.Errorf("upstream path = %q, want the prefix stripped", gotPath)
	}
	if gotQuery != "ticket=single-use" {
		t.Errorf("upstream query = %q, want it untouched", gotQuery)
	}
	if gotAuth != "" {
		t.Errorf("a credential crossed the byte relay: %q", gotAuth)
	}
}

// TestCarryTransfer_ACutDownloadStaysCut — a download the backend ends
// after its first byte (the tracker attachment route does, since it
// declares no length) reaches the page as a failed transfer, never as a
// file that looks whole.
func TestCarryTransfer_ACutDownloadStaysCut(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64<<10))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()
	carrier, err := New(Config{
		WSURL:          "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws",
		Token:          "upstream-launch-token",
		TransferPrefix: "/backend/mini",
	})
	if err != nil {
		t.Fatalf("new carrier: %v", err)
	}
	relay := httptest.NewServer(http.HandlerFunc(carrier.CarryTransfer))
	defer relay.Close()
	resp, err := http.Get(relay.URL + "/backend/mini/attachments/tracker/501?ticket=single-use")
	if err != nil {
		return // cut before the headers: a failed transfer too
	}
	defer func() { _ = resp.Body.Close() }()
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Fatalf("a cut download read as a whole file of %d bytes", len(body))
	}
}

// TestCarryTransfer_ADownloadIsBoundedByItsProgress — a carried download
// that keeps moving outlives any total, the stall window many times over,
// and one whose page stops reading ends within the stall window, which is
// shortened here.
func TestCarryTransfer_ADownloadIsBoundedByItsProgress(t *testing.T) {
	const stall = 2 * time.Second
	ended := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.URL.Query().Get("ticket") == "moving" {
			for range 50 {
				if _, err := w.Write([]byte("x")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
			}
			return
		}
		defer func() { ended <- struct{}{} }()
		chunk := make([]byte, 1<<20)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	carrier, err := New(Config{
		WSURL: "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws",
		Token: "upstream-launch-token",
	})
	if err != nil {
		t.Fatalf("new carrier: %v", err)
	}
	carrier.stall = stall
	relay := httptest.NewServer(http.HandlerFunc(carrier.CarryTransfer))
	defer relay.Close()

	resp, err := http.Get(relay.URL + "/attachments/tracker/501?ticket=moving")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || len(body) != 50 {
		t.Fatalf("a download moving for 2.5 times its %s stall window: %d bytes, %v", stall, len(body), err)
	}

	conn, err := net.Dial("tcp", strings.TrimPrefix(relay.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /attachments/tracker/502?ticket=stalled HTTP/1.1\r\nHost: relay\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(3 * stall):
		t.Fatalf("a carried download whose page stopped reading was still being sent %s later, past its %s stall window", 3*stall, stall)
	}
}

// TestFetchBootstrap_PresentsTheConfiguredCredential — the status is the
// verdict on whether that credential is still honoured, which is the one
// question a page cannot ask for itself.
func TestFetchBootstrap_PresentsTheConfiguredCredential(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"backendId":"store-uuid"}`))
	}))
	defer upstream.Close()

	carrier, err := New(Config{
		WSURL: "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws",
		Token: "upstream-launch-token",
	})
	if err != nil {
		t.Fatalf("new carrier: %v", err)
	}
	status, body, err := carrier.FetchBootstrap(t.Context())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if gotAuth != "Bearer upstream-launch-token" {
		t.Errorf("upstream saw %q, want the configured credential", gotAuth)
	}
	if !strings.Contains(string(body), "store-uuid") {
		t.Errorf("body = %q, want the far side's manifest", body)
	}
}
