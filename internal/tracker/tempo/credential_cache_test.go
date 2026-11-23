package tempo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// rotatingToken hands out token A on the first read and token B on
// every later one.
type rotatingToken struct{ reads atomic.Int32 }

func (s *rotatingToken) Credential(context.Context) (*trackerapi.Credential, error) {
	if s.reads.Add(1) == 1 {
		return trackerapi.Bearer(secret.New([]byte("fake-tempo-A"))), nil
	}
	return trackerapi.Bearer(secret.New([]byte("fake-tempo-B"))), nil
}

// TestCredentialRetiresSiteCache proves the work attributes and settings
// a token read are read again once the transport holds another token
// read, here after a refusal of token A on a call that is no cache's:
// the token selects the Tempo tenant, so a value of A's is not B's.
func TestCredentialRetiresSiteCache(t *testing.T) {
	var (
		mu      sync.Mutex
		revoked bool
		reads   = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		reads[r.URL.Path]++
		refuse := revoked && r.Header.Get("Authorization") == "Bearer fake-tempo-A"
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case refuse:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"message":"unauthorized"}]}`)
		case r.URL.Path == "/4/work-attributes":
			_, _ = io.WriteString(w, attributesJSON)
		case r.URL.Path == "/4/globalconfiguration":
			_, _ = io.WriteString(w, `{"allowLoggingOnClosedAccount":false,"approvalPeriod":"WEEK","approvalWeekStart":1,"numberOfDaysAllowedIntoFuture":30,"planApprovalEnabled":false,"weekStart":1}`)
		case r.URL.Path == "/4/worklogs/70":
			_, _ = io.WriteString(w, wl{id: 70, issue: 10002, author: "acc-user1", date: "2026-11-10", spent: 3600}.json())
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	src := &rotatingToken{}
	opts, err := transportOptions(Options{BaseURL: srv.URL, Version: "test", Credentials: src, Issues: newIssues()})
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxPerSecond = 0
	c, err := newClient(opts, newIssues())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	ctx := context.Background()
	readSite := func() {
		t.Helper()
		if _, err := c.Attributes(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Settings(ctx); err != nil {
			t.Fatal(err)
		}
	}
	readSite()
	readSite()
	mu.Lock()
	revoked = true
	mu.Unlock()
	if _, err := c.Worklog(ctx, 70); err != nil {
		t.Fatalf("the read after the rotation: %v", err)
	}
	if n := src.reads.Load(); n != 2 {
		t.Fatalf("%d token reads, want 2: the rotation did not happen", n)
	}
	readSite()
	readSite()
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/4/work-attributes", "/4/globalconfiguration"} {
		if reads[path] != 2 {
			t.Errorf("%s read %d times, want once under each token", path, reads[path])
		}
	}
}
