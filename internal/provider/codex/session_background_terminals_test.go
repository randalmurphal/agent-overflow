package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// backgroundTerminalTestTimeout bounds every wait in this file. Generous
// on purpose: a passing test never waits, and only a hang pays it — a
// tighter bound just turns a slow `-race` build into a flake.
const backgroundTerminalTestTimeout = 15 * time.Second

// backgroundTerminalRequestFrame is the decoded outbound wire frame the
// per-row background-terminal RPCs write. Only the fields the tests
// assert on are modelled; decoding rather than string-matching keeps the
// assertions independent of JSON key ordering.
type backgroundTerminalRequestFrame struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		ThreadID  string  `json:"threadId"`
		ProcessID string  `json:"processId"`
		Cursor    *string `json:"cursor"`
	} `json:"params"`
}

// newCapturingSession builds an in-process Session whose subprocess is a
// plain `cat` into capturePath, so every request frame we write lands on
// disk while responses are injected straight into the pending channel.
// Same shape as the CleanBackgroundTerminals in-process tests, plus the
// capture file.
func newCapturingSession(t *testing.T, codexThreadID string) (*Session, string) {
	t.Helper()
	capturePath := t.TempDir() + "/requests.ndjson"
	procCtx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(procCtx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", fmt.Sprintf("cat > %q", capturePath)},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancelProc,
	}
	s.setRootThreadID(codexThreadID)
	go s.readLoop()
	return s, capturePath
}

// waitForCapturedFrames polls the capture file until it holds at least
// count decodable frames. `cat` copies our writes asynchronously, so the
// frame can trail the pending-channel registration by a scheduling slice.
func waitForCapturedFrames(t *testing.T, path string, count int, within time.Duration) []backgroundTerminalRequestFrame {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read captured requests: %v", err)
		}
		var frames []backgroundTerminalRequestFrame
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var frame backgroundTerminalRequestFrame
			if json.Unmarshal([]byte(line), &frame) != nil {
				continue
			}
			frames = append(frames, frame)
		}
		if len(frames) >= count {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d captured frames, got %d (raw: %s)", count, len(frames), string(raw))
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pendingAnswerer answers in-flight requests one at a time. It tracks the
// ids it has already served so a paginating caller's second request is
// matched to a genuinely new pending entry — waitForPending returns an
// arbitrary map member, so answering twice without this would re-serve
// the first (already-drained) channel and hang the second request.
type pendingAnswerer struct {
	session *Session
	served  map[int64]struct{}
}

func newPendingAnswerer(s *Session) *pendingAnswerer {
	return &pendingAnswerer{session: s, served: map[int64]struct{}{}}
}

// answer injects a JSON-RPC result body for the next unserved in-flight
// request.
func (p *pendingAnswerer) answer(t *testing.T, result string) {
	t.Helper()
	ch, rpcID := p.next(t, backgroundTerminalTestTimeout)
	ch <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, rpcID, result))
}

func (p *pendingAnswerer) next(t *testing.T, within time.Duration) (chan json.RawMessage, int64) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		p.session.mu.Lock()
		for id, ch := range p.session.pending {
			if _, done := p.served[id]; done {
				continue
			}
			p.served[id] = struct{}{}
			p.session.mu.Unlock()
			return ch, id
		}
		p.session.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a new pending request registration")
			return nil, 0
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSession_ListBackgroundTerminals_DecodesEveryWireField drives one
// page of `thread/backgroundTerminals/list` and asserts both the outbound
// frame (method name, threadId, no cursor on the first page) and the full
// inbound projection. The second entry omits every nullable host metric,
// which must decode as nil rather than a zero reading — a UI that renders
// "0 KB / 0% CPU" for an unmeasured process is stating a fact the server
// never sent.
func TestSession_ListBackgroundTerminals_DecodesEveryWireField(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-list")

	type listResult struct {
		terminals []BackgroundTerminal
		err       error
	}
	done := make(chan listResult, 1)
	go func() {
		terminals, err := s.ListBackgroundTerminals(context.Background())
		done <- listResult{terminals: terminals, err: err}
	}()

	newPendingAnswerer(s).answer(t, `{"data":[
		{"itemId":"item-1","processId":"42","command":"pnpm dev","cwd":"/repo",
		 "osPid":98765,"cpuPercent":12.5,"rssKb":204800},
		{"itemId":"item-2","processId":"43","command":"tail -f log","cwd":"/repo",
		 "osPid":null,"cpuPercent":null,"rssKb":null}
	],"nextCursor":null}`)

	got := <-done
	if got.err != nil {
		t.Fatalf("ListBackgroundTerminals: %v", got.err)
	}
	if len(got.terminals) != 2 {
		t.Fatalf("got %d terminals, want 2: %+v", len(got.terminals), got.terminals)
	}

	first := got.terminals[0]
	if first.ItemID != "item-1" || first.ProcessID != "42" ||
		first.Command != "pnpm dev" || first.Cwd != "/repo" {
		t.Errorf("first terminal identity fields wrong: %+v", first)
	}
	if first.OSPid == nil || *first.OSPid != 98765 {
		t.Errorf("first.OSPid = %v, want 98765", first.OSPid)
	}
	if first.CPUPercent == nil || *first.CPUPercent != 12.5 {
		t.Errorf("first.CPUPercent = %v, want 12.5", first.CPUPercent)
	}
	if first.RSSKb == nil || *first.RSSKb != 204800 {
		t.Errorf("first.RSSKb = %v, want 204800", first.RSSKb)
	}

	second := got.terminals[1]
	if second.OSPid != nil || second.CPUPercent != nil || second.RSSKb != nil {
		t.Errorf("null host metrics must decode as nil, got %+v", second)
	}

	frames := waitForCapturedFrames(t, capturePath, 1, backgroundTerminalTestTimeout)
	if frames[0].Method != "thread/backgroundTerminals/list" {
		t.Errorf("method = %q, want thread/backgroundTerminals/list", frames[0].Method)
	}
	if frames[0].Params.ThreadID != "codex-thread-list" {
		t.Errorf("threadId = %q, want codex-thread-list", frames[0].Params.ThreadID)
	}
	if frames[0].Params.Cursor != nil {
		t.Errorf("first page must omit cursor, got %q", *frames[0].Params.Cursor)
	}
}

