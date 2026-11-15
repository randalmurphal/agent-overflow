//go:build !noremote

package deviceclient

import (
	"context"
	"encoding/json"
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
	for _, step := range steps {
		if step.Outcome != "ok" || step.Duration < 60*time.Millisecond {
			t.Fatalf("slow step = %+v, want outcome=ok duration>=60ms", step)
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
