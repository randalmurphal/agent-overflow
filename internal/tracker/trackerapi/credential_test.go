package trackerapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"agent-overflow/internal/secret"
)

func TestCredentialNeverFormatsItsSecret(t *testing.T) {
	t.Parallel()
	basicHeader := base64.StdEncoding.EncodeToString([]byte("user1@example.com:" + testSecret))
	for name, cred := range map[string]*Credential{
		"basic":  Basic("user1@example.com", secret.New([]byte(testSecret))),
		"bearer": Bearer(secret.New([]byte(testSecret))),
		"key":    Key(secret.New([]byte(testSecret))),
	} {
		type holder struct {
			Cred   Credential
			Ptr    *Credential
			Nested struct{ Inner *Credential }
			hidden *Credential
			inline Credential
		}
		h := holder{Cred: *cred, Ptr: cred, hidden: cred, inline: *cred}
		h.Nested.Inner = cred
		var logged bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logged, nil))
		logger.Info("read", "cred", cred, "value", *cred, "holder", h)
		slog.New(slog.NewTextHandler(&logged, nil)).Info("read", "cred", cred, "holder", h)
		outputs := []string{
			fmt.Sprintf("%v %s %+v %#v %q %x %X %d", cred, cred, cred, cred, cred, cred, cred, cred),
			fmt.Sprintf("%v %s %+v %#v %q %x", *cred, *cred, *cred, *cred, *cred, *cred),
			// Through any, so vet lets the verbs that reach fmt's bad-verb
			// path through: that path prints nested fields.
			fmt.Sprintf("%v %+v %#v %s %q %x %d", any(h), any(h), any(h), any(h), any(h), any(h), any(h)),
			fmt.Sprint(cred, *cred, h),
			cred.String(),
			cred.GoString(),
			logged.String(),
		}
		for _, value := range []any{cred, *cred, h} {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("%s: json.Marshal(%T): %v", name, value, err)
			}
			outputs = append(outputs, string(encoded))
		}
		text, err := cred.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, string(text))
		for _, out := range outputs {
			if strings.Contains(out, testSecret) || strings.Contains(out, basicHeader) {
				t.Fatalf("%s: a formatter printed the secret: %q", name, out)
			}
			// JSON escapes the angle brackets of secret.Redacted.
			if !strings.Contains(out, "redacted") {
				t.Fatalf("%s: output %q does not say %s", name, out, secret.Redacted)
			}
		}
	}
}

func TestCredentialSchemesOnTheWire(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cred *Credential
		want string
	}{
		{Basic("user1@example.com", secret.New([]byte(testSecret))), "Basic " + base64.StdEncoding.EncodeToString([]byte("user1@example.com:"+testSecret))},
		{Bearer(secret.New([]byte(testSecret))), "Bearer " + testSecret},
		{Key(secret.New([]byte(testSecret))), testSecret},
	} {
		tr := newTracker(t, route{"GET /me": func(w http.ResponseWriter, r *http.Request) { writeString(w, `{}`) }})
		opts := testOptions(tr.url())
		opts.Credentials = StaticCredentials(tc.cred)
		c, _ := loopbackClient(t, opts)
		if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil {
			t.Fatal(err)
		}
		if got := tr.last().Header.Get("Authorization"); got != tc.want {
			t.Fatalf("Authorization = %q, want %q", got, tc.want)
		}
	}
}

// A credential no request can carry is refused before anything is sent,
// zeroed, and named without its secret.
func TestUnusableCredentialIsNotSent(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{})
	for name, build := range map[string]func() *Credential{
		"nil":                func() *Credential { return nil },
		"empty secret":       func() *Credential { return Bearer(secret.New(nil)) },
		"zero value":         func() *Credential { return &Credential{secret: secret.New([]byte(testSecret))} },
		"basic without user": func() *Credential { return Basic("", secret.New([]byte(testSecret))) },
		"basic user colon":   func() *Credential { return Basic("a:b", secret.New([]byte(testSecret))) },
		"key with newline":   func() *Credential { return Key(secret.New([]byte(testSecret + "\n"))) },
		"bearer with NUL":    func() *Credential { return Bearer(secret.New([]byte(testSecret + "\x00"))) },
	} {
		cred := build()
		var backing []byte
		if cred != nil {
			backing = cred.secret.Reveal()
		}
		opts := testOptions(tr.url())
		opts.Credentials = sourceFunc(func(context.Context) (*Credential, error) { return cred, nil })
		c, _ := loopbackClient(t, opts)
		_, err := c.Do(t.Context(), Request{Path: "me"})
		if _, ok := errors.AsType[*NotSentError](err); !ok || !strings.Contains(err.Error(), "credential") {
			t.Errorf("%s: Do = %v, want a *NotSentError about the credential", name, err)
		}
		if err != nil && strings.Contains(err.Error(), testSecret) {
			t.Errorf("%s: the error repeats the secret: %v", name, err)
		}
		if len(backing) > 0 && !isZero(backing) {
			t.Errorf("%s: the refused credential was not zeroed", name)
		}
	}
}

