package claudetui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"net/http/httptest"
)

// recordingDriver is a fake agentTurnDriver that counts begin/end calls and
// captures every reconstructed envelope, so a gateway test can assert which
// inbound requests surface as agent turns and that the SSE was teed.
type recordingDriver struct {
	mu       sync.Mutex
	begins   int
	ends     int
	envelope []json.RawMessage
	rec      *reconstructor
}

func newRecordingDriver() *recordingDriver {
	d := &recordingDriver{}
	d.rec = newReconstructor(func(line json.RawMessage) {
		d.mu.Lock()
		d.envelope = append(d.envelope, line)
		d.mu.Unlock()
	})
	return d
}

func (d *recordingDriver) beginAgentTurn(req *messagesRequest) *agentRequest {
	d.mu.Lock()
	d.begins++
	d.mu.Unlock()
	return d.rec.beginAgentRequest(req)
}

func (d *recordingDriver) endAgentTurn(ar *agentRequest) {
	d.mu.Lock()
	d.ends++
	d.mu.Unlock()
	ar.end()
}

func (d *recordingDriver) snapshot() (begins, ends int, envelopes []json.RawMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.begins, d.ends, append([]json.RawMessage(nil), d.envelope...)
}

// sseUpstream returns a stub upstream that answers any request with a minimal
// SSE body, recording the inbound request body so the test can assert verbatim
// forwarding.
func sseUpstream(t *testing.T, status int, gotBody *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if gotBody != nil {
			*gotBody = b
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestGateway(t *testing.T, upstream string, drive agentTurnDriver) *gateway {
	t.Helper()
	gw, err := newGateway(upstream, drive, func(err error) { t.Logf("gateway error: %v", err) })
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	gw.start()
	t.Cleanup(func() { _ = gw.close() })
	return gw
}

func postGateway(t *testing.T, gw *gateway, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, gw.baseURL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestGatewayReconstructsAgentTurn proves the happy path: a real agent
// /v1/messages POST forwards the body verbatim, returns the upstream SSE, opens
// exactly one turn, and tees the SSE into reconstruction.
func TestGatewayReconstructsAgentTurn(t *testing.T) {
	var forwarded []byte
	up := sseUpstream(t, http.StatusOK, &forwarded)
	drive := newRecordingDriver()
	gw := newTestGateway(t, up.URL, drive)

	resp := postGateway(t, gw, http.MethodPost, "/v1/messages", agentReqBody)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "message_start") {
		t.Errorf("client did not receive the upstream SSE: %q", body)
	}
	if string(forwarded) != agentReqBody {
		t.Errorf("upstream got body %q, want verbatim %q", forwarded, agentReqBody)
	}

	begins, ends := waitDriver(t, drive, 1)
	if begins != 1 || ends != 1 {
		t.Fatalf("begins=%d ends=%d, want 1/1", begins, ends)
	}
	_, _, envelopes := drive.snapshot()
	var sawStreamEvent bool
	for _, e := range envelopes {
		if strings.Contains(string(e), "stream_event") {
			sawStreamEvent = true
		}
	}
	if !sawStreamEvent {
		t.Errorf("SSE was not teed into reconstruction; envelopes: %v", envelopes)
	}
}

// TestGatewayGatesNonAgentTraffic proves reconstruction is set up ONLY for a
// real agent POST /v1/messages with status 200 — preflight bodies, other paths,
// other methods, and non-200 responses are forwarded transparently with no turn.
func TestGatewayGatesNonAgentTraffic(t *testing.T) {
	cases := []struct {
		name   string
		status int
		method string
		path   string
		body   string
	}{
		{"preflight body", http.StatusOK, http.MethodPost, "/v1/messages", `{"model":"x","max_tokens":1,"tools":[{"name":"Bash"}]}`},
		{"auxiliary no-tools", http.StatusOK, http.MethodPost, "/v1/messages", `{"model":"x","max_tokens":4096,"tools":[]}`},
		{"non-message path", http.StatusOK, http.MethodPost, "/v1/complete", agentReqBody},
		{"non-200 upstream", http.StatusTooManyRequests, http.MethodPost, "/v1/messages", agentReqBody},
		{"GET", http.StatusOK, http.MethodGet, "/v1/messages", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := sseUpstream(t, tc.status, nil)
			drive := newRecordingDriver()
			gw := newTestGateway(t, up.URL, drive)

			resp := postGateway(t, gw, tc.method, tc.path, tc.body)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			// Give any (erroneous) async begin a beat to land before asserting zero.
			time.Sleep(20 * time.Millisecond)
			begins, _, _ := drive.snapshot()
			if begins != 0 {
				t.Errorf("%s: opened %d turns, want 0", tc.name, begins)
			}
		})
	}
}

func waitDriver(t *testing.T, d *recordingDriver, wantEnds int) (begins, ends int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		begins, ends, _ = d.snapshot()
		if ends >= wantEnds {
			return begins, ends
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d end(s); begins=%d ends=%d", wantEnds, begins, ends)
	return begins, ends
}
