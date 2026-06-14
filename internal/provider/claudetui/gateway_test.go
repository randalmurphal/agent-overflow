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
	mu            sync.Mutex
	begins        int
	subBegins     int
	captureBegins int
	ends          int
	envelope      []json.RawMessage
	rec           *reconstructor
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

func (d *recordingDriver) beginSubagentTurn(req *messagesRequest, agentID string) *agentRequest {
	d.mu.Lock()
	d.subBegins++
	d.mu.Unlock()
	parent := d.rec.resolveSubagentParent(agentID, firstUserText(req.Messages))
	if parent == "" {
		return nil
	}
	return d.rec.beginSubagentRequest(parent)
}

func (d *recordingDriver) beginCompactionCapture(req *messagesRequest) *agentRequest {
	ar := d.rec.beginCompactionCapture()
	if ar != nil {
		d.mu.Lock()
		d.captureBegins++
		d.mu.Unlock()
	}
	return ar
}

func (d *recordingDriver) endAgentTurn(ar *agentRequest) {
	d.mu.Lock()
	d.ends++
	d.mu.Unlock()
	ar.end()
}

func (d *recordingDriver) subagentBegins() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.subBegins
}

func (d *recordingDriver) captureBeginsCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.captureBegins
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

func newTestGateway(t *testing.T, upstream string, drive agentTurnDriver, onClassify func(requestClass, int, []byte)) *gateway {
	t.Helper()
	gw, err := newGateway(upstream, drive, func(err error) { t.Logf("gateway error: %v", err) }, onClassify)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	gw.start()
	t.Cleanup(func() { _ = gw.close() })
	return gw
}

func postGateway(t *testing.T, gw *gateway, method, path, body string) *http.Response {
	t.Helper()
	return postGatewayWithHeaders(t, gw, method, path, body, nil)
}

func postGatewayWithHeaders(t *testing.T, gw *gateway, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, gw.baseURL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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
	gw := newTestGateway(t, up.URL, drive, nil)

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

// TestGatewayCapsOversizedRequestBody proves the memory-pressure backstop: a
// request body larger than maxBodyBytes is refused with 413 and never read
// unbounded into memory, classified, or forwarded upstream. The cap fails loud
// because the body is proxied verbatim — a silent truncation would corrupt the
// upstream request. A small cap keeps the test body tiny so the client finishes
// writing before the server trips and replies, and so a valid agent body (which
// WOULD forward and open a turn without the cap) discriminates the fix.
func TestGatewayCapsOversizedRequestBody(t *testing.T) {
	var forwarded []byte
	up := sseUpstream(t, http.StatusOK, &forwarded)
	drive := newRecordingDriver()
	gw, err := newGateway(up.URL, drive, func(err error) { t.Logf("gateway error: %v", err) }, nil)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	// Set before start(): read-only on the serving goroutine, so no race.
	gw.maxBodyBytes = 16
	gw.start()
	t.Cleanup(func() { _ = gw.close() })

	// agentReqBody is a valid agent request and far exceeds the 16-byte cap.
	resp := postGateway(t, gw, http.MethodPost, "/v1/messages", agentReqBody)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (over-limit body must be refused)", resp.StatusCode)
	}
	// Refused before the upstream request is built — give any (erroneous) async
	// begin a beat to land before asserting zero turns and zero forwarding.
	time.Sleep(20 * time.Millisecond)
	if begins, _, _ := drive.snapshot(); begins != 0 {
		t.Errorf("opened %d turns, want 0 (over-limit body must not reconstruct)", begins)
	}
	if forwarded != nil {
		t.Errorf("over-limit body forwarded upstream (%d bytes); want refused before forward", len(forwarded))
	}
}

// TestGatewayRoutesArmedSummarizerToCapture proves the compaction routing: when
// a compaction is armed (PreCompact seen), the next classAgent request — the
// summarizer — routes to the capture path and NOT to beginAgentTurn, so it never
// reconstructs as a turn. The summarizer carries no agent-id header (it is
// main-loop), so without the armed check it would take the normal main path.
func TestGatewayRoutesArmedSummarizerToCapture(t *testing.T) {
	up := sseUpstream(t, http.StatusOK, nil)
	drive := newRecordingDriver()
	drive.rec.armCompaction() // PreCompact arrived
	gw := newTestGateway(t, up.URL, drive, nil)

	resp := postGateway(t, gw, http.MethodPost, "/v1/messages", agentReqBody)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	begins, _ := waitDriver(t, drive, 1) // capture path still calls endAgentTurn
	if got := drive.captureBeginsCount(); got != 1 {
		t.Fatalf("capture begins = %d, want 1 (armed summarizer must route to capture)", got)
	}
	if begins != 0 {
		t.Fatalf("main-loop begins = %d, want 0 (summarizer must not take the main turn path)", begins)
	}
	// Suppressed: the summarizer emits no turn envelope (no PostCompact here, so
	// no boundary either).
	_, _, envelopes := drive.snapshot()
	for _, e := range envelopes {
		if strings.Contains(string(e), "stream_event") || strings.Contains(string(e), `"assistant"`) || strings.Contains(string(e), `"result"`) {
			t.Errorf("summarizer leaked a turn envelope: %s", e)
		}
	}
}

