//go:build !noremote

package deviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/testutil"
)

// slowSteps parses the device client's slow-step lines and fails on any
// that carries a value the line must never hold.
func slowSteps(t *testing.T, logs string, secrets ...string) []testutil.SlowStep {
	t.Helper()
	return testutil.CheckedSlowSteps(t, logs, "deviceclient", secrets...)
}

func lowerSlowStepThreshold(t *testing.T, threshold time.Duration) {
	t.Helper()
	testutil.LowerSlowStepThreshold(t, threshold)
}

func expectOneSlowStep(t *testing.T, steps []testutil.SlowStep, step, outcome string, atLeast time.Duration) {
	t.Helper()
	testutil.ExpectOneSlowStep(t, steps, step, outcome, atLeast)
}

// A redemption that outlasts the threshold names itself and how long it
// took, and carries none of the link token, the verification number, the
// credential or the device label.
func TestSlowStep_PairRedemptionPastThresholdLogsItsDuration(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	be := newBackend(t)
	be.stall.Store(int64(60 * time.Millisecond))
	logs := testutil.CaptureLogOutput(t)

	link := Link{BackendID: "backend-a", BackendName: "Studio Mac", Endpoint: be.URL, Token: "link-token-secret"}
	client, _, err := Pair(context.Background(), t.TempDir(), link, "Robin laptop", "test")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	t.Cleanup(func() { settled(t, client) })

	steps := slowSteps(t, logs.String(), "link-token-secret", "pending-verification", "ao1.", "refresh-", "Robin laptop", "Studio Mac", be.Listener.Addr().String())
	expectOneSlowStep(t, steps, "pair", "ok", 60*time.Millisecond)
}

// A ticket mint past the threshold is one line; the ticket and the
// credential it was minted with stay out of it.
func TestSlowStep_TicketPastThresholdLogsItsDuration(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	be := newBackend(t)
	client, _ := openAgainst(t, be, nil)
	be.stall.Store(int64(60 * time.Millisecond))
	logs := testutil.CaptureLogOutput(t)

	if _, err := client.Ticket(context.Background()); err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	steps := slowSteps(t, logs.String(), "socket-ticket", "ao1.", "refresh-", be.Listener.Addr().String())
	expectOneSlowStep(t, steps, "ticket", "ok", 60*time.Millisecond)
}

// A refused renewal reports the refusal as its outcome, not its reason or
// the refresh secret it presented.
func TestSlowStep_RefusedRenewalReportsTheOutcome(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	be := newBackend(t)
	be.refusal = reasonPendingConfirmation
	client, _ := openAgainst(t, be, nil)
	be.stall.Store(int64(60 * time.Millisecond))
	logs := testutil.CaptureLogOutput(t)

	if err := client.Renew(context.Background()); err == nil {
		t.Fatal("Renew against a pending pairing succeeded")
	}
	steps := slowSteps(t, logs.String(), "refresh-", "ao1.", reasonPendingConfirmation)
	expectOneSlowStep(t, steps, "renew", "refused", 60*time.Millisecond)
}

// A renewal that first asks whether the backend supports recovery logs the
// probe and the exchange as two steps, so either one can be the slow one.
func TestSlowStep_RenewalSupportProbeIsItsOwnStep(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	be := newBackend(t)
	client, _ := openAgainst(t, be, func(s *Session) { s.RefreshRecovery = nil })
	be.stall.Store(int64(60 * time.Millisecond))
	logs := testutil.CaptureLogOutput(t)

	if err := client.Renew(context.Background()); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	steps := slowSteps(t, logs.String(), "refresh-", "ao1.")
	if len(steps) != 2 || steps[0].Step != "renewal_support" || steps[1].Step != "renew" {
		t.Fatalf("slow steps = %+v, want renewal_support then renew", steps)
	}
	// The fixture is a legacy host: its health answers 404, which the probe
	// reports as refused and the renewal reads as no recovery support.
	for i, outcome := range []string{"refused", "ok"} {
		if step := steps[i]; step.Outcome != outcome || step.Duration < 60*time.Millisecond {
			t.Fatalf("slow step = %+v, want outcome=%s duration>=60ms", step, outcome)
		}
	}
}

