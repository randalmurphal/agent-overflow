package attachedbackends

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

const peerStall = 60 * time.Millisecond

// stalledPeer is a paired computer serving the own-device surface whose
// upgrade, hello and call answers each take the time a test sets.
type stalledPeer struct {
	*httptest.Server
	id                string
	dial, hello, call time.Duration
	refuseUpgrade     bool
}

func newStalledPeer(t *testing.T, p *stalledPeer) (*Manager, *stalledPeer) {
	t.Helper()
	remotetest.Require(t)
	p.id = uuid.NewString()
	p.Server = httptest.NewServer(http.HandlerFunc(p.route))
	t.Cleanup(p.Close)
	m, dir := newManager(t)
	seed(t, dir, deviceclient.Session{BackendID: p.id, Endpoint: p.URL, SessionID: "session", Credential: "credential", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()})
	return m, p
}

func (p *stalledPeer) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/auth/ticket":
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "socket-ticket"})
		return
	case "/ws":
	default:
		http.NotFound(w, r)
		return
	}
	time.Sleep(p.dial)
	if p.refuseUpgrade {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := context.Background()
	time.Sleep(p.hello)
	if wsjson.Write(ctx, conn, map[string]any{"type": "hello", "backendId": p.id, "protocolVersion": transport.ProtocolVersion, "capabilities": []string{owndevices.Capability}}) != nil {
		return
	}
	for {
		var frame transport.ClientFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return
		}
		time.Sleep(p.call)
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "rpc", "id": frame.ID, "result": map[string]any{}})
	}
}

func listOwnDevices(t *testing.T, m *Manager, id string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var list owndevices.List
	return m.CallOwnDevice(ctx, id, "ListOwnDevices", &list)
}

func peerSteps(t *testing.T, logs *testutil.SyncLogBuffer, p *stalledPeer) []testutil.SlowStep {
	t.Helper()
	return testutil.CheckedSlowSteps(t, logs.String(), slowStepComponent, "socket-ticket", "credential", p.id, p.Listener.Addr().String())
}

// Each step of a call to a paired computer that runs past the threshold logs
// its duration and outcome once; a call's step names the method.
func TestSlowStep_PeerStepPastThresholdLogsItsDuration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		peer    stalledPeer
		refused bool
		step    string
		outcome string
	}{
		{"dial", stalledPeer{dial: peerStall}, false, stepPeerDial, "ok"},
		{"refused dial", stalledPeer{dial: peerStall, refuseUpgrade: true}, true, stepPeerDial, "refused"},
		{"hello", stalledPeer{hello: peerStall}, false, stepPeerHello, "ok"},
		{"call", stalledPeer{call: peerStall}, false, stepPeerCall + ".ListOwnDevices", "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.LowerSlowStepThreshold(t, 20*time.Millisecond)
			m, p := newStalledPeer(t, &tc.peer)
			logs := testutil.CaptureLogOutput(t)

			if err := listOwnDevices(t, m, p.id); (err != nil) != tc.refused {
				t.Fatalf("CallOwnDevice = %v, want refused %t", err, tc.refused)
			}
			testutil.ExpectOneSlowStep(t, peerSteps(t, logs, p), tc.step, tc.outcome, peerStall)
		})
	}
}

func TestSlowStep_FastPeerStepsLogNothing(t *testing.T) {
	m, p := newStalledPeer(t, &stalledPeer{})
	logs := testutil.CaptureLogOutput(t)

	if err := listOwnDevices(t, m, p.id); err != nil {
		t.Fatalf("CallOwnDevice: %v", err)
	}
	if steps := peerSteps(t, logs, p); len(steps) != 0 {
		t.Fatalf("fast peer steps logged %+v", steps)
	}
}
