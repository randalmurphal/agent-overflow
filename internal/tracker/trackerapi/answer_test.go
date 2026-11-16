package trackerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A 429 closes the gate at its headers: a call made while its body is
// still coming is refused unsent.
func TestRateLimitClosesTheGateBeforeItsBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var others atomic.Int32
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/limited" {
				others.Add(1)
				return
			}
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = http.NewResponseController(w).Flush()
			hang(r)
		}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() {
			_, err := c.Do(ctx, Request{Path: "limited"})
			done <- err
		}()
		synctest.Wait()
		_, err := c.Do(t.Context(), Request{Path: "other"})
		cancel()
		if _, ok := errors.AsType[*RateLimitedError](<-done); !ok {
			t.Error("the limited call did not settle as its limit")
		}
		if _, ok := errors.AsType[*RateLimitedError](err); !ok || others.Load() != 0 {
			t.Fatalf("Do = %v with %d requests sent, want the gate closed", err, others.Load())
		}
	})
}

// A rate limit known from its headers settles the call that drew it: the
// body that stalls behind them neither holds the call past its short read
// budget nor turns the limit into a transient failure that is retried.
func TestRateLimitSettlesAtItsHeaders(t *testing.T) {
	for _, transfer := range []bool{false, true} {
		t.Run(fmt.Sprintf("transfer %v", transfer), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var requests atomic.Int32
				c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					writeString(w, `{"message":`)
					_ = http.NewResponseController(w).Flush()
					hang(r)
				}, nil)
				start := time.Now()
				var err error
				if transfer {
					var dst strings.Builder
					_, err = c.Stream(t.Context(), Request{Path: "a", Transfer: true}, &dst, 100)
				} else {
					_, err = c.Do(t.Context(), Request{Path: "a"})
				}
				rate, ok := errors.AsType[*RateLimitedError](err)
				if !ok || rate.Status != http.StatusTooManyRequests || !rate.Until.Equal(start.Add(time.Minute)) {
					t.Fatalf("%v, want the 429's limit", err)
				}
				if elapsed := time.Since(start); elapsed > limitBodyWait || requests.Load() != 1 {
					t.Fatalf("settled after %s and %d requests, want at once and one", elapsed, requests.Load())
				}
			})
		})
	}
}

// bodyTimeDialect names a limit's end in the body: {"until": <unix>}.
type bodyTimeDialect struct{ PlainDialect }

func (bodyTimeDialect) RateLimited(_ int, _ http.Header, body []byte) (time.Time, bool) {
	var v struct{ Until int64 }
	if json.Unmarshal(body, &v) != nil || v.Until == 0 {
		return time.Time{}, false
	}
	return time.Unix(v.Until, 0), true
}

// A rate limit's body can move its end later, read within the body's
// budget, and never earlier than the headers named.
func TestRateLimitBodyRefinesTheTime(t *testing.T) {
	t.Parallel()
	start := newTestClock().now()
	tr := newTracker(t, route{
		"GET /later": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"until":%d}`, start.Add(time.Minute).Unix())
		},
		"GET /earlier": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"until":%d}`, start.Add(time.Second).Unix())
		},
	})
	opts := testOptions(tr.url())
	opts.Dialect = bodyTimeDialect{}
	for path, want := range map[string]time.Duration{"later": time.Minute, "earlier": 5 * time.Second} {
		c, _ := loopbackClient(t, opts)
		_, err := c.Do(t.Context(), Request{Path: path})
		if rate, ok := errors.AsType[*RateLimitedError](err); !ok || !rate.Until.Equal(start.Add(want)) {
			t.Errorf("%s: %v, want the limit until %s from now", path, err, want)
		}
	}
}

// A transfer's stall and header timers start once the hop is admitted:
// a long wait for the pacing does not stall it.
func TestTransferTimersSkipAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			writeString(w, `{}`)
		}, func(o *Options) { o.MaxPerSecond = 0.01 })
		if _, err := c.Do(t.Context(), Request{Path: "a"}); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "upload", Body: []byte("data"), ContentType: "text/plain", Transfer: true})
		if err != nil || time.Since(start) != 100*time.Second {
			t.Fatalf("Do = %v after %s, want the upload sent after its 100s pacing wait", err, time.Since(start))
		}
	})
}

// A closed gate refuses a background call before any credential work.
func TestClosedGateRefusesBeforeTheCredential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &testSource{secrets: []string{testSecret}, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a call went out through a closed gate")
		}, func(o *Options) { o.Credentials = src })
		c.gate.close(time.Now().Add(30 * time.Second))
		start := time.Now()
		_, err := c.Do(t.Context(), Request{Path: "a"})
		if _, ok := errors.AsType[*RateLimitedError](err); !ok || time.Since(start) != 0 || len(src.entered) != 0 {
			t.Fatalf("Do = %v after %s, want a refusal at once without a credential read", err, time.Since(start))
		}
	})
}

