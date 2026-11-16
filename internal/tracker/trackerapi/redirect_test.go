package trackerapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A write is never sent on to the URL a 3xx names: the 3xx is the write's
// answer, an error whose outcome is unknown, whatever the target would
// have answered.
func TestWriteRedirectIsAnError(t *testing.T) {
	t.Parallel()
	refused := "http://127.0.0.1:1/receipt?token=secret-q"
	for _, tc := range []struct {
		name        string
		method      string
		status      int
		location    string
		receipt     http.HandlerFunc
		interactive bool
	}{
		{"401 on the target", http.MethodPost, http.StatusSeeOther, "/receipt?token=secret-q", reply(http.StatusUnauthorized), false},
		{"429 on the target", http.MethodPost, http.StatusSeeOther, "/receipt?token=secret-q", reply(http.StatusTooManyRequests, "Retry-After", "0"), true},
		{"400 on the target", http.MethodPost, http.StatusSeeOther, "/receipt?token=secret-q", reply(http.StatusBadRequest), false},
		{"target refuses the dial", http.MethodPost, http.StatusSeeOther, refused, nil, false},
		{"302", http.MethodPost, http.StatusFound, "/receipt?token=secret-q", reply(http.StatusOK), false},
		{"307", http.MethodPost, http.StatusTemporaryRedirect, "/receipt?token=secret-q", reply(http.StatusCreated), false},
		{"308 on a PUT", http.MethodPut, http.StatusPermanentRedirect, "/receipt?token=secret-q", reply(http.StatusOK), false},
		{"301 on a DELETE", http.MethodDelete, http.StatusMovedPermanently, "/receipt?token=secret-q", reply(http.StatusNoContent), false},
		{"304 on a PATCH", http.MethodPatch, http.StatusNotModified, "", nil, false},
	} {
		key := tc.method + " /issue"
		routes := route{key: func(w http.ResponseWriter, r *http.Request) {
			if tc.location != "" {
				w.Header().Set("Location", tc.location)
			}
			w.WriteHeader(tc.status)
		}}
		if tc.receipt != nil {
			routes["GET /receipt"], routes[tc.method+" /receipt"] = tc.receipt, tc.receipt
		}
		tr := newTracker(t, routes)
		opts := testOptions(tr.url())
		opts.Credentials = &testSource{secrets: []string{testSecret, testSecret2}}
		c, _ := loopbackClient(t, opts)
		ctx := t.Context()
		if tc.interactive {
			ctx = WithInteractive(ctx)
		}
		_, err := c.Do(ctx, Request{Method: tc.method, Path: "issue", Body: map[string]string{"summary": "s"}})
		status, ok := errors.AsType[*StatusError](err)
		if !ok || status.Status != tc.status || WriteNotApplied(err) {
			t.Errorf("%s: %v, want a *StatusError of %d with an unknown outcome", tc.name, err, tc.status)
			continue
		}
		if tr.count(key) != 1 || tr.count("GET /receipt")+tr.count(tc.method+" /receipt") != 0 {
			t.Errorf("%s: the write went %d times and the target was asked", tc.name, tr.count(key))
		}
		if strings.Contains(err.Error(), "secret-q") {
			t.Errorf("%s: %q quotes the redirect's query", tc.name, err)
		}
		if tc.location != "" && (!strings.HasSuffix(status.Location, "/receipt") || !strings.Contains(err.Error(), status.Location)) {
			t.Errorf("%s: %q names the target %q, want it redacted", tc.name, err, status.Location)
		}
	}
}

// A redirect's target that is not a URL fails the call with an error that
// quotes none of it.
func TestMalformedRedirectTargetIsRedacted(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /attachment/1": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/file%zz?token=secret-q")
			w.WriteHeader(http.StatusFound)
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Path: "attachment/1"})
	if !errors.Is(err, errRedirect) || strings.Contains(err.Error(), "secret-q") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("Do = %v, want a refused redirect quoting none of its target", err)
	}
	_, err = c.Location(t.Context(), Request{Path: "attachment/1"})
	if err == nil || strings.Contains(err.Error(), "secret-q") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("Location = %v, want an error quoting none of the target", err)
	}
	_, err = c.Do(t.Context(), Request{Path: tr.srv.URL + "/file%zz?token=secret-q", Absolute: true})
	if err == nil || strings.Contains(err.Error(), "secret-q") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("Absolute = %v, want an error quoting none of the URL", err)
	}
}