// TestSession_ListBackgroundTerminals_FollowsCursor pins the pagination
// walk: a non-null nextCursor must produce a second request carrying it,
// and the caller must see both pages concatenated in order.
func TestSession_ListBackgroundTerminals_FollowsCursor(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-pages")

	done := make(chan []BackgroundTerminal, 1)
	errCh := make(chan error, 1)
	go func() {
		terminals, err := s.ListBackgroundTerminals(context.Background())
		if err != nil {
			errCh <- err
			return
		}
		done <- terminals
	}()

	answerer := newPendingAnswerer(s)
	answerer.answer(t, `{"data":[{"itemId":"a","processId":"1","command":"one","cwd":"/w"}],"nextCursor":"page-2"}`)
	answerer.answer(t, `{"data":[{"itemId":"b","processId":"2","command":"two","cwd":"/w"}],"nextCursor":null}`)

	select {
	case err := <-errCh:
		t.Fatalf("ListBackgroundTerminals: %v", err)
	case terminals := <-done:
		if len(terminals) != 2 || terminals[0].ItemID != "a" || terminals[1].ItemID != "b" {
			t.Fatalf("pages not concatenated in order: %+v", terminals)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ListBackgroundTerminals never returned")
	}

	frames := waitForCapturedFrames(t, capturePath, 2, backgroundTerminalTestTimeout)
	if frames[0].Params.Cursor != nil {
		t.Errorf("first page must omit cursor, got %q", *frames[0].Params.Cursor)
	}
	if frames[1].Params.Cursor == nil || *frames[1].Params.Cursor != "page-2" {
		t.Errorf("second page cursor = %v, want page-2", frames[1].Params.Cursor)
	}
}

// TestSession_ListBackgroundTerminals_RepeatedCursorFailsLoudly covers a
// server that hands back the cursor it was just given. Without the
// progress check that is an unbounded request loop against the
// app-server; the contract is a loud error instead.
func TestSession_ListBackgroundTerminals_RepeatedCursorFailsLoudly(t *testing.T) {
	s, _ := newCapturingSession(t, "codex-thread-stuck")

	errCh := make(chan error, 1)
	go func() {
		_, err := s.ListBackgroundTerminals(context.Background())
		errCh <- err
	}()

	answerer := newPendingAnswerer(s)
	answerer.answer(t, `{"data":[],"nextCursor":"stuck"}`)
	answerer.answer(t, `{"data":[],"nextCursor":"stuck"}`)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected an error when the server repeats a cursor")
		}
		if !strings.Contains(err.Error(), "repeated cursor") {
			t.Errorf("error should name the repeated cursor, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ListBackgroundTerminals kept paginating instead of failing")
	}
}

