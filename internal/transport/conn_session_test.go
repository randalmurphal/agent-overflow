package transport

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func dialSessionTest(t *testing.T, f *sessionFixture) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+f.addr+"/ws?token=integration-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	readFrameOfType(t, conn, frameTypeHello)
	return conn
}

func requireSessionClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("session stayed open until the test deadline")
			}
			return
		}
	}
}

func TestSessionRefusalClosesOnNextRPC(t *testing.T) {
	var dead atomic.Bool
	var checks atomic.Int32
	ready := make(chan struct{})
	f := newSessionFixtureWith(t, func(cfg *Config) {
		cfg.SessionRecheckInterval = time.Hour
		sessionAuthorityForTest(cfg).check = func(string) SessionStatus {
			if checks.Add(1) == 3 {
				close(ready)
			}
			if dead.Load() {
				return SessionStatus{Refusal: "expired_session"}
			}
			return SessionStatus{}
		}
	})
	conn := dialSessionTest(t, f)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("watcher did not initialize")
	}
	dead.Store(true)
	result := callRPC(t, conn, "SimpleCall", "not executed")
	if result.Error == nil || result.Error.Reason != "expired_session" {
		t.Fatalf("RPC refusal: %+v", result)
	}
	requireSessionClosed(t, conn)
}

func TestSessionDeadlineRechecksRenewalAndClosesWithoutPolling(t *testing.T) {
	var deadline atomic.Int64
	initial := time.Now().Add(300 * time.Millisecond).Truncate(time.Millisecond)
	var checks atomic.Int32
	armed := make(chan struct{})
	deadline.Store(initial.UnixMilli())
	checkedOld := make(chan struct{}, 1)
	f := newSessionFixtureWith(t, func(cfg *Config) {
		cfg.SessionRecheckInterval = time.Hour
		sessionAuthorityForTest(cfg).check = func(string) SessionStatus {
			until := time.UnixMilli(deadline.Load())
			if checks.Add(1) == 3 {
				close(armed)
			}
			if !time.Now().Before(until) {
				return SessionStatus{Refusal: "expired_session"}
			}
			if time.Now().After(initial) {
				select {
				case checkedOld <- struct{}{}:
				default:
				}
			}
			return SessionStatus{Deadline: until}
		}
	})
	conn := dialSessionTest(t, f)
	select {
	case <-armed:
	case <-time.After(time.Second):
		t.Fatal("watcher did not arm the original deadline")
	}
	deadline.Store(time.Now().Add(750 * time.Millisecond).UnixMilli())
	select {
	case <-checkedOld:
	case <-time.After(time.Second):
		t.Fatal("old deadline was not rechecked")
	}
	if result := callRPC(t, conn, "SimpleCall", "still live"); result.Error != nil {
		t.Fatalf("renewal disconnected session: %+v", result.Error)
	}
	requireSessionClosed(t, conn)
}

type sessionProofStub struct{ attempt func() }

func (s *sessionProofStub) RefuseProof() error {
	s.attempt()
	return AuthRefused("passkey_refused")
}

func TestMethodProofRefusalOnlyClosesAnEndedSession(t *testing.T) {
	for _, endDuringCall := range []bool{false, true} {
		t.Run(fmt.Sprint(endDuringCall), func(t *testing.T) {
			var dead atomic.Bool
			var checks atomic.Int32
			ready := make(chan struct{})
			f := newSessionFixtureWith(t, func(cfg *Config) {
				cfg.SessionRecheckInterval = time.Hour
				sessionAuthorityForTest(cfg).check = func(string) SessionStatus {
					if checks.Add(1) == 3 {
						close(ready)
					}
					if dead.Load() {
						return SessionStatus{Refusal: "revoked_session"}
					}
					return SessionStatus{}
				}
				_, err := cfg.Dispatcher.Register(&sessionProofStub{attempt: func() {
					dead.Store(endDuringCall)
				}}, RegisterOptions{Package: "test", TypeName: "Proof"})
				if err != nil {
					t.Fatal(err)
				}
			})
			conn := dialSessionTest(t, f)
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("watcher did not initialize")
			}
			result := callRPC(t, conn, "RefuseProof")
			if result.Error == nil || result.Error.Reason != "passkey_refused" {
				t.Fatalf("proof refusal: %+v", result)
			}
			if endDuringCall {
				ended := readFrameOfType(t, conn, frameTypeSessionEnded)
				if ended.Error == nil || ended.Error.Reason != "revoked_session" {
					t.Fatalf("session refusal: %+v", ended)
				}
				requireSessionClosed(t, conn)
			} else if next := callRPC(t, conn, "SimpleCall", "still authorized"); next.Error != nil {
				t.Fatalf("proof refusal ended a healthy session: %+v", next.Error)
			}
		})
	}
}