// TestGatewayRoutesSubagentByHeader proves how the gateway splits the main-loop
// and subagent reconstruction paths. The X-Claude-Code-Agent-Id header routes a
// request to beginSubagentTurn when present and beginAgentTurn when absent —
// EXCEPT when the body reveals the request is really the main loop observing a
// backgrounded subagent's completion (a <task-notification> whose task-id is the
// agent's own id), which must stay on the main path despite the header. See
// requestReportsAgentCompletion (turndriver.go).
func TestGatewayRoutesSubagentByHeader(t *testing.T) {
	t.Run("agent-id header routes to subagent path", func(t *testing.T) {
		up := sseUpstream(t, http.StatusOK, nil)
		drive := newRecordingDriver()
		gw := newTestGateway(t, up.URL, drive, nil)

		resp := postGatewayWithHeaders(t, gw, http.MethodPost, "/v1/messages", agentReqBody,
			map[string]string{agentIDHeader: "aid-abc123"})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		// beginSubagentTurn is invoked synchronously before any body is streamed,
		// so it has already run by the time the client drains the response.
		if got := drive.subagentBegins(); got != 1 {
			t.Fatalf("subagent begins = %d, want 1", got)
		}
		if begins, _, _ := drive.snapshot(); begins != 0 {
			t.Fatalf("main-loop begins = %d, want 0 (subagent must not take the main path)", begins)
		}
	})

	t.Run("no header routes to main path", func(t *testing.T) {
		up := sseUpstream(t, http.StatusOK, nil)
		drive := newRecordingDriver()
		gw := newTestGateway(t, up.URL, drive, nil)

		resp := postGateway(t, gw, http.MethodPost, "/v1/messages", agentReqBody)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		begins, _ := waitDriver(t, drive, 1)
		if begins != 1 {
			t.Fatalf("main-loop begins = %d, want 1", begins)
		}
		if got := drive.subagentBegins(); got != 0 {
			t.Fatalf("subagent begins = %d, want 0 (no header must take the main path)", got)
		}
	})

	// The bug this guards: when the MAIN loop resumes to observe a backgrounded
	// subagent's completion, Claude attaches that subagent's agent-id to the
	// resume (plus cc_is_subagent=true). Routing on the header alone misrouted it
	// to beginSubagentTurn, so emitBackgroundCompletions never ran (the completion
	// was lost) and the "second one back" response nested under the never-completing
	// Agent card. A backgrounded subagent's task_id IS its agent_id, so the body's
	// self-referential <task-notification> is the deterministic tell.
	t.Run("agent-id header but body self-reports completion routes to main path", func(t *testing.T) {
		up := sseUpstream(t, http.StatusOK, nil)
		drive := newRecordingDriver()
		gw := newTestGateway(t, up.URL, drive, nil)

		body := bgResumeReqBody(taskNotificationXML("aid-abc123", "toolu_launch", "completed", "/tmp/out.txt", "done"))
		resp := postGatewayWithHeaders(t, gw, http.MethodPost, "/v1/messages", body,
			map[string]string{agentIDHeader: "aid-abc123"})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		begins, _ := waitDriver(t, drive, 1)
		if begins != 1 {
			t.Fatalf("main-loop begins = %d, want 1 (a self-reported completion must take the main path)", begins)
		}
		if got := drive.subagentBegins(); got != 0 {
			t.Fatalf("subagent begins = %d, want 0 (the header must not win over a self-reporting task-notification)", got)
		}

		// End-to-end: the completion was actually reconstructed on the main path —
		// task_updated then task_notification, both keyed to the observed agent.
		_, _, envelopes := drive.snapshot()
		var sawUpdated, sawNotif bool
		for _, e := range envelopes {
			s := string(e)
			if strings.Contains(s, "task_updated") && strings.Contains(s, "aid-abc123") {
				sawUpdated = true
			}
			if strings.Contains(s, "task_notification") && strings.Contains(s, "aid-abc123") {
				sawNotif = true
			}
		}
		if !sawUpdated || !sawNotif {
			t.Fatalf("background completion not reconstructed on the main path: task_updated=%v task_notification=%v; envelopes=%v",
				sawUpdated, sawNotif, envelopes)
		}
	})

	t.Run("agent-id header with a non-self (child) task-notification stays on the subagent path", func(t *testing.T) {
		up := sseUpstream(t, http.StatusOK, nil)
		drive := newRecordingDriver()
		gw := newTestGateway(t, up.URL, drive, nil)

		// A genuine subagent (aid-parent) polling a backgrounded CHILD: the body's
		// <task-notification> task-id is the child's, not the subagent's own
		// agent-id, so the match fails and it correctly stays nested.
		body := bgResumeReqBody(taskNotificationXML("child-task-xyz", "toolu_child", "completed", "/tmp/out.txt", "done"))
		resp := postGatewayWithHeaders(t, gw, http.MethodPost, "/v1/messages", body,
			map[string]string{agentIDHeader: "aid-parent"})
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if got := drive.subagentBegins(); got != 1 {
			t.Fatalf("subagent begins = %d, want 1 (a child task-notification must not divert to the main path)", got)
		}
		if begins, _, _ := drive.snapshot(); begins != 0 {
			t.Fatalf("main-loop begins = %d, want 0 (a child task-notification is still a subagent turn)", begins)
		}
	})
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
		{"suggestion-mode autocomplete", http.StatusOK, http.MethodPost, "/v1/messages", `{"model":"x","max_tokens":64000,"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}]}`},
		{"non-message path", http.StatusOK, http.MethodPost, "/v1/complete", agentReqBody},
		{"non-200 upstream", http.StatusTooManyRequests, http.MethodPost, "/v1/messages", agentReqBody},
		{"GET", http.StatusOK, http.MethodGet, "/v1/messages", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := sseUpstream(t, tc.status, nil)
			drive := newRecordingDriver()
			gw := newTestGateway(t, up.URL, drive, nil)

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

// classifyCapture records every onClassify callback for assertion.
type classifyCapture struct {
	mu      sync.Mutex
	entries []classifyEntry
}

type classifyEntry struct {
	class  requestClass
	status int
	body   string
}

func (c *classifyCapture) record(class requestClass, status int, body []byte) {
	c.mu.Lock()
	c.entries = append(c.entries, classifyEntry{class: class, status: status, body: string(body)})
	c.mu.Unlock()
}

func (c *classifyCapture) snapshot() []classifyEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]classifyEntry(nil), c.entries...)
}

