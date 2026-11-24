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
	for _, tc := range []struct {
		name, instanceURL, pageID, want string
		targets                         []Target
	}{
		{name: "the instance page", instanceURL: "http://127.0.0.1:4321/?token=abc", want: "ws://instance", targets: []Target{
			{Type: "service_worker", URL: "http://127.0.0.1:4321/sw.js", WebSocketDebuggerURL: "ws://sw"},
			page("https://example.com/", "ws://other"),
			page("http://127.0.0.1:4321/?token=abc", "ws://instance"),
		}},
		// The page is opened as localhost, the instance publishes 127.0.0.1,
		// and the token query never matches — origin equality has to
		// reconcile the loopback spellings or the match silently falls
		// through to "the only page", which is the wrong answer as soon as a
		// second tab exists.
		{name: "loopback spellings", instanceURL: "http://127.0.0.1:4321/?token=abc", want: "ws://instance", targets: []Target{
			page("http://localhost:4321/?token=abc", "ws://instance"),
			page("https://example.com/", "ws://other"),
		}},
		{name: "page id among same-origin pages", instanceURL: "http://127.0.0.1:4321/?page=instance-marker", pageID: "wanted", want: "ws://wanted", targets: []Target{
			page("http://127.0.0.1:4321/?page=instance-marker&pageId=other", "ws://other"),
			page("http://127.0.0.1:4321/?page=instance-marker&pageId=wanted", "ws://wanted"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectPageTarget(tc.targets, tc.instanceURL, tc.pageID)
			if err != nil {
				t.Fatalf("selectPageTarget: %v", err)
			}
			if got.WebSocketDebuggerURL != tc.want {
				t.Fatalf("picked %q, want %q", got.WebSocketDebuggerURL, tc.want)
			}
		})
	}
}

// selectPageTarget refuses to guess, and its error names what it saw. A page
// with no webSocketDebuggerUrl is one another debugger client already holds:
// "no page target" is the wrong diagnosis for it, since the page is right
// there and the fix is to close the other client.
func TestSelectPageTargetRefuses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		targets     []Target
		instanceURL string
		want        []string
	}{
		{"an unmarked only page", []Target{
			{Type: "background_page", URL: "chrome://x", WebSocketDebuggerURL: "ws://bg"},
			page("about:blank", "ws://only"),
		}, "http://127.0.0.1:4321/?page=expected", nil},
		{"two unrelated pages", []Target{
			page("http://example.com/a", "ws://a"),
			page("http://example.org/b", "ws://b"),
		}, "http://127.0.0.1:4321/", []string{"ws://a", "ws://b"}},
		{"two pages on the instance origin", []Target{
			page("http://127.0.0.1:4321/?token=a", "ws://a"),
			page("http://127.0.0.1:4321/design/?token=a", "ws://b"),
		}, "http://127.0.0.1:4321/?token=a", []string{"ws://b"}},
		{"a claimed page", []Target{page("http://127.0.0.1:4321/", "")}, "http://127.0.0.1:4321/", []string{"already claimed"}},
		{"no pages", []Target{{Type: "worker", URL: "x"}}, "", []string{"no page target"}},
		{"the wrong page marker", []Target{page("http://127.0.0.1:4321/?page=instance-marker", "ws://instance")},
			"http://127.0.0.1:4321/?page=other-marker", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectPageTarget(tc.targets, tc.instanceURL, "")
			if err == nil {
				t.Fatalf("selected %q; want a refusal", got.WebSocketDebuggerURL)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %q: %v", want, err)
				}
			}
		})
	}
}