// TestSession_TerminateBackgroundTerminal_RoundTrip asserts the outbound
// frame (method + threadId + processId) and that `terminated` is returned
// verbatim. The false case is the "already exited / not ours" answer and
// must NOT surface as an error.
func TestSession_TerminateBackgroundTerminal_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     string
		wantKilled bool
	}{
		{name: "terminated", result: `{"terminated":true}`, wantKilled: true},
		{name: "no matching process", result: `{"terminated":false}`, wantKilled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, capturePath := newCapturingSession(t, "codex-thread-term")

			type terminateResult struct {
				killed bool
				err    error
			}
			done := make(chan terminateResult, 1)
			go func() {
				killed, err := s.TerminateBackgroundTerminal(context.Background(), "42")
				done <- terminateResult{killed: killed, err: err}
			}()

			newPendingAnswerer(s).answer(t, tc.result)

			got := <-done
			if got.err != nil {
				t.Fatalf("TerminateBackgroundTerminal: %v", got.err)
			}
			if got.killed != tc.wantKilled {
				t.Errorf("terminated = %v, want %v", got.killed, tc.wantKilled)
			}

			frames := waitForCapturedFrames(t, capturePath, 1, backgroundTerminalTestTimeout)
			if frames[0].Method != "thread/backgroundTerminals/terminate" {
				t.Errorf("method = %q, want thread/backgroundTerminals/terminate", frames[0].Method)
			}
			if frames[0].Params.ThreadID != "codex-thread-term" {
				t.Errorf("threadId = %q, want codex-thread-term", frames[0].Params.ThreadID)
			}
			// The 0.146.0 app-server rejects a request without this exact
			// key with `-32600 missing field processId` (spike capture
			// 41-bgterminals-terminate-probe), so the name is load-bearing.
			if frames[0].Params.ProcessID != "42" {
				t.Errorf("processId = %q, want 42", frames[0].Params.ProcessID)
			}
		})
	}
}

// TestSession_TerminateBackgroundTerminal_RejectsBlankProcessID keeps a
// caller with an empty/whitespace id from reaching the wire, where it
// would come back as an opaque `-32600 missing field` instead of naming
// the client-side bug.
func TestSession_TerminateBackgroundTerminal_RejectsBlankProcessID(t *testing.T) {
	s, capturePath := newCapturingSession(t, "codex-thread-blank")

	for _, processID := range []string{"", "   "} {
		killed, err := s.TerminateBackgroundTerminal(context.Background(), processID)
		if err == nil {
			t.Fatalf("TerminateBackgroundTerminal(%q): expected an error, got nil", processID)
		}
		if killed {
			t.Errorf("TerminateBackgroundTerminal(%q) reported a kill it never attempted", processID)
		}
		if !strings.Contains(err.Error(), "process id required") {
			t.Errorf("error should name the missing process id, got: %v", err)
		}
	}

	if raw, err := os.ReadFile(capturePath); err == nil && strings.TrimSpace(string(raw)) != "" {
		t.Errorf("blank process id must not reach the wire, captured: %s", string(raw))
	}
}

// TestSession_BackgroundTerminalRPCs_RequireThreadID pins the partial
// Session guard both per-row RPCs share with CleanBackgroundTerminals: a
// session whose handshake never produced a Codex thread id must fail with
// our own message, not send `{"threadId":""}` and let the server answer.
func TestSession_BackgroundTerminalRPCs_RequireThreadID(t *testing.T) {
	s := &Session{
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
	}

	if _, err := s.ListBackgroundTerminals(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no thread id") {
		t.Errorf("ListBackgroundTerminals without a thread id: got %v", err)
	}
	if _, err := s.TerminateBackgroundTerminal(context.Background(), "42"); err == nil ||
		!strings.Contains(err.Error(), "no thread id") {
		t.Errorf("TerminateBackgroundTerminal without a thread id: got %v", err)
	}
}

// TestSession_TerminateBackgroundTerminal_ErrorResponse confirms a
// JSON-RPC error reaches the caller with the server's detail intact
// rather than being flattened into `terminated: false`, which would read
// as "the process was already gone".
func TestSession_TerminateBackgroundTerminal_ErrorResponse(t *testing.T) {
	s, _ := newCapturingSession(t, "codex-thread-term-err")

	type terminateResult struct {
		killed bool
		err    error
	}
	done := make(chan terminateResult, 1)
	go func() {
		killed, err := s.TerminateBackgroundTerminal(context.Background(), "42")
		done <- terminateResult{killed: killed, err: err}
	}()

	ch, rpcID := waitForPending(t, s, backgroundTerminalTestTimeout)
	ch <- json.RawMessage(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"error":{"code":-32600,"message":"Invalid request: missing field `+"`processId`"+`"}}`,
		rpcID,
	))

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("expected the JSON-RPC error to surface")
		}
		if got.killed {
			t.Error("a failed RPC must not report a kill")
		}
		if !strings.Contains(got.err.Error(), "missing field") {
			t.Errorf("error should carry the server detail, got: %v", got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TerminateBackgroundTerminal never returned")
	}
}