// A redirect target with user info is refused without quoting it.
func TestRedirectTargetWithUserInfoIsRefused(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /a": func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://user-1:secret-p@media.example.com/b", http.StatusFound)
	}})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Path: "a"})
	if !errors.Is(err, errRedirect) || strings.Contains(err.Error(), "secret-p") {
		t.Fatalf("Do = %v, want the redirect refused", err)
	}
}

// A transfer's redirect hop gets the header timeout to answer, not the
// stall timeout the previous hop's answer started.
func TestTransferHopWaitsForItsAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/a" {
				http.Redirect(w, r, "/b", http.StatusFound)
				return
			}
			time.Sleep(stallTimeout + time.Minute)
			writeString(w, "content")
		}, nil)
		var dst strings.Builder
		if _, err := c.Stream(t.Context(), Request{Path: "a", Transfer: true}, &dst, 100); err != nil || dst.String() != "content" {
			t.Fatalf("Stream = %v, %q", err, dst.String())
		}
	})
}

// Every redirect hop is paced and passes the gate like a first request.
func TestRedirectHopsAreAdmitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var first, second atomic.Int64
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/a":
				first.Store(time.Now().UnixNano())
				http.Redirect(w, r, "/b", http.StatusFound)
			case "/b":
				second.Store(time.Now().UnixNano())
			default:
				t.Errorf("unexpected request %s", r.URL.Path)
			}
		}, func(o *Options) { o.MaxPerSecond = 1 })
		if _, err := c.Do(t.Context(), Request{Path: "a"}); err != nil {
			t.Fatal(err)
		}
		if gap := time.Duration(second.Load() - first.Load()); gap != time.Second {
			t.Fatalf("the redirect hop went %s after the first request, want 1s", gap)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		var c *Client
		var hops atomic.Int32
		c = memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/a":
				c.gate.close(time.Now().Add(30 * time.Second))
				http.Redirect(w, r, "/b", http.StatusFound)
			default:
				hops.Add(1)
			}
		}, nil)
		_, err := c.Do(t.Context(), Request{Path: "a"})
		if _, ok := errors.AsType[*RateLimitedError](err); !ok || hops.Load() != 0 {
			t.Fatalf("Do = %v with %d hops past a closed gate, want the hop refused", err, hops.Load())
		}
		if _, ok := errors.AsType[*NotSentError](err); ok {
			t.Fatalf("Do = %v: the first request was sent", err)
		}
	})
}

// Through a proxy, the credential still goes only to the base origin: a
// redirect to another origin reaches the proxy without it.
func TestProxyKeepsTheCredentialOnTheBaseOrigin(t *testing.T) {
	t.Parallel()
	direct := newTracker(t, route{})
	other := "127.0.0.1:1"
	proxy := newTracker(t, route{
		"GET /attachment/1": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://"+other+"/file/1", http.StatusFound)
		},
		"GET /file/1": func(w http.ResponseWriter, r *http.Request) { writeString(w, "content") },
	})
	proxyURL, err := url.Parse(proxy.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions(direct.url())
	opts.Proxy = http.ProxyURL(proxyURL)
	c, _ := loopbackClient(t, opts)
	resp, err := c.Do(t.Context(), Request{Path: "attachment/1"})
	if err != nil || string(resp.Body) != "content" {
		t.Fatalf("Do = %v", err)
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if len(proxy.reqs) != 2 || proxy.reqs[0].Header.Get("Authorization") == "" {
		t.Fatalf("%d requests through the proxy, want 2 with the first authorized", len(proxy.reqs))
	}
	if hop := proxy.reqs[1]; hop.URL.Host != other || hop.Header.Get("Authorization") != "" {
		t.Fatalf("the hop to %s carried the credential %q", hop.URL.Host, hop.Header.Get("Authorization"))
	}
}
