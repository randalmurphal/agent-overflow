package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrialKeepsEveryClientRouteOutsideTheRollbackBoundary(t *testing.T) {
	commit, entered := make(chan struct{}), make(chan struct{}, 32)
	var reached atomic.Int32
	fixture := newServerFixtureWith(t, func(cfg *Config) {
		cfg.WaitForActivation = func(ctx context.Context) error {
			entered <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-commit:
				return nil
			}
		}
		cfg.AssetHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.WriteHeader(http.StatusNoContent)
		})
	})
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	type answer struct {
		status int
		err    error
	}
	request := func(ctx context.Context, method, path string) <-chan answer {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://"+fixture.srv.Addr()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		done := make(chan answer, 1)
		go func() {
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- answer{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			done <- answer{status: response.StatusCode}
		}()
		return done
	}
	awaitEntry := func() {
		t.Helper()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("request did not reach the trial gate")
		}
	}
	for _, path := range []string{BootstrapPath, WSPath, ScopedRPCPath, AuthTokenPath, AuthPairPath, AuthTicketPath, ThreadTransferPrefix + "operation", "/"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodOptions} {
			canceled, cancel := context.WithCancel(ctx)
			done := request(canceled, method, path)
			awaitEntry()
			if reached.Load() != 0 {
				t.Fatal("client reached an inner handler during a trial")
			}
			cancel()
			if got := <-done; !errors.Is(got.err, context.Canceled) {
				t.Fatalf("trial answered %s %s before commit: %+v", method, path, got)
			}
		}
	}
	if got := <-request(ctx, http.MethodGet, HealthPath); got.err != nil || got.status != http.StatusOK {
		t.Fatal("trial cannot prove listener health", got)
	}
	bootstrap := request(ctx, http.MethodGet, BootstrapPath)
	awaitEntry()
	close(commit)
	if got := <-bootstrap; got.err != nil || got.status != http.StatusOK {
		t.Fatal("commit did not release bootstrap", got)
	}
	if got := <-request(ctx, http.MethodGet, "/"); got.err != nil || got.status != http.StatusNoContent || reached.Load() != 1 {
		t.Fatal("commit did not release other routes", got)
	}
	fixture.dial(t)
}
