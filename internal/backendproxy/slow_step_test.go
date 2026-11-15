package backendproxy

import (
	"context"
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

func TestSlowStep_ManifestFetchPastThresholdLogsItsDuration(t *testing.T) {
	testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(upstreamStall)
		http.Error(w, "gone", http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	carrier, err := New(Config{WSURL: "ws://" + strings.TrimPrefix(upstream.URL, "http://") + "/ws", Token: "upstream-launch-token"})
	if err != nil {
		t.Fatal(err)
	}
	logs := testutil.CaptureLogOutput(t)

	if status, _, err := carrier.FetchBootstrap(t.Context()); err != nil || status != http.StatusUnauthorized {
		t.Fatalf("FetchBootstrap = %d, %v; want the upstream's 401", status, err)
	}
	testutil.ExpectOneSlowStep(t, proxySteps(t, logs), stepManifest, "refused", upstreamStall)
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