// An interactive call waits at most interactiveGateWait for the gate in
// all, however often the gate is closed again.
func TestInteractiveGateWaitIsOneBudget(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /me": reply(http.StatusOK)})
	c, clock := loopbackClient(t, testOptions(tr.url()))
	c.gate.close(clock.now().Add(9 * time.Second))
	extensions := 0
	c.sleep = func(ctx context.Context, d time.Duration) error {
		err := clock.sleep(ctx, d)
		if extensions < 4 {
			extensions++
			c.gate.close(clock.now().Add(9 * time.Second))
		}
		return err
	}
	_, err := c.Do(WithInteractive(t.Context()), Request{Path: "me"})
	if _, ok := errors.AsType[*RateLimitedError](err); !ok || tr.count("GET /me") != 0 {
		t.Fatalf("Do = %v, want the gate's refusal", err)
	}
	if got := clock.slept(); !slices.Equal(got, seconds(9)) {
		t.Fatalf("waited %v, want one wait within the budget", got)
	}

	tr = newTracker(t, route{"GET /a": reply(http.StatusTooManyRequests, "Retry-After", "6")})
	c, clock = loopbackClient(t, testOptions(tr.url()))
	_, err = c.Do(WithInteractive(t.Context()), Request{Path: "a"})
	if rate, ok := errors.AsType[*RateLimitedError](err); !ok || rate.Status != http.StatusTooManyRequests {
		t.Fatalf("Do = %v, want the second 429", err)
	}
	if tr.count("GET /a") != 2 || !slices.Equal(clock.slept(), seconds(6)) {
		t.Fatalf("%d requests, waits %v; want a second request and no wait past the budget", tr.count("GET /a"), clock.slept())
	}
}

// An error body is read only up to the call's bound.
func TestErrorBodyHonorsTheBound(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /a": func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeString(w, strings.Repeat("x", 1000))
	}})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Path: "a", MaxBodyBytes: 16})
	if status, ok := errors.AsType[*StatusError](err); !ok || len(status.Body) != 16 {
		t.Fatalf("Do = %v, want the 400 with 16 bytes of its body", err)
	}
}

// An error answer cut short is still the status for a write, keeping what
// arrived; for a read it is transient and retried.
func TestErrorBodyCutShort(t *testing.T) {
	t.Parallel()
	cut := func(w http.ResponseWriter, r *http.Request) {
		hijackClose(t, w, "HTTP/1.1 400 Bad Request\r\nContent-Length: 100\r\n\r\n{\"message\":")
	}
	tr := newTracker(t, route{
		"POST /issue": cut,
		"GET /issue":  failing(1, cut, reply(http.StatusOK)),
	})
	c, clock := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
	if status, ok := errors.AsType[*StatusError](err); !ok || status.Status != http.StatusBadRequest || status.Body != `{"message":` || !WriteNotApplied(err) {
		t.Fatalf("write: %v, want the 400 with what arrived", err)
	}
	if _, err := c.Do(t.Context(), Request{Path: "issue"}); err != nil {
		t.Fatalf("read: %v, want a retry", err)
	}
	if tr.count("GET /issue") != 2 || !slices.Equal(clock.slept(), seconds(1)) {
		t.Fatalf("read: %d requests, waits %v", tr.count("GET /issue"), clock.slept())
	}
}

// A call that ends while it reads an answer body ends with its cause, not
// with what the body seemed to say. The in-memory network ends a body
// cleanly when the call is cancelled, as some transports do.
func TestCallEndingDuringTheBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		method string
	}{
		{"success", http.StatusOK, http.MethodGet},
		{"error read", http.StatusBadRequest, http.MethodGet},
		{"error write", http.StatusBadRequest, http.MethodPost},
	} {
		for _, closing := range []bool{false, true} {
			name := fmt.Sprintf("%s, closing %v", tc.name, closing)
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(tc.status)
						writeString(w, `{"message":`)
						_ = http.NewResponseController(w).Flush()
						hang(r)
					}, nil)
					ctx, cancel := context.WithCancel(t.Context())
					done := make(chan error)
					go func() {
						r := Request{Method: tc.method, Path: "a"}
						if tc.method == http.MethodPost {
							r.Body = map[string]string{}
						}
						_, err := c.Do(ctx, r)
						done <- err
					}()
					synctest.Wait()
					want := context.Canceled
					if closing {
						want = errClosed
						c.Close()
					} else {
						cancel()
					}
					err := <-done
					cancel()
					if _, ok := errors.AsType[*StatusError](err); ok || !errors.Is(err, want) || WriteNotApplied(err) {
						t.Errorf("%v, want the call's end", err)
					}
				})
			})
		}
	}
	synctest.Test(t, func(t *testing.T) {
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeString(w, "partial")
			_ = http.NewResponseController(w).Flush()
			hang(r)
		}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() {
			var dst strings.Builder
			_, err := c.Stream(ctx, Request{Path: "a"}, &dst, 100)
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Stream = %v, want the cancellation", err)
		}
	})
}