// TestGatewayClassifyCallbackReportsEveryMessagesRequest proves the debug
// classify hook fires for EVERY POST /v1/messages — agent and dropped classes
// alike, and crucially on non-200 responses (the old code classified only
// inside the agent+200 guard, so a dropped or failed call left no trace). Other
// paths and non-POST methods must not fire it. This is the seam the phantom
// investigation relies on to see which request produced a surfaced turn.
func TestGatewayClassifyCallbackReportsEveryMessagesRequest(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		method    string
		path      string
		body      string
		wantFire  bool
		wantClass requestClass
	}{
		{"agent 200", http.StatusOK, http.MethodPost, "/v1/messages", agentReqBody, true, classAgent},
		{"auxiliary 200", http.StatusOK, http.MethodPost, "/v1/messages", `{"model":"x","max_tokens":4096,"tools":[]}`, true, classAuxiliary},
		{"preflight 200", http.StatusOK, http.MethodPost, "/v1/messages", `{"model":"x","max_tokens":1,"tools":[{"name":"Bash"}]}`, true, classPreflight},
		{"nested-subcall 200", http.StatusOK, http.MethodPost, "/v1/messages", `{"model":"x","max_tokens":4096,"tools":[{"type":"web_search_20250305"}]}`, true, classNestedSubcall},
		{"agent non-200", http.StatusTooManyRequests, http.MethodPost, "/v1/messages", agentReqBody, true, classAgent},
		{"non-message path", http.StatusOK, http.MethodPost, "/v1/complete", agentReqBody, false, 0},
		{"GET", http.StatusOK, http.MethodGet, "/v1/messages", "", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := sseUpstream(t, tc.status, nil)
			cap := &classifyCapture{}
			drive := newRecordingDriver()
			gw := newTestGateway(t, up.URL, drive, cap.record)

			resp := postGateway(t, gw, tc.method, tc.path, tc.body)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			entries := cap.snapshot()
			if !tc.wantFire {
				if len(entries) != 0 {
					t.Fatalf("%s: classify fired %d time(s), want 0", tc.name, len(entries))
				}
				return
			}
			if len(entries) != 1 {
				t.Fatalf("%s: classify fired %d time(s), want 1", tc.name, len(entries))
			}
			if entries[0].class != tc.wantClass {
				t.Errorf("%s: class = %v, want %v", tc.name, entries[0].class, tc.wantClass)
			}
			if entries[0].status != tc.status {
				t.Errorf("%s: status = %d, want %d", tc.name, entries[0].status, tc.status)
			}
			if entries[0].body != tc.body {
				t.Errorf("%s: body = %q, want verbatim %q", tc.name, entries[0].body, tc.body)
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
