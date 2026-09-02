package transport

import (
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/pagehost"
)

// parseCSP splits a policy into directive name → source list. The
// constants are written as one string, so a test that asserts on
// substrings would pass on a policy that says the right words in the
// wrong directive.
func parseCSP(t *testing.T, policy ContentSecurityPolicy) map[string][]string {
	t.Helper()
	directives := map[string][]string{}
	for _, part := range strings.Split(string(policy), ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			t.Fatalf("empty directive in policy %q", policy)
		}
		name := fields[0]
		if _, dup := directives[name]; dup {
			// A repeated directive is not additive: the FIRST one wins
			// and the second is ignored, so a duplicate silently
			// discards whatever the author thought they were adding.
			t.Fatalf("directive %q appears twice in policy %q", name, policy)
		}
		directives[name] = fields[1:]
	}
	return directives
}

func hasSource(sources []string, want string) bool {
	for _, source := range sources {
		if source == want {
			return true
		}
	}
	return false
}

// TestCSPProductionRefusesInlineAndEvaluatedScript pins the property the
// whole policy exists for. The production bundle carries no eval, no new
// Function, no WebAssembly and no Worker, and the SPA shell carries no
// inline script — so nothing here is a concession to the app, and any
// future need for one is a decision, not an edit.
func TestCSPProductionRefusesInlineAndEvaluatedScript(t *testing.T) {
	scriptSrc := parseCSP(t, CSPProduction)["script-src"]
	if len(scriptSrc) != 1 || scriptSrc[0] != "'self'" {
		t.Fatalf("script-src = %v, want exactly ['self'] — a hash, a nonce or a widened source is a policy change", scriptSrc)
	}
}

// TestCSPRefusesFramingAndBaseRewrites covers the directives that cost
// the app nothing and are therefore easy to drop by accident.
func TestCSPRefusesFramingAndBaseRewrites(t *testing.T) {
	for name, policy := range map[string]ContentSecurityPolicy{
		"production": CSPProduction,
		"dev-server": CSPDevServer,
	} {
		directives := parseCSP(t, policy)
		for directive, want := range map[string]string{
			// The modern half of X-Frame-Options: DENY, which
			// WriteSecurityHeaders sends beside it.
			"frame-ancestors": "'none'",
			"object-src":      "'none'",
			"base-uri":        "'self'",
			// Every <form> in the app is an onsubmit handler that
			// preventDefaults; one that forgot would navigate the SPA
			// away and lose its state. Refusing the submit is the
			// better failure.
			"form-action": "'none'",
			// The floor for media-src, worker-src, manifest-src and
			// frame-src, none of which the bundle uses today.
			"default-src": "'self'",
		} {
			sources, ok := directives[directive]
			if !ok {
				t.Errorf("%s policy: %s missing", name, directive)
				continue
			}
			if len(sources) != 1 || sources[0] != want {
				t.Errorf("%s policy: %s = %v, want [%s]", name, directive, sources, want)
			}
		}
	}
}

// TestCSPAdmitsWhatTheBundleLoads is the other direction: a policy
// tightened past what the app actually loads breaks the app silently in
// a browser and loudly nowhere else. Each of these is a load observed in
// the shipped bundle, named so a future tightening has to argue with the
// reason rather than with a bare string.
func TestCSPAdmitsWhatTheBundleLoads(t *testing.T) {
	directives := parseCSP(t, CSPProduction)
	for _, want := range []struct {
		directive string
		source    string
		because   string
	}{
		{"style-src", "'unsafe-inline'", "Svelte writes style attributes on every render, and attribute-level style is not noncable"},
		{"img-src", "http:", "chat markdown renders remote images by design"},
		{"img-src", "https:", "chat markdown renders remote images by design"},
		{"img-src", "data:", "spinner sprite strips and the in-app browser's frame JPEGs"},
		{"img-src", "blob:", "attachment previews and the markdown image host"},
		{"font-src", "data:", "the frontend build inlines small woff2 faces as data URIs"},
		{"font-src", "'self'", "the rest of the Hack Nerd Font slices are served from /assets"},
		{"connect-src", "'self'", "the manifest fetch and the same-origin WebSocket"},
	} {
		if !hasSource(directives[want.directive], want.source) {
			t.Errorf("%s is missing %s (%s)", want.directive, want.source, want.because)
		}
	}
}

