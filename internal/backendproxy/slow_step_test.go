package backendproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"agent-overflow/internal/testutil"
)

const upstreamStall = 60 * time.Millisecond

// stalledUpstream answers its manifest and its socket after stall, with
// wsStatus refusing the upgrade when it is not zero.
func stalledUpstream(t *testing.T, stall time.Duration, wsStatus int) *Carrier {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			_, _ = w.Write([]byte(`{"backendId":"store-uuid"}`))
			return
		}
		time.Sleep(stall)
		if wsStatus != 0 {
			http.Error(w, "refused", wsStatus)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(upstream.Close)
	carrier, err := New(Config{
		WSURL: "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws",
		Token: "upstream-launch-token",
	})
	if err != nil {
		t.Fatalf("new carrier: %v", err)
	}
	return carrier
}

// carriedDial upgrades through the carrier the way a page does.
func carriedDial(t *testing.T, carrier *Carrier) {
	t.Helper()
	local := httptest.NewServer(http.HandlerFunc(carrier.CarryUpgrade))
	t.Cleanup(local.Close)
	conn, _, err := websocket.Dial(context.Background(), "ws://"+strings.TrimPrefix(local.URL, "http://")+"/ws", nil)
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

func proxySteps(t *testing.T, logs *testutil.SyncLogBuffer) []testutil.SlowStep {
	t.Helper()
	return testutil.CheckedSlowSteps(t, logs.String(), slowStepComponent, "upstream-launch-token", "store-uuid", "127.0.0.1")
}

// A slow manifest fetch logs the upstream's answer as its outcome: the
// fetch returns a 4xx or 5xx with a nil error, and that is not ok.
func TestSlowStep_ManifestFetchPastThresholdLogsItsDuration(t *testing.T) {
	for _, tc := range []struct {
		status  int
		outcome string
	}{
		{http.StatusOK, "ok"},
		{http.StatusUnauthorized, "refused"},
		{http.StatusServiceUnavailable, "error"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
			carrier := manifestUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(upstreamStall)
				http.Error(w, "answer", tc.status)
			})
			logs := testutil.CaptureLogOutput(t)

			if status, _, err := carrier.FetchBootstrap(t.Context()); err != nil || status != tc.status {
				t.Fatalf("FetchBootstrap = %d, %v; want the upstream's %d", status, err, tc.status)
			}
			testutil.ExpectOneSlowStep(t, proxySteps(t, logs), stepManifest, tc.outcome, upstreamStall)
		})
	}
}

// A manifest fetch its caller cancelled mid-wait logs the cancellation.
func TestSlowStep_CancelledManifestFetchLogsTheCancellation(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	carrier := manifestUpstream(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	logs := testutil.CaptureLogOutput(t)

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(upstreamStall, cancel)
	if _, _, err := carrier.FetchBootstrap(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchBootstrap err = %v, want context.Canceled", err)
	}
	testutil.ExpectOneSlowStep(t, proxySteps(t, logs), stepManifest, "canceled", upstreamStall)
}

// manifestUpstream is a carrier whose upstream answers every request with serve.
func manifestUpstream(t *testing.T, serve http.HandlerFunc) *Carrier {
	t.Helper()
	upstream := httptest.NewServer(serve)
	t.Cleanup(upstream.Close)
	carrier, err := New(Config{WSURL: "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws", Token: "upstream-launch-token"})
	if err != nil {
		t.Fatal(err)
	}
	return carrier
}

// The carried upgrade is timed to the upstream's answer, not to the end of
// the socket the proxy then splices.
func TestSlowStep_CarriedUpgradeTimesTheUpstreamHandshake(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	carrier := stalledUpstream(t, upstreamStall, 0)
	logs := testutil.CaptureLogOutput(t)

	carriedDial(t, carrier)
	testutil.ExpectOneSlowStep(t, proxySteps(t, logs), stepUpgrade, "ok", upstreamStall)
}

func TestSlowStep_RefusedCarriedUpgradeReportsTheRefusal(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	carrier := stalledUpstream(t, upstreamStall, http.StatusNotFound)
	logs := testutil.CaptureLogOutput(t)

	carriedDial(t, carrier)
	testutil.ExpectOneSlowStep(t, proxySteps(t, logs), stepUpgrade, "refused", upstreamStall)
}

func TestSlowStep_FastCarryLogsNothing(t *testing.T) {
	carrier := stalledUpstream(t, 0, 0)
	logs := testutil.CaptureLogOutput(t)

	if _, _, err := carrier.FetchBootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	carriedDial(t, carrier)
	if steps := proxySteps(t, logs); len(steps) != 0 {
		t.Fatalf("fast carry logged %+v", steps)
	}
}
