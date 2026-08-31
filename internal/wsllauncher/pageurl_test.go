package wsllauncher

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/pagehost"
	"agent-overflow/internal/transport"
)

// TestPageURLPathMatchesTransport is the drift guard for the route this
// package restates. The launcher deliberately does not link the transport
// server (it never runs one), so the path exists twice; a rename on the
// server side that missed this copy would leave the reload keybinding
// asking a route nobody serves, and only a live Windows session would
// notice. The test costs one import in test code and nothing in the
// shipped launcher.
func TestPageURLPathMatchesTransport(t *testing.T) {
	if PageURLPath != transport.PageURLPath {
		t.Fatalf("PageURLPath = %q, transport.PageURLPath = %q", PageURLPath, transport.PageURLPath)
	}
}

// TestPageURLWebviewQueryMatchesPagehost guards the other half of the
// restated route contract: the query that selects the bare-URL-plus-
// ticket answer. A rename of the marker would otherwise leave the
// launcher asking for the browser shape and injecting a ticket it never
// received, which again only a live Windows session would notice.
func TestPageURLWebviewQueryMatchesPagehost(t *testing.T) {
	want := "?" + pagehost.Param + "=" + pagehost.Webview
	if PageURLWebviewQuery != want {
		t.Fatalf("PageURLWebviewQuery = %q, want %q", PageURLWebviewQuery, want)
	}
}

// TestReadBootstrapLine_RequiresPageURL: the page URL is assembled by the
// backend, because the client id and page marker on it are the backend's
// to know. A bootstrap line without one means the launcher has nothing to
// navigate to, and a launcher that accepted it would open an empty
// WebView2 and report success. Refusing at the parse boundary turns that
// into a startup error the picker UI can show.
func TestReadBootstrapLine_RequiresPageURL(t *testing.T) {
	for name, payload := range map[string]string{
		"absent": `{"port":54321,"token":"abc123"}`,
		"empty":  `{"port":54321,"token":"abc123","pageUrl":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			stdin := strings.NewReader(DefaultBootstrapPrefix + payload + "\n")
			bs, err := readBootstrapLine(context.Background(), stdin, DefaultBootstrapPrefix, nil)
			if err == nil {
				t.Fatalf("readBootstrapLine accepted a bootstrap with no page url: %+v", bs)
			}
			if !strings.Contains(err.Error(), "invalid bootstrap") {
				t.Fatalf("error = %v, want the invalid-bootstrap shape", err)
			}
		})
	}
}
