package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The shell origin is admitted at ONE place, and both the HTTP routes and
// the WebSocket upgrade read that place (OriginAllowed). These cases pin
// the admission, the CORS answer that goes with it, and — the load-bearing
// half — that neither is visible to any other origin.

func TestOriginAllowedAdmitsTheShellAndNothingLikeIt(t *testing.T) {
	admitted := httpRequestForOrigin(t, "desk.example-tailnet.ts.net", ShellOrigin)
	if !OriginAllowed(admitted, nil) {
		t.Error("the shell's fixed page origin was refused")
	}

	// The `.invalid` TLD is what makes one constant safe rather than a
	// pattern. Nothing that merely resembles it is the same authority.
	for _, origin := range []string{
		"http://shell.agent-overflow.invalid",       // wrong scheme
		"https://shell.agent-overflow.invalid.test", // a name somebody can register
		"https://agent-overflow.invalid",
		"https://evil.shell.agent-overflow.invalid",
		"null",
	} {
		req := httpRequestForOrigin(t, "desk.example-tailnet.ts.net", origin)
		if OriginAllowed(req, nil) {
			t.Errorf("origin %q was admitted", origin)
		}
	}
}

func TestShellOriginExtraAdmitsExactlyOneMoreOrigin(t *testing.T) {
	const extra = "http://127.0.0.1:45999"
	t.Setenv(ShellOriginExtraEnv, extra)

	if !OriginAllowed(httpRequestForOrigin(t, "127.0.0.1:7777", extra), nil) {
		t.Error("the harness-declared origin was refused")
	}
	// One more, not a family: a sibling port is a different principal.
	if OriginAllowed(httpRequestForOrigin(t, "127.0.0.1:7777", "http://127.0.0.1:45998"), nil) {
		t.Error("a neighbouring port was admitted by the harness override")
	}
	// The built-in constant still stands beside it.
	if !OriginAllowed(httpRequestForOrigin(t, "127.0.0.1:7777", ShellOrigin), nil) {
		t.Error("the shell origin stopped being admitted while the override was set")
	}
}

// The preflight is what a browser asks before it will send X-AO-Session,
// so a route that answers it wrongly is a route the shell cannot use at
// all — and the failure surfaces on the phone as a request that never
// left.
func TestShellPreflightAnswersTheHeadersTheClientPresents(t *testing.T) {
	handler := withShellCORS(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the wrapped handler ran for a preflight")
	})

	req := httptest.NewRequest(http.MethodOptions, "http://desk.test/auth/pair", nil)
	req.Header.Set("Origin", ShellOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "x-ao-session, content-type")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	h := rec.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != ShellOrigin {
		t.Errorf("allow-origin = %q, want %q", got, ShellOrigin)
	}
	for _, want := range []string{SessionCredentialHeader, DeviceKeyHeader, "Content-Type"} {
		if !containsHeaderName(h.Get("Access-Control-Allow-Headers"), want) {
			t.Errorf("allow-headers %q does not name %q", h.Get("Access-Control-Allow-Headers"), want)
		}
	}
	if !containsHeaderName(h.Get("Access-Control-Allow-Methods"), http.MethodPost) {
		t.Errorf("allow-methods = %q, want it to name POST", h.Get("Access-Control-Allow-Methods"))
	}
	if h.Get("Vary") != "Origin" {
		t.Errorf("vary = %q, want Origin", h.Get("Vary"))
	}
	// The whole reason a wildcard is banned here: no browser is ever
	// invited to attach an ambient credential to these routes.
	if h.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("the credentials flag was written")
	}
}

func TestForeignOriginGetsNoCORSHeadersAtAll(t *testing.T) {
	handler := withShellCORS(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "http://desk.test/bootstrap.json", nil)
	req.Header.Set("Origin", "https://somewhere.else.example")
	rec := httptest.NewRecorder()
	handler(rec, req)

	// The route answers exactly as it always did; the browser is what
	// withholds the body. Nothing here tells a caller which origins this
	// backend knows about.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a foreign origin must not be answered differently", rec.Code)
	}
	for _, header := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Headers",
		"Access-Control-Allow-Methods",
		"Access-Control-Expose-Headers",
	} {
		if rec.Header().Get(header) != "" {
			t.Errorf("%s was written for a foreign origin", header)
		}
	}
	// Vary is stamped regardless: the header set varies by origin even
	// when it varies by becoming empty, and a cache keyed without it
	// would hand one page another's permission.
	if rec.Header().Get("Vary") != "Origin" {
		t.Errorf("vary = %q, want Origin", rec.Header().Get("Vary"))
	}
}

// The claim the middleware has to keep for every client that exists
// today: a same-origin request is byte-identical to what it was before
// this file.
func TestSameOriginAnswerIsUnchanged(t *testing.T) {
	body := "manifest"
	inner := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	bare := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://desk.test/bootstrap.json", nil)
	inner(bare, req)

	wrapped := httptest.NewRecorder()
	// No Origin at all is what a same-origin GET sends.
	withShellCORS(http.MethodGet, inner)(wrapped, httptest.NewRequest(
		http.MethodGet, "http://desk.test/bootstrap.json", nil))

	if wrapped.Body.String() != bare.Body.String() {
		t.Errorf("body = %q, want %q", wrapped.Body.String(), bare.Body.String())
	}
	if wrapped.Code != bare.Code {
		t.Errorf("status = %d, want %d", wrapped.Code, bare.Code)
	}
	for name, values := range wrapped.Header() {
		if name == "Vary" {
			continue
		}
		if got, want := values[0], bare.Header().Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

// The admitted answer exposes the two headers the client actually reads
// off an attachment response. A shell that could not read them would
// mis-render a download it was otherwise allowed to make.
func TestAdmittedResponseExposesWhatTheClientReads(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://desk.test/attachments/t/a", nil)
	req.Header.Set("Origin", ShellOrigin)
	withShellCORS(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(rec, req)

	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	for _, want := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
		if !containsHeaderName(exposed, want) {
			t.Errorf("expose-headers %q does not name %q", exposed, want)
		}
	}
}

// containsHeaderName compares one entry of a comma-separated header list
// case-insensitively, which is how header names compare everywhere else.
func containsHeaderName(list, name string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(part), name) {
			return true
		}
	}
	return false
}
