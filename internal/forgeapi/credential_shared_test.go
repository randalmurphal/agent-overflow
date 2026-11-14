package forgeapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// heldSource blocks its first read until release closes and reports
// whether that read's context was cancelled, the way ExecProcess kills a
// handoff command whose context ends.
type heldSource struct {
	started chan struct{}
	release chan struct{}
	killed  chan error
	testSource
}

func (s *heldSource) Token(ctx context.Context, forge, host string) ([]byte, SourceInfo, error) {
	if s.readCount() == 0 {
		close(s.started)
		select {
		case <-s.release:
			s.killed <- nil
		case <-ctx.Done():
			s.killed <- ctx.Err()
			return nil, SourceInfo{}, ctx.Err()
		}
	}
	return s.testSource.Token(ctx, forge, host)
}

func TestCallerLeavingDuringATokenReadEndsOnlyItsWait(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testSecret {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"n":1}`)
	}))
	defer srv.Close()
	src := &heldSource{
		started: make(chan struct{}), release: make(chan struct{}), killed: make(chan error, 1),
		testSource: testSource{tokens: []string{testSecret}},
	}
	s, _ := liveService(t, src, srv)
	gh := s.GitHub("github.com")

	callerA, leaveA := context.WithCancel(t.Context())
	aDone := make(chan error, 1)
	go func() {
		_, err := gh.Do(callerA, Request{Path: "user"})
		aDone <- err
	}()
	<-src.started
	bDone := make(chan error, 1)
	go func() {
		_, err := gh.Do(t.Context(), Request{Path: "user"})
		bDone <- err
	}()

	leaveA()
	if err := <-aDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller A after leaving: %v, want context.Canceled", err)
	}
	select {
	case err := <-src.killed:
		t.Fatalf("caller A leaving ended the shared token read: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(src.release)
	if err := <-src.killed; err != nil {
		t.Fatalf("token read: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("caller B, waiting on the same read: %v", err)
	}
	if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil {
		t.Fatalf("a later request saw caller A's cancellation: %v", err)
	}
	if n := src.readCount(); n != 1 {
		t.Fatalf("token reads = %d, want one shared read", n)
	}
}

func TestCloseEndsAndJoinsATokenRead(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	src := &heldSource{
		started: make(chan struct{}), release: make(chan struct{}), killed: make(chan error, 1),
		testSource: testSource{tokens: []string{testSecret}},
	}
	s, _ := liveService(t, src, srv)
	done := make(chan error, 1)
	go func() {
		_, err := s.GitHub("github.com").Do(t.Context(), Request{Path: "user"})
		done <- err
	}()
	<-src.started
	s.Close()
	select {
	case err := <-src.killed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("token read after Close: %v, want its context cancelled", err)
		}
	default:
		t.Fatal("Close returned before the token read it ended")
	}
	if err := <-done; !errors.Is(err, errClosed) {
		t.Fatalf("request whose read Close ended: %v, want errClosed", err)
	}
}