type sourceFunc func(context.Context) (*Credential, error)

func (f sourceFunc) Credential(ctx context.Context) (*Credential, error) { return f(ctx) }

// A source that fails is reported as never sent, and the next call reads
// it again.
func TestCredentialReadFailure(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /me": func(w http.ResponseWriter, r *http.Request) { writeString(w, `{}`) }})
	src := &testSource{secrets: []string{testSecret}, err: errors.New("key file missing")}
	opts := testOptions(tr.url())
	opts.Credentials = src
	c, _ := loopbackClient(t, opts)
	_, err := c.Do(t.Context(), Request{Path: "me"})
	if _, ok := errors.AsType[*NotSentError](err); !ok || !strings.Contains(err.Error(), "key file missing") || !WriteNotApplied(err) {
		t.Fatalf("Do = %v, want a *NotSentError carrying the source's error", err)
	}
	src.setErr(nil)
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil || src.readCount() != 2 {
		t.Fatalf("after the source recovered: %v, %d reads", err, src.readCount())
	}
}

func TestCredentialReadOnceUnderConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &testSource{secrets: []string{testSecret}, gate: make(chan struct{})}
		var answered atomic.Int32
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testSecret {
				t.Errorf("a request went without the credential")
			}
			answered.Add(1)
			writeString(w, `{}`)
		}, func(o *Options) { o.Credentials = src })
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				_, err := c.Do(t.Context(), Request{Path: "rest/api/3/myself"})
				errs <- err
			})
		}
		synctest.Wait() // every call waits for the one read
		close(src.gate)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if src.readCount() != 1 || answered.Load() != 8 {
			t.Fatalf("%d reads for %d answers, want one read", src.readCount(), answered.Load())
		}
	})
}

// A 401 reads the credential again once, sends the call again with the
// new one and zeroes the old. A credential the re-read leaves as it was,
// or a second 401, is an *AuthError.
func TestRefusedCredentialIsReadAgainOnce(t *testing.T) {
	t.Parallel()
	var accepted atomic.Value
	accepted.Store(testSecret2)
	tr := newTracker(t, route{
		"GET /me": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+accepted.Load().(string) {
				http.Error(w, `{"message":"bad credentials"}`, http.StatusUnauthorized)
				return
			}
			writeString(w, `{}`)
		},
	})
	src := &testSource{secrets: []string{testSecret, testSecret2, testSecret2, "fake-token-3"}}
	opts := testOptions(tr.url())
	opts.Credentials = src
	opts.Dialect = testDialect{}
	c, _ := loopbackClient(t, opts)

	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil {
		t.Fatalf("after one 401: %v", err)
	}
	if src.readCount() != 2 || tr.count("GET /me") != 2 {
		t.Fatalf("%d reads, %d requests; want 2 and 2", src.readCount(), tr.count("GET /me"))
	}
	if !src.zeroed(0) {
		t.Fatal("the replaced credential was not zeroed")
	}

	// The tracker now refuses the new credential too, and a re-read
	// returns the same bytes: refused at once, without sending again.
	accepted.Store("fake-token-9")
	_, err := c.Do(t.Context(), Request{Path: "me"})
	auth, ok := errors.AsType[*AuthError](err)
	if !ok || auth.Status != http.StatusUnauthorized || auth.Message != "bad credentials" || !WriteNotApplied(err) {
		t.Fatalf("unchanged re-read: %v, want an *AuthError", err)
	}
	if src.readCount() != 3 || tr.count("GET /me") != 3 {
		t.Fatalf("unchanged re-read: %d reads, %d requests; want 3 and 3", src.readCount(), tr.count("GET /me"))
	}
	if !src.zeroed(2) || src.zeroed(1) {
		t.Fatal("want the duplicate the re-read returned zeroed and the held one kept")
	}

	// A re-read that changes the credential sends once more; a second 401
	// ends the call.
	_, err = c.Do(t.Context(), Request{Path: "me"})
	if _, ok := errors.AsType[*AuthError](err); !ok {
		t.Fatalf("second 401: %v, want an *AuthError", err)
	}
	if src.readCount() != 4 || tr.count("GET /me") != 5 {
		t.Fatalf("second 401: %d reads, %d requests; want 4 and 5", src.readCount(), tr.count("GET /me"))
	}
}

// Concurrent calls refused for the same credential share one re-read.
func TestConcurrentRefusalsShareOneReread(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &testSource{secrets: []string{testSecret, testSecret2, "fake-token-3"}}
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testSecret2 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeString(w, `{}`)
		}, func(o *Options) { o.Credentials = src })
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if src.readCount() != 2 {
			t.Fatalf("%d reads, want the first and one shared re-read", src.readCount())
		}
	})
}

