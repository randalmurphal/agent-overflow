package cdpclient

import (
	"strings"
	"testing"
)

func TestParseEndpointSpellings(t *testing.T) {
	cases := []struct {
		spec     string
		wantHTTP string
		wantWS   string
		wantErr  bool
	}{
		{spec: "9224", wantHTTP: "http://127.0.0.1:9224"},
		{spec: " 9225 ", wantHTTP: "http://127.0.0.1:9225"},
		{spec: "127.0.0.1:9224", wantHTTP: "http://127.0.0.1:9224"},
		{spec: "localhost:9224", wantHTTP: "http://localhost:9224"},
		{spec: "http://127.0.0.1:9224", wantHTTP: "http://127.0.0.1:9224"},
		{spec: "http://127.0.0.1:9224/json/list", wantHTTP: "http://127.0.0.1:9224"},
		{spec: "ws://127.0.0.1:9224/devtools/page/AB", wantWS: "ws://127.0.0.1:9224/devtools/page/AB"},
		{spec: "", wantErr: true},
		{spec: "not-a-port", wantErr: true},
		{spec: "127.0.0.1:not-a-port", wantErr: true},
		{spec: "0", wantErr: true},
		{spec: "99999", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseEndpoint(tc.spec)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseEndpoint(%q) = %+v, want an error", tc.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEndpoint(%q): %v", tc.spec, err)
			continue
		}
		if got.HTTPBase != tc.wantHTTP || got.WSURL != tc.wantWS {
			t.Errorf("ParseEndpoint(%q) = %+v, want http=%q ws=%q", tc.spec, got, tc.wantHTTP, tc.wantWS)
		}
	}
}

func page(url, ws string) Target {
	return Target{Type: "page", URL: url, Title: url, WebSocketDebuggerURL: ws}
}

func TestSelectPageTargetPicksTheInstancePage(t *testing.T) {
	targets := []Target{
		{Type: "service_worker", URL: "http://127.0.0.1:4321/sw.js", WebSocketDebuggerURL: "ws://sw"},
		page("https://example.com/", "ws://other"),
		page("http://127.0.0.1:4321/?token=abc", "ws://instance"),
	}
	got, err := SelectPageTarget(targets, "http://127.0.0.1:4321/?token=abc")
	if err != nil {
		t.Fatalf("SelectPageTarget: %v", err)
	}
	if got.WebSocketDebuggerURL != "ws://instance" {
		t.Fatalf("picked %q, want the instance page", got.WebSocketDebuggerURL)
	}
}

// The page is opened as localhost, the instance publishes 127.0.0.1, and
// the token query never matches — origin equality has to reconcile the
// loopback spellings or the match silently falls through to "the only
// page", which is the wrong answer as soon as a second tab exists.
func TestSelectPageTargetReconcilesLoopbackSpellings(t *testing.T) {
	targets := []Target{
		page("http://localhost:4321/?token=abc", "ws://instance"),
		page("https://example.com/", "ws://other"),
	}
	got, err := SelectPageTarget(targets, "http://127.0.0.1:4321/?token=abc")
	if err != nil {
		t.Fatalf("SelectPageTarget: %v", err)
	}
	if got.WebSocketDebuggerURL != "ws://instance" {
		t.Fatalf("picked %q, want the instance page", got.WebSocketDebuggerURL)
	}
}

func TestSelectPageTargetRefusesAnUnmarkedOnlyPage(t *testing.T) {
	targets := []Target{
		{Type: "background_page", URL: "chrome://x", WebSocketDebuggerURL: "ws://bg"},
		page("about:blank", "ws://only"),
	}
	if _, err := SelectPageTarget(targets, "http://127.0.0.1:4321/?page=expected"); err == nil {
		t.Fatal("an unmarked sole page must not be selected")
	}
}

func TestSelectPageTargetRefusesAmbiguity(t *testing.T) {
	targets := []Target{
		page("http://example.com/a", "ws://a"),
		page("http://example.org/b", "ws://b"),
	}
	_, err := SelectPageTarget(targets, "http://127.0.0.1:4321/")
	if err == nil {
		t.Fatal("two unrelated pages should be an error, not a guess")
	}
	for _, want := range []string{"ws://a", "ws://b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not list candidate %q: %v", want, err)
		}
	}
}

func TestSelectPageTargetRefusesTwoPagesOnTheSameOrigin(t *testing.T) {
	targets := []Target{
		page("http://127.0.0.1:4321/?token=a", "ws://a"),
		page("http://127.0.0.1:4321/design/?token=a", "ws://b"),
	}
	_, err := SelectPageTarget(targets, "http://127.0.0.1:4321/?token=a")
	if err == nil {
		t.Fatal("two pages on the instance origin should be an error, not a guess")
	}
	if !strings.Contains(err.Error(), "ws://b") {
		t.Errorf("error does not list the candidates: %v", err)
	}
}

// A page with no webSocketDebuggerUrl is one another debugger client
// already holds. "No page target" is the wrong diagnosis for it: the page
// is right there, and the fix is to close the other client.
func TestSelectPageTargetNamesAClaimedPage(t *testing.T) {
	targets := []Target{page("http://127.0.0.1:4321/", "")}
	_, err := SelectPageTarget(targets, "http://127.0.0.1:4321/")
	if err == nil {
		t.Fatal("a claimed page cannot be attached to")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error should name the claim: %v", err)
	}
}

func TestSelectPageTargetWithNoPages(t *testing.T) {
	_, err := SelectPageTarget([]Target{{Type: "worker", URL: "x"}}, "")
	if err == nil || !strings.Contains(err.Error(), "no page target") {
		t.Fatalf("want a no-page-target error, got %v", err)
	}
}

func TestSelectPageTargetRefusesWrongPageMarker(t *testing.T) {
	targets := []Target{page("http://127.0.0.1:4321/?page=instance-marker", "ws://instance")}
	if _, err := SelectPageTarget(targets, "http://127.0.0.1:4321/?page=other-marker"); err == nil {
		t.Fatal("a page with the wrong marker must not be selected")
	}
}

func TestSelectPageTargetForPageDisambiguatesSameOriginPages(t *testing.T) {
	targets := []Target{
		page("http://127.0.0.1:4321/?page=instance-marker&pageId=other", "ws://other"),
		page("http://127.0.0.1:4321/?page=instance-marker&pageId=wanted", "ws://wanted"),
	}
	got, err := SelectPageTargetForPage(targets, "http://127.0.0.1:4321/?page=instance-marker", "wanted")
	if err != nil {
		t.Fatalf("SelectPageTargetForPage: %v", err)
	}
	if got.WebSocketDebuggerURL != "ws://wanted" {
		t.Fatalf("picked %q, want ws://wanted", got.WebSocketDebuggerURL)
	}
}
