package trackerapi

import (
	"bytes"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewRefusesMisconfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*Options)
		want   string
	}{
		{"empty name", func(o *Options) { o.Name = "" }, "Name"},
		{"empty version", func(o *Options) { o.Version = "" }, "Version"},
		{"no credentials", func(o *Options) { o.Credentials = nil }, "Credentials"},
		{"no dialect", func(o *Options) { o.Dialect = nil }, "Dialect"},
		{"http off loopback", func(o *Options) { o.BaseURL = "http://example.atlassian.net/" }, "not https"},
		{"http on a loopback name", func(o *Options) { o.BaseURL = "http://localhost:8080/" }, "not https"},
		{"another scheme", func(o *Options) { o.BaseURL = "ftp://example.com/" }, "not https"},
		{"relative", func(o *Options) { o.BaseURL = "example.atlassian.net" }, "absolute"},
		{"no host", func(o *Options) { o.BaseURL = "https:///rest" }, "absolute"},
		{"user info", func(o *Options) { o.BaseURL = "https://user:pass@example.atlassian.net/" }, "user info"},
		{"query", func(o *Options) { o.BaseURL = "https://example.atlassian.net/?token=x" }, "query"},
		{"fragment", func(o *Options) { o.BaseURL = "https://example.atlassian.net/#x" }, "fragment"},
		{"not a URL", func(o *Options) { o.BaseURL = "https://exa mple.com/\x7f" }, "not a URL"},
		{"negative rate", func(o *Options) { o.MaxPerSecond = -1 }, "MaxPerSecond"},
		{"NaN rate", func(o *Options) { o.MaxPerSecond = math.NaN() }, "MaxPerSecond"},
		{"infinite rate", func(o *Options) { o.MaxPerSecond = math.Inf(1) }, "MaxPerSecond"},
		{"negative timeout", func(o *Options) { o.ReadTimeout = -time.Second }, "ReadTimeout"},
		{"negative body bound", func(o *Options) { o.MaxBodyBytes = -1 }, "MaxBodyBytes"},
	} {
		opts := testOptions("https://example.atlassian.net/")
		tc.change(&opts)
		c, err := New(opts)
		if err == nil || c != nil {
			t.Errorf("%s: New = %v, %v; want a refusal", tc.name, c, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to name %q", tc.name, err, tc.want)
		}
		if strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "token=") {
			t.Errorf("%s: the refusal repeats a secret: %v", tc.name, err)
		}
	}
}

func TestNewAcceptsHTTPSAndLoopback(t *testing.T) {
	t.Parallel()
	for _, base := range []string{
		"https://example.atlassian.net",
		"https://api.example.com/4",
		"http://127.0.0.1:8080/",
		"http://[::1]:8080/api",
	} {
		c, err := New(testOptions(base))
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		if !strings.HasSuffix(c.base.Path, "/") {
			t.Errorf("%s: base path %q does not end in a slash", base, c.base.Path)
		}
		c.Close()
	}
}

