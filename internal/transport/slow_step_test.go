package transport

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/testutil"
)

const routeStall = 60 * time.Millisecond

// routeSteps parses the transport's slow-step lines and fails on one that
// carries a value it must never hold.
func routeSteps(t *testing.T, logs *testutil.SyncLogBuffer, secrets ...string) []testutil.SlowStep {
	t.Helper()
	return testutil.CheckedSlowSteps(t, logs.String(), slowStepComponent, secrets...)
}

// awaitRouteSteps waits for want slow-step lines: a socket's line is
// written after the hello the client has already read.
func awaitRouteSteps(t *testing.T, logs *testutil.SyncLogBuffer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(routeSteps(t, logs)) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// A redemption the endpoint takes long to decide is timed from the request
// to the answer written, and the line names none of the link token, the
// credential, the verification number or the label.
func TestSlowStep_PairRouteTimesTheAnswer(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	auth := &stubAuth{stall: routeStall, grant: TokenGrant{
		SessionID: "sess-1", Credential: "ao1.credential", RefreshSecret: "refresh-1",
		AwaitingConfirmation: true, VerificationNumber: "004217", PairingID: "pair-1",
	}}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
	logs := testutil.CaptureLogOutput(t)

	resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{Token: "pairing-token", Label: "A Phone"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	steps := routeSteps(t, logs, "pairing-token", "ao1.", "refresh-1", "004217", "A Phone", "sess-1", "pair-1")
	testutil.ExpectOneSlowStep(t, steps, stepPairRoute, "ok", routeStall)
}

// A refused renewal is timed and reported as a refusal, without its reason
// or the refresh secret.
func TestSlowStep_RenewRouteReportsARefusal(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	auth := &stubAuth{stall: routeStall, reason: "refresh_reused"}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
	logs := testutil.CaptureLogOutput(t)

	resp := postJSON(t, f.srv.Addr(), AuthTokenPath, map[string]string{"refreshSecret": "secret-1"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	steps := routeSteps(t, logs, "secret-1", "refresh_reused")
	testutil.ExpectOneSlowStep(t, steps, stepRenewRoute, "refused", routeStall)
}

// stalledSessions takes routeStall to resolve the session a request names.
type stalledSessions struct{ SessionAuthority }

func (s stalledSessions) Resolve(r *http.Request) (string, bool) {
	time.Sleep(routeStall)
	return s.SessionAuthority.Resolve(r)
}

func TestSlowStep_TicketRouteTimesTheMint(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	f := newSessionFixtureWith(t, func(cfg *Config) { cfg.Sessions = stalledSessions{cfg.Sessions} })
	logs := testutil.CaptureLogOutput(t)

	ticket := mintTicket(t, f)
	steps := routeSteps(t, logs, ticket, "sess-1")
	testutil.ExpectOneSlowStep(t, steps, stepTicketRoute, "ok", routeStall)
}

// slowIdentity answers the backend identity after routeStall. The manifest,
// the health probe and the socket's hello all read it.
func slowIdentity() (string, string) {
	time.Sleep(routeStall)
	return "backend-a", "replica-1"
}

func TestSlowStep_BootstrapAndHealthRoutesTimeTheAnswer(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.BackendIdentity = slowIdentity })
	logs := testutil.CaptureLogOutput(t)

	for _, path := range []string{BootstrapPath, HealthPath} {
		req, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
	steps := routeSteps(t, logs, "test-token", "backend-a", "replica-1")
	if len(steps) != 2 || steps[0].Step != stepBootstrap || steps[1].Step != stepHealth {
		t.Fatalf("slow steps = %+v, want bootstrap then health", steps)
	}
	for _, step := range steps {
		if step.Outcome != "ok" || step.Duration < routeStall {
			t.Fatalf("slow step = %+v, want outcome=ok duration>=%s", step, routeStall)
		}
	}
}

// An upgrade is timed from the request to the hello written, the frame a
// client is connected on.
func TestSlowStep_SocketAcceptTimesUpgradeThroughHello(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.BackendIdentity = slowIdentity })
	logs := testutil.CaptureLogOutput(t)

	conn := f.dial(t)
	if _, _, err := conn.Read(context.Background()); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	awaitRouteSteps(t, logs, 1)
	testutil.ExpectOneSlowStep(t, routeSteps(t, logs, "test-token", "backend-a"), stepWSAccept, "ok", routeStall)
}

// A refused upgrade is timed too, so a slow session lookup that ends in a
// refusal is not silent.
func TestSlowStep_SocketRefusalIsTimed(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	f := newSessionFixtureWith(t, func(cfg *Config) { cfg.Sessions = stalledSessions{cfg.Sessions} })
	f.setRefuse(true)
	logs := testutil.CaptureLogOutput(t)

	if _, _, err := websocket.Dial(context.Background(), "ws://"+f.addr+"/ws?token=integration-token", nil); err == nil {
		t.Fatal("a refused session opened a socket")
	}
	testutil.ExpectOneSlowStep(t, routeSteps(t, logs), stepWSAccept, "refused", routeStall)
}

// Steps inside the threshold write no slow-step line.
func TestSlowStep_FastRoutesLogNothing(t *testing.T) {
	auth := &stubAuth{renewGood: true, grant: TokenGrant{SessionID: "sess-1", Credential: "ao1.next"}}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
	logs := testutil.CaptureLogOutput(t)

	_ = postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{Token: "pairing-token"}, nil)
	_ = postJSON(t, f.srv.Addr(), AuthTokenPath, map[string]string{"refreshSecret": "secret-1"}, nil)
	conn := f.dial(t)
	if _, _, err := conn.Read(context.Background()); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if steps := routeSteps(t, logs); len(steps) != 0 {
		t.Fatalf("fast routes logged %+v", steps)
	}
}
