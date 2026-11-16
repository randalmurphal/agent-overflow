package trackerapi

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httputil"
	"strings"
	"testing"
)

// The credential goes only to the base origin: a redirect to another
// origin, and an absolute URL on one, are requested without it.
func TestCredentialStaysOnTheBaseOrigin(t *testing.T) {
	t.Parallel()
	media := newTracker(t, route{
		"GET /file/1": func(w http.ResponseWriter, r *http.Request) { writeString(w, "content") },
	})
	tr := newTracker(t, route{
		"GET /attachment/content/1": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, media.srv.URL+"/file/1?token=signed", http.StatusSeeOther)
		},
		"GET /attachment/local": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/file/2", http.StatusFound)
		},
		"GET /file/2": func(w http.ResponseWriter, r *http.Request) { writeString(w, "local") },
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	var dst bytes.Buffer
	if _, err := c.Stream(t.Context(), Request{Path: "attachment/content/1"}, &dst, 1<<20); err != nil || dst.String() != "content" {
		t.Fatalf("Stream = %v, %q", err, dst.String())
	}
	if got := media.last().Header.Get("Authorization"); got != "" {
		t.Fatalf("the redirect target received the credential: %q", got)
	}
	if got := tr.last().Header.Get("Authorization"); got == "" {
		t.Fatal("the base origin received no credential")
	}
	if _, err := c.Do(t.Context(), Request{Path: media.srv.URL + "/file/1", Absolute: true}); err != nil {
		t.Fatal(err)
	}
	if got := media.last().Header.Get("Authorization"); got != "" {
		t.Fatalf("an absolute URL on another origin received the credential: %q", got)
	}
	if _, err := c.Do(t.Context(), Request{Path: tr.srv.URL + "/file/2", Absolute: true}); err != nil {
		t.Fatal(err)
	}
	if tr.last().Header.Get("Authorization") == "" {
		t.Fatal("an absolute URL on the base origin received no credential")
	}
	if _, err := c.Do(t.Context(), Request{Path: "attachment/local"}); err != nil {
		t.Fatal(err)
	}
	if tr.last().Header.Get("Authorization") == "" {
		t.Fatal("a redirect within the base origin dropped the credential")
	}
}

// A redirect to plain http off loopback is refused, and the redirect
// policy drops the credential headers on a hop to another origin.
func TestRedirectPolicy(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /attachment/content/1": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://media.example.com/file/1?token=signed", http.StatusFound)
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Path: "attachment/content/1"})
	if !errors.Is(err, errRedirect) || !strings.Contains(err.Error(), "not https") || strings.Contains(err.Error(), "signed") {
		t.Fatalf("Do = %v, want the redirect refused and its query redacted", err)
	}
	if _, ok := errors.AsType[*TransientError](err); ok {
		t.Fatalf("a refused redirect is not transient: %v", err)
	}

	first, _ := http.NewRequest(http.MethodGet, "https://example.atlassian.net/a", nil)
	for target, kept := range map[string]bool{
		"https://example.atlassian.net/b":      true,
		"https://example.atlassian.net:443/b":  true,
		"https://media.example.com/b":          false,
		"https://example.atlassian.net:8443/b": false,
	} {
		hop, _ := http.NewRequest(http.MethodGet, target, nil)
		hop.Header.Set("Authorization", "Bearer x")
		hop.Header.Set("Cookie", "a=b")
		if err := checkRedirect(hop, []*http.Request{first}); err != nil {
			t.Fatal(err)
		}
		if got := hop.Header.Get("Authorization") != "" && hop.Header.Get("Cookie") != ""; got != kept {
			t.Errorf("%s: credential headers kept = %v, want %v", target, got, kept)
		}
	}
}

// A 401 from another origin is that origin's answer, not a refusal of
// the client's credential: no re-read.
func TestUnauthorizedOffTheBaseOriginIsAStatus(t *testing.T) {
	t.Parallel()
	media := newTracker(t, route{"GET /file/1": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }})
	tr := newTracker(t, route{})
	src := &testSource{secrets: []string{testSecret}}
	opts := testOptions(tr.url())
	opts.Credentials = src
	c, _ := loopbackClient(t, opts)
	_, err := c.Do(t.Context(), Request{Path: media.srv.URL + "/file/1", Absolute: true})
	if status, ok := errors.AsType[*StatusError](err); !ok || status.Status != http.StatusUnauthorized {
		t.Fatalf("Do = %v, want a *StatusError", err)
	}
	if src.readCount() != 1 {
		t.Fatalf("%d reads, want no re-read", src.readCount())
	}
}

// The request a Client builds carries no credential: only authTransport's
// per-hop clone does.
func TestBuiltRequestCarriesNoCredential(t *testing.T) {
	t.Parallel()
	c, _ := loopbackClient(t, testOptions("https://example.atlassian.net/"))
	k, err := c.prepare(Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{"a": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.loadCredential(t.Context(), false, 0); err != nil {
		t.Fatal(err)
	}
	req, err := k.request(t.Context(), nil, c.userAgent)
	if err != nil {
		t.Fatal(err)
	}
	dump, err := httputil.DumpRequestOut(req, true)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(dump, []byte(testSecret)) || bytes.Contains(bytes.ToLower(dump), []byte("authorization")) {
		t.Fatalf("the built request carries a credential:\n%s", dump)
	}
}