// A request the client refuses is never sent: the server fails the test
// on any request.
func TestRequestRefusals(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	for _, tc := range []struct {
		name string
		r    Request
	}{
		{"unknown method", Request{Method: "OPTIONS", Path: "x"}},
		{"ReadOnly on a PUT", Request{Method: http.MethodPut, Path: "x", ReadOnly: true}},
		{"ReadOnly on a GET", Request{Path: "x", ReadOnly: true}},
		{"negative timeout", Request{Path: "x", Timeout: -time.Second}},
		{"timeout on a transfer", Request{Path: "x", Transfer: true, Timeout: time.Second}},
		{"negative body bound", Request{Path: "x", MaxBodyBytes: -1}},
		{"leading slash", Request{Path: "/rest/api/3/myself"}},
		{"dot segment", Request{Path: "rest/../admin"}},
		{"escaped dot segment", Request{Path: "rest/%2e%2e/admin"}},
		{"absolute without the flag", Request{Path: "https://example.com/x"}},
		{"scheme-relative without the flag", Request{Path: "//example.com/x"}},
		{"flag without an absolute URL", Request{Path: "rest/x", Absolute: true}},
		{"absolute over http", Request{Path: "http://example.com/x", Absolute: true}},
		{"absolute with user info", Request{Path: "https://u:p@example.com/x", Absolute: true}},
		{"not a URL", Request{Path: "rest/\x7f"}},
		{"body on a GET", Request{Path: "x", Body: map[string]int{"a": 1}}},
		{"bytes without a content type", Request{Method: http.MethodPost, Path: "x", Body: []byte("a")}},
		{"reader without a content type", Request{Method: http.MethodPost, Path: "x", Body: strings.NewReader("a")}},
		{"content type without a body", Request{Method: http.MethodPost, Path: "x", ContentType: "text/plain"}},
		{"body that does not encode", Request{Method: http.MethodPost, Path: "x", Body: func() {}}},
		{"Authorization header", Request{Path: "x", Header: http.Header{"Authorization": {"Bearer other"}}}},
		{"cookie header", Request{Path: "x", Header: http.Header{"cookie": {"a=b"}}}},
		{"user agent header", Request{Path: "x", Header: http.Header{"User-Agent": {"other"}}}},
		{"header injection", Request{Path: "x", Header: http.Header{"X-A": {"a\r\nAuthorization: x"}}}},
		{"bad header name", Request{Path: "x", Header: http.Header{"X A": {"a"}}}},
	} {
		_, err := c.Do(t.Context(), tc.r)
		if _, ok := errors.AsType[*NotSentError](err); !ok {
			t.Errorf("%s: Do = %v, want a *NotSentError", tc.name, err)
		}
	}
	for name, call := range map[string]func() error{
		"Stream with no limit": func() error {
			_, err := c.Stream(t.Context(), Request{Path: "x"}, &bytes.Buffer{}, 0)
			return err
		},
		"Stream with MaxBodyBytes": func() error {
			_, err := c.Stream(t.Context(), Request{Path: "x", MaxBodyBytes: 10}, &bytes.Buffer{}, 10)
			return err
		},
		"Stream with no destination": func() error {
			_, err := c.Stream(t.Context(), Request{Path: "x"}, nil, 10)
			return err
		},
		"Stream of a POST": func() error {
			_, err := c.Stream(t.Context(), Request{Method: http.MethodPost, Path: "x", ReadOnly: true}, &bytes.Buffer{}, 10)
			return err
		},
		"Location of a DELETE": func() error {
			_, err := c.Location(t.Context(), Request{Method: http.MethodDelete, Path: "x"})
			return err
		},
	} {
		if _, ok := errors.AsType[*NotSentError](call()); !ok {
			t.Errorf("%s: want a *NotSentError", name)
		}
	}
}

func TestRequestBuildsTheURLAndHeaders(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"POST /4/worklogs/search": func(w http.ResponseWriter, r *http.Request) { writeString(w, `{}`) },
	})
	c, _ := loopbackClient(t, testOptions(tr.srv.URL+"/4"))
	_, err := c.Do(t.Context(), Request{
		Method: "post", Path: "worklogs/search?from=2026-01-01&limit=5", ReadOnly: true,
		Query:  url.Values{"limit": {"50"}, "jql": {"project = WEB & text ~ \"a b\""}},
		Header: http.Header{"X-Request-Mode": {"no-check"}},
		Body:   map[string]string{"q": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := tr.last()
	query := req.URL.Query()
	if query.Get("from") != "2026-01-01" || query.Get("limit") != "50" || query.Get("jql") != `project = WEB & text ~ "a b"` {
		t.Fatalf("query = %v", query)
	}
	for name, want := range map[string]string{
		"User-Agent":     "agent-overflow/test",
		"Content-Type":   "application/json",
		"Accept":         "application/json",
		"X-Request-Mode": "no-check",
		"Authorization":  "Bearer " + testSecret,
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// A request goes through the proxy Options.Proxy names and never to the
// tracker directly: the tracker here fails the test on any request.
func TestProxyCarriesTheRequest(t *testing.T) {
	t.Parallel()
	direct := newTracker(t, route{})
	proxy := newTracker(t, route{"GET /issue": func(w http.ResponseWriter, _ *http.Request) {
		writeString(w, `{}`)
	}})
	proxyURL, err := url.Parse(proxy.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions(direct.url())
	opts.Proxy = http.ProxyURL(proxyURL)
	c, _ := loopbackClient(t, opts)
	if _, err := c.Do(t.Context(), Request{Method: http.MethodGet, Path: "issue"}); err != nil {
		t.Fatal(err)
	}
	if got, want := proxy.last().URL.Host, strings.TrimPrefix(direct.srv.URL, "http://"); got != want {
		t.Errorf("the proxy was asked for host %q, want the tracker's %q", got, want)
	}
}