// A step its caller's deadline ended reports a timeout.
func TestSlowStep_DeadlineReportsATimeout(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	be := newBackend(t)
	client, _ := openAgainst(t, be, nil)
	be.stall.Store(int64(time.Second))
	logs := testutil.CaptureLogOutput(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := client.Ticket(ctx); err == nil {
		t.Fatal("Ticket past its deadline succeeded")
	}
	expectOneSlowStep(t, slowSteps(t, logs.String()), "ticket", "timeout", 60*time.Millisecond)
}

// A profile lock held by another owner past the threshold is its own step,
// so a slow exchange and a slow local wait are told apart.
func TestSlowStep_ProfileLockWaitPastThresholdLogsItsDuration(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	be := newBackend(t)
	client, dir := openAgainst(t, be, nil)
	path, err := sessionPath(dir, "backend-a")
	if err != nil {
		t.Fatalf("sessionPath: %v", err)
	}
	release, err := lockProfile(context.Background(), dir, filepath.Base(path))
	if err != nil {
		t.Fatalf("lockProfile: %v", err)
	}
	logs := testutil.CaptureLogOutput(t)
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = release()
	}()

	if err := client.SetNickname("Desk nickname"); err != nil {
		t.Fatalf("SetNickname: %v", err)
	}
	steps := slowSteps(t, logs.String(), "Desk nickname")
	expectOneSlowStep(t, steps, "profile_lock", "ok", 60*time.Millisecond)
}

// The route probe an own-device enrollment runs before its redemption is a
// step of its own, logged without the route it probed.
func TestSlowStep_PairingRouteProbePastThresholdLogsItsDuration(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	server := httptest.NewTLSServer(routeTestHandler(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{"backendId": "target"})
	}))
	t.Cleanup(server.Close)
	route := computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)}
	logs := testutil.CaptureLogOutput(t)

	if _, err := SelectPairingRoute(context.Background(), "target", []computerroute.Route{route}); err != nil {
		t.Fatalf("SelectPairingRoute: %v", err)
	}
	steps := slowSteps(t, logs.String(), server.Listener.Addr().String(), "target")
	expectOneSlowStep(t, steps, "route_probe", "ok", 60*time.Millisecond)
}

// A slow health probe logs the answer's status as its outcome. The probe
// returns a fully read 4xx or 5xx with a nil error for its caller to judge,
// and the route check and renewal-support check both refuse a non-200.
func TestSlowStep_HealthProbeLogsTheAnswersStatus(t *testing.T) {
	for _, tc := range []struct {
		status  int
		outcome string
	}{
		{http.StatusOK, "ok"},
		{http.StatusNotFound, "refused"},
		{http.StatusServiceUnavailable, "error"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			lowerSlowStepThreshold(t, 20*time.Millisecond)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(60 * time.Millisecond)
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			logs := testutil.CaptureLogOutput(t)

			status, _, _, err := healthProbe(context.Background(), stepRouteProbe, server.Client(), server.URL)
			if err != nil || status != tc.status {
				t.Fatalf("healthProbe = %d, %v; want %d", status, err, tc.status)
			}
			expectOneSlowStep(t, slowSteps(t, logs.String(), server.Listener.Addr().String()), "route_probe", tc.outcome, 60*time.Millisecond)
		})
	}
}

// A health probe its caller cancelled mid-wait logs the cancellation.
func TestSlowStep_CancelledHealthProbeLogsTheCancellation(t *testing.T) {
	lowerSlowStepThreshold(t, 20*time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(server.Close)
	logs := testutil.CaptureLogOutput(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(60*time.Millisecond, cancel)
	if _, _, _, err := healthProbe(ctx, stepRenewalSupport, server.Client(), server.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("healthProbe err = %v, want context.Canceled", err)
	}
	expectOneSlowStep(t, slowSteps(t, logs.String()), "renewal_support", "canceled", 60*time.Millisecond)
}

// Steps inside the threshold write nothing: the line is for the step that
// stalled, not a record of every exchange.
func TestSlowStep_FastStepsLogNothing(t *testing.T) {
	be := newBackend(t)
	logs := testutil.CaptureLogOutput(t)

	link := Link{BackendID: "backend-a", Endpoint: be.URL, Token: "link-token-secret"}
	client, _, err := Pair(context.Background(), t.TempDir(), link, "label", "test")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	t.Cleanup(func() { settled(t, client) })
	if _, err := client.Ticket(context.Background()); err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	if err := client.Renew(context.Background()); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if got := logs.String(); got != "" {
		t.Fatalf("fast steps logged %q, want nothing", got)
	}
}