// A re-read that fails after a 401 ends the call with the refusal, which
// carries the failure, and still proves a write was not applied.
func TestRereadFailureAfterRefusal(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"POST /issue": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }})
	src := &testSource{secrets: []string{testSecret}}
	opts := testOptions(tr.url())
	opts.Credentials = sourceFunc(func(ctx context.Context) (*Credential, error) {
		if src.readCount() == 1 {
			return nil, errors.New("key file unreadable")
		}
		return src.Credential(ctx)
	})
	c, _ := loopbackClient(t, opts)
	_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{"summary": "s"}})
	auth, ok := errors.AsType[*AuthError](err)
	if !ok || auth.Err == nil || !strings.Contains(err.Error(), "key file unreadable") || !WriteNotApplied(err) {
		t.Fatalf("Do = %v, want an *AuthError carrying the re-read failure", err)
	}
	if tr.count("POST /issue") != 1 {
		t.Fatalf("the write was sent %d times", tr.count("POST /issue"))
	}
}

// A dialect's refused login on a 4xx is handled as a 401. On a 2xx it
// ends a write at once, unconfirmed, since the tracker may have run it.
func TestDialectRefusedLogin(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /search": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Login-Reason", "AUTHENTICATED_FAILED")
			writeString(w, `{"issues":[]}`)
		},
		"POST /comment": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Login-Reason", "AUTHENTICATED_FAILED")
			w.WriteHeader(http.StatusCreated)
		},
		"POST /denied": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Login-Reason", "AUTHENTICATION_DENIED")
			w.WriteHeader(http.StatusForbidden)
		},
	})
	src := &testSource{secrets: []string{testSecret, testSecret2, "fake-token-3", "fake-token-4"}}
	opts := testOptions(tr.url())
	opts.Credentials = src
	opts.Dialect = testDialect{}
	c, _ := loopbackClient(t, opts)

	_, err := c.Do(t.Context(), Request{Path: "search"})
	if auth, ok := errors.AsType[*AuthError](err); !ok || auth.Status != http.StatusOK {
		t.Fatalf("read: %v, want an *AuthError on the 200", err)
	}
	if tr.count("GET /search") != 2 {
		t.Fatalf("read sent %d times, want once more after the re-read", tr.count("GET /search"))
	}
	_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "comment", Body: map[string]string{"body": "b"}})
	if _, ok := errors.AsType[*AuthError](err); !ok || WriteNotApplied(err) {
		t.Fatalf("write on a 201: %v, want an unconfirmed *AuthError", err)
	}
	if tr.count("POST /comment") != 1 || src.readCount() != 2 {
		t.Fatalf("write sent %d times after %d reads; want once, without a re-read", tr.count("POST /comment"), src.readCount())
	}
	_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "denied", Body: map[string]string{"body": "b"}})
	if _, ok := errors.AsType[*AuthError](err); !ok || !WriteNotApplied(err) {
		t.Fatalf("write on a 403: %v, want a refusal that proves it not applied", err)
	}
	if tr.count("POST /denied") != 2 {
		t.Fatalf("write on a 403 sent %d times, want once more with the new credential", tr.count("POST /denied"))
	}
}

// ForgetCredential zeroes the credential and the next call reads again.
func TestForgetCredential(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /me": func(w http.ResponseWriter, r *http.Request) { writeString(w, `{}`) }})
	src := &testSource{secrets: []string{testSecret, testSecret2}}
	opts := testOptions(tr.url())
	opts.Credentials = src
	c, _ := loopbackClient(t, opts)
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil {
		t.Fatal(err)
	}
	c.ForgetCredential()
	if !src.zeroed(0) {
		t.Fatal("ForgetCredential left the secret behind")
	}
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil || src.readCount() != 2 {
		t.Fatalf("after ForgetCredential: %v, %d reads", err, src.readCount())
	}
	if got := tr.last().Header.Get("Authorization"); got != "Bearer "+testSecret2 {
		t.Fatalf("sent %q after the re-read", got)
	}
}

// ForgetCredential while a read is in flight discards what that read
// returns; the waiting call reads again.
func TestForgetDuringReadDiscardsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &testSource{secrets: []string{testSecret, testSecret2}, gate: make(chan struct{})}
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer "+testSecret2 {
				t.Errorf("sent %q, want the credential read after ForgetCredential", got)
			}
		}, func(o *Options) { o.Credentials = src })
		done := make(chan error)
		go func() {
			_, err := c.Do(t.Context(), Request{Path: "me"})
			done <- err
		}()
		synctest.Wait()
		c.ForgetCredential()
		close(src.gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !src.zeroed(0) {
			t.Fatal("the discarded credential was not zeroed")
		}
	})
}