// TestCSPDevServerRelaxesOnlyConnectSrc pins the split itself. "Relaxed
// in dev" is a standing invitation to widen script-src too; this fails
// the moment the two policies differ anywhere but connect-src.
func TestCSPDevServerRelaxesOnlyConnectSrc(t *testing.T) {
	production := parseCSP(t, CSPProduction)
	dev := parseCSP(t, CSPDevServer)

	if len(production) != len(dev) {
		t.Fatalf("policies declare different directive sets: production %d, dev %d", len(production), len(dev))
	}
	for directive, productionSources := range production {
		devSources, ok := dev[directive]
		if !ok {
			t.Errorf("dev policy is missing %s", directive)
			continue
		}
		if directive == "connect-src" {
			continue
		}
		if strings.Join(productionSources, " ") != strings.Join(devSources, " ") {
			t.Errorf("%s differs between the policies: production %v, dev %v — only connect-src may", directive, productionSources, devSources)
		}
	}

	// Vite's HMR client falls back to a direct socket on the bundler's
	// own port, which is a different origin from the page when this
	// server is proxying, so 'self' cannot reach it.
	for _, scheme := range []string{"ws:", "wss:"} {
		if !hasSource(dev["connect-src"], scheme) {
			t.Errorf("dev connect-src is missing %s; Vite's direct HMR socket fallback would be refused", scheme)
		}
	}
	if hasSource(production["connect-src"], "ws:") || hasSource(production["connect-src"], "wss:") {
		t.Error("production connect-src carries a bare ws:/wss: scheme; the same-origin socket needs only 'self'")
	}
}

// TestServerPicksItsPolicyFromTheBootMode covers the wiring: the choice
// is made once, in New, and every route then writes the same string.
func TestServerPicksItsPolicyFromTheBootMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		devProxy bool
		want     ContentSecurityPolicy
	}{
		{"embedded bundle", false, CSPProduction},
		{"vite dev proxy", true, CSPDevServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Config{
				Dispatcher:    NewDispatcher(),
				EventBus:      NewEventBus(1),
				Token:         "test-token",
				DevAssetProxy: tc.devProxy,
			})
			if err != nil {
				t.Fatalf("new server: %v", err)
			}
			if srv.csp != tc.want {
				t.Fatalf("csp = %q, want %q", srv.csp, tc.want)
			}
		})
	}
}

// TestEveryHTTPRouteCarriesThePolicy is the completeness half: a route
// that writes its own headers instead of going through
// WriteSecurityHeaders ships with no CSP at all, and nothing else would
// notice. The asset route stands in for the SPA shell, which is the one
// response where the policy actually governs a document.
//
// ONE route is excluded by name: the dev-server preview, "/" on the
// "dev-server preview" listener (previewgateway.go). It writes no policy
// of ours on purpose — the bytes are somebody else's application, the
// posture is theirs, and a Content-Security-Policy this process invented
// would silently break it. What replaces this gate for that route is
// devgateway_contract_test.go, which pins the ticket exchange, the
// cookie flags, the per-request session check, the Host and Origin
// rewrite, and the Location rewrite. The exclusion is safe because the
// preview is a DIFFERENT ORIGIN: a different port is a different
// authority, so nothing served there reaches the SPA's scripts, storage
// or cookies.
func TestEveryHTTPRouteCarriesThePolicy(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.AssetHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><title>t</title>"))
		})
	})

	for _, tc := range []struct {
		name string
		path string
	}{
		{"spa shell", "/"},
		{"bootstrap manifest", "/bootstrap.json"},
		{"page url", PageURLPath},
		{"page url, webview shape", PageURLPath + "?" + pagehost.Param + "=" + pagehost.Webview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+tc.path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer test-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			_, _ = readAllAndClose(resp)
			if got := resp.Header.Get("Content-Security-Policy"); got != string(CSPProduction) {
				t.Errorf("GET %s: Content-Security-Policy = %q, want the production policy", tc.path, got)
			}
			if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("GET %s: X-Frame-Options = %q, want DENY", tc.path, got)
			}
		})
	}
}
