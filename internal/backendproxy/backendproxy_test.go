package backendproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
