package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// mockBin is the ao-mockprovider binary built once per test run by
// TestMain and driven as a subprocess over real pipes.
var mockBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "ao-mockprovider-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
		os.Exit(1)
	}
	mockBin = filepath.Join(tmp, "ao-mockprovider")
	build := exec.Command("go", "build", "-o", mockBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build ao-mockprovider: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// mockProc drives one ao-mockprovider subprocess over real pipes.
type mockProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	all    []string // every stdout line, for parser-validation passes
	stderr *syncBuffer
}

// startMock launches the built binary with the given argv, extra env,
// and workdir. Harness/scenario env inherited from the test runner is
// stripped so tests control acquisition exactly.
func startMock(t *testing.T, args, extraEnv []string, dir string) *mockProc {
	t.Helper()
	cmd := exec.Command(mockBin, args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, control.EnvAddr+"=") ||
			strings.HasPrefix(kv, control.EnvToken+"=") ||
			strings.HasPrefix(kv, control.EnvTranscriptHome+"=") ||
			strings.HasPrefix(kv, envScenarioFile+"=") ||
			strings.HasPrefix(kv, envFixtureRoot+"=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, extraEnv...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	p := &mockProc{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 256), stderr: &syncBuffer{}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mock: %v", err)
	}

	go func() {
		defer close(p.lines)
		r := newTestLineReader(stdout)
		for {
			line, err := r()
			if line != "" {
				p.lines <- line
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("mock stderr:\n%s", p.stderr.String())
		}
	})
	return p
}

func TestClaudeHarnessTranscriptIsDurableBeforeTheEcho(t *testing.T) {
	home := t.TempDir()
	env := append(writeScenarioFile(t, claudeTwoTurnScenario(), ""),
		control.EnvTranscriptHome+"="+home)
	args := append(append([]string(nil), claudeSessionArgs...), "--resume", "sess-durable")
	p := startMock(t, args, env, t.TempDir())

	p.send(userLine)
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	p.expectLineContaining(`"type":"user"`, testTimeout)

	path := filepath.Join(home, ".claude", "projects", "mock", "sess-durable.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript after visible echo: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &row); err != nil {
		t.Fatalf("decode transcript row: %v", err)
	}
	if row["type"] != "user" || row["uuid"] == "" {
		t.Fatalf("transcript row = %+v, want a resumable user leaf", row)
	}

	p.closeStdinAndExpectExit(0, testTimeout)
}

func newTestLineReader(r io.Reader) func() (string, error) {
	br := newBufReader(r)
	return func() (string, error) {
		raw, err := br.ReadBytes('\n')
		return strings.TrimRight(string(raw), "\r\n"), err
	}
}

func (p *mockProc) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.stdin, line+"\n"); err != nil {
		p.t.Fatalf("write stdin: %v", err)
	}
}

func (p *mockProc) expectLine(timeout time.Duration) string {
	p.t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("stdout closed while expecting a line; stderr:\n%s", p.stderr.String())
		}
		p.all = append(p.all, line)
		return line
	case <-time.After(timeout):
		p.t.Fatalf("timed out waiting for stdout line; stderr:\n%s", p.stderr.String())
		return ""
	}
}

func (p *mockProc) expectLineContaining(substr string, timeout time.Duration) string {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			p.t.Fatalf("timed out waiting for line containing %q; stderr:\n%s", substr, p.stderr.String())
		}
		line := p.expectLine(remaining)
		if strings.Contains(line, substr) {
			return line
		}
	}
}

func (p *mockProc) closeStdinAndExpectExit(wantCode int, timeout time.Duration) {
	p.t.Helper()
	_ = p.stdin.Close()
	p.expectExit(wantCode, timeout)
}

func (p *mockProc) expectExit(wantCode int, timeout time.Duration) {
	p.t.Helper()
	// Drain stdout to EOF first so Wait doesn't race the pipe reader.
	deadline := time.Now().Add(timeout)
	for open := true; open; {
		select {
		case line, ok := <-p.lines:
			if !ok {
				open = false
				break
			}
			p.all = append(p.all, line)
		case <-time.After(time.Until(deadline)):
			p.t.Fatalf("stdout never closed while awaiting exit; stderr:\n%s", p.stderr.String())
		}
	}
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		code := p.cmd.ProcessState.ExitCode()
		if code != wantCode {
			p.t.Fatalf("exit code = %d (err %v), want %d; stderr:\n%s", code, err, wantCode, p.stderr.String())
		}
	case <-time.After(timeout):
		p.t.Fatalf("process did not exit; stderr:\n%s", p.stderr.String())
	}
}

// writeScenarioFile serializes a scenario to a temp file and returns
// the env assignments that point the mock at it.
func writeScenarioFile(t *testing.T, sc *scenario.Scenario, fixtureRoot string) []string {
	t.Helper()
	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	if _, err := scenario.Parse(data); err != nil {
		t.Fatalf("test scenario invalid: %v", err)
	}
	path := filepath.Join(t.TempDir(), "scenario.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	env := []string{envScenarioFile + "=" + path}
	if fixtureRoot != "" {
		env = append(env, envFixtureRoot+"="+fixtureRoot)
	}
	return env
}

var claudeSessionArgs = []string{
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--permission-prompt-tool", "stdio",
	"--include-partial-messages",
	"--replay-user-messages",
}

const userLine = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`

const testTimeout = 10 * time.Second

// --- version ---

func TestVersionSatisfiesBothProviders(t *testing.T) {
	out, err := exec.Command(mockBin, "--version").Output()
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if got != versionString {
		t.Fatalf("--version output = %q, want %q", got, versionString)
	}

	// Claude gate: any trimmed non-empty output. Codex gate: DetectProvider
	// must come back "ready" (not version_too_old / error).
	for _, name := range []string{"claude", "codex"} {
		status := providerDetect(t, name)
		if !status.Installed || status.Status != "ready" {
			t.Fatalf("DetectProvider(%q) = %+v, want ready", name, status)
		}
	}

	// DetectProvider reports "ready" even when the version fails to parse
	// (the gate only fires on a parsed version), so additionally pin the
	// codex parse: the first semver token — the one parseCodexCLIVersion's
	// regex selects — must be a high version, comfortably >= 0.143.0.
	first := regexp.MustCompile(`\bv?(\d+\.\d+(?:\.\d+)?(?:-[0-9A-Za-z.-]+)?)\b`).FindStringSubmatch(got)
	if len(first) < 2 || first[1] != "99.0.0" {
		t.Fatalf("first version token in %q = %v, want 99.0.0", got, first)
	}
}

// --- claude mode ---

// claudeTwoTurnScenario carries only scenario-owned frames: the mock's
// claude adapter emits the per-turn system/init and the user-envelope
// echo itself, exactly like the real CLI.
func claudeTwoTurnScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-happy",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Label: "reply", Steps: []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
			`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-${TURN}","role":"assistant"}}}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello turn ${TURN}"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`,
			`{"type":"assistant","message":{"id":"msg-${TURN}","role":"assistant","content":[{"type":"text","text":"hello turn ${TURN}"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		}}}}}},
		AfterTurns: "repeatLast",
	}
}

func TestClaudeHappyPathTurnsAndInterruptAck(t *testing.T) {
	env := writeScenarioFile(t, claudeTwoTurnScenario(), "")
	args := append(append([]string(nil), claudeSessionArgs...), "--resume", "sess-42")
	p := startMock(t, args, env, t.TempDir())

	// Out-of-band control_request must be acked without consuming a
	// turn — and nothing precedes it, because init is per-turn now.
	p.send(`{"type":"control_request","request_id":"so-1","request":{"subtype":"interrupt"}}`)
	ack := p.expectLine(testTimeout)
	if ack != `{"type":"control_response","response":{"subtype":"success","request_id":"so-1","response":{}}}` {
		t.Fatalf("interrupt ack = %q", ack)
	}

	// Each user turn opens with an adapter-emitted init (carrying the
	// resumed session id) and the user-envelope replay echo.
	p.send(userLine)
	init := p.expectLine(testTimeout)
	if !strings.Contains(init, `"session_id":"sess-42"`) || !strings.Contains(init, `"subtype":"init"`) {
		t.Fatalf("turn-1 init line = %q", init)
	}
	echo := p.expectLine(testTimeout)
	if !strings.Contains(echo, `"isReplay":true`) || !strings.Contains(echo, `"type":"user"`) {
		t.Fatalf("turn-1 echo line = %q", echo)
	}
	p.expectLineContaining(`"text_delta","text":"hello turn 1"`, testTimeout)
	p.expectLineContaining(`"id":"msg-1"`, testTimeout)
	p.expectLineContaining(`"type":"result"`, testTimeout)

	// Beyond the scripted turns: repeatLast re-runs with the next TURN,
	// and the adapter emits a fresh init for it.
	p.send(userLine)
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	p.expectLineContaining(`"text_delta","text":"hello turn 2"`, testTimeout)
	p.expectLineContaining(`"id":"msg-2"`, testTimeout)
	p.expectLineContaining(`"type":"result"`, testTimeout)

	// set_permission_mode (the app sends this before turns) is acked too.
	p.send(`{"type":"control_request","request_id":"so-2","request":{"subtype":"set_permission_mode","mode":"plan"}}`)
	p.expectLineContaining(`"request_id":"so-2"`, testTimeout)

	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

func TestClaudeInterruptAbortsWaitSignalTurn(t *testing.T) {
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-interrupt",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Steps: []scenario.Step{
			{Emit: &scenario.EmitStep{Lines: []string{
				`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-interrupt","role":"assistant"}}}`,
				`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
				`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"working"}}}`,
			}}},
			{WaitSignal: &scenario.WaitSignalStep{Name: "hold"}},
			{Emit: &scenario.EmitStep{Lines: []string{`{"mock":"must-not-run"}`}}},
		}}},
	}
	p := startMock(t, claudeSessionArgs, writeScenarioFile(t, sc, ""), t.TempDir())
	p.send(userLine)
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	p.expectLineContaining(`"isReplay":true`, testTimeout)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"id":"msg-interrupt"`) {
		t.Fatalf("pre-interrupt line = %q", got)
	}
	p.expectLineContaining(`"content_block_start"`, testTimeout)
	p.expectLineContaining(`"text":"working"`, testTimeout)

	p.send(`{"type":"control_request","request_id":"stop-1","request":{"subtype":"interrupt"}}`)
	if got := p.expectLine(testTimeout); got != `{"type":"control_response","response":{"subtype":"success","request_id":"stop-1","response":{}}}` {
		t.Fatalf("interrupt ack = %q", got)
	}
	result := p.expectLine(testTimeout)
	if !strings.Contains(result, `"subtype":"error_during_execution"`) ||
		!strings.Contains(result, `"terminal_reason":"aborted_streaming"`) ||
		!strings.Contains(result, `"is_error":true`) {
		t.Fatalf("interrupted result = %q", result)
	}
	parser := claude.NewParser()
	t.Cleanup(parser.Close)
	parser.MarkInterruptAcked()
	events, err := parser.ParseLine("thread-interrupt", []byte(result))
	if err != nil {
		t.Fatalf("parse interrupted result: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("interrupted result events = %+v", events)
	}
	meta, ok := events[0].TurnComplete.(*provider.WireTurnCompleteMeta)
	if events[0].Kind != provider.EventTurnComplete || !ok ||
		!meta.Aborted || meta.StopReason != "interrupted" || meta.ErrorMessage != "" {
		t.Fatalf("interrupted result events = %+v", events)
	}

	// A later out-of-band request must be the next line. If the engine ran
	// the post-gate step, its marker would appear first and fail this check.
	p.send(`{"type":"control_request","request_id":"mode-1","request":{"subtype":"set_permission_mode"}}`)
	if got := p.expectLine(testTimeout); !strings.Contains(got, `"request_id":"mode-1"`) {
		t.Fatalf("line after interrupted result = %q; remaining scenario step ran", got)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

func TestClaudeApprovalRoundTrip(t *testing.T) {
	approvalScenario := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-approval",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Steps: []scenario.Step{{Approval: &scenario.ApprovalStep{
			ToolName: "Bash",
			Input:    json.RawMessage(`{"command":"ls ${CWD}"}`),
			OnAllow:  []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{`{"mock":"allowed"}`}}}},
			OnDeny:   []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{`{"mock":"denied"}`}}}},
		}}}}},
	}

	run := func(t *testing.T, behavior, want string) {
		// ${CWD} is substituted from the mock's os.Getwd(), which reports the
		// symlink-resolved path (/private/var vs /var on macOS temp dirs).
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		env := writeScenarioFile(t, approvalScenario, "")
		p := startMock(t, claudeSessionArgs, env, dir)
		p.send(userLine)

		reqLine := p.expectLineContaining(`"subtype":"can_use_tool"`, testTimeout)
		var req struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype  string `json:"subtype"`
				ToolName string `json:"tool_name"`
				Input    struct {
					Command string `json:"command"`
				} `json:"input"`
			} `json:"request"`
		}
		if err := json.Unmarshal([]byte(reqLine), &req); err != nil {
			t.Fatalf("parse approval request %q: %v", reqLine, err)
		}
		if req.Type != "control_request" || req.RequestID == "" || req.Request.ToolName != "Bash" {
			t.Fatalf("approval request = %+v", req)
		}
		if req.Request.Input.Command != "ls "+dir {
			t.Fatalf("input.command = %q (vars not substituted?)", req.Request.Input.Command)
		}

		// Answer in the exact shape the app writes (claude/approvals.go).
		p.send(fmt.Sprintf(
			`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"behavior":%q}}}`,
			req.RequestID, behavior))
		branch := p.expectLine(testTimeout)
		if branch != want {
			t.Fatalf("branch line = %q, want %q", branch, want)
		}
		p.closeStdinAndExpectExit(0, testTimeout)
	}

	t.Run("allow", func(t *testing.T) { run(t, "allow", `{"mock":"allowed"}`) })
	t.Run("deny", func(t *testing.T) { run(t, "deny", `{"mock":"denied"}`) })
}

func TestClaudeChunkedEmissionReassembles(t *testing.T) {
	long := strings.Repeat("chunky-payload-", 40)
	sc := &scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "claude-chunked",
		Provider: scenario.ProviderClaude,
		Turns: []scenario.Turn{{Steps: []scenario.Step{{Emit: &scenario.EmitStep{
			Lines:           []string{`{"payload":"` + long + `"}`, `{"after":"chunks"}`},
			ChunkBytes:      7,
			ChunkIntervalMs: 1,
		}}}}},
	}
	env := writeScenarioFile(t, sc, "")
	p := startMock(t, claudeSessionArgs, env, t.TempDir())
	p.send(userLine)

	// Skip the adapter-emitted init + echo to reach the scenario lines.
	if got := p.expectLineContaining(`"payload":"`, testTimeout); got != `{"payload":"`+long+`"}` {
		t.Fatalf("chunked line reassembled to %q", got)
	}
	if got := p.expectLine(testTimeout); got != `{"after":"chunks"}` {
		t.Fatalf("second line = %q", got)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}

func TestClaudeBuiltinFallbackWithoutScenario(t *testing.T) {
	p := startMock(t, claudeSessionArgs, nil, t.TempDir())
	p.send(userLine)
	p.expectLineContaining(`"subtype":"init"`, testTimeout)
	p.expectLineContaining(fallbackResponseText, testTimeout)
	p.expectLineContaining(`"type":"result"`, testTimeout)
	if !strings.Contains(p.stderr.String(), "builtin claude fallback") {
		t.Fatalf("fallback was not announced on stderr:\n%s", p.stderr.String())
	}
	p.closeStdinAndExpectExit(0, testTimeout)
	validateClaudeFrames(t, p.all)
}

func TestClaudeProbeInvocation(t *testing.T) {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--max-turns", "0"}
	p := startMock(t, args, nil, t.TempDir())

	init := p.expectLine(testTimeout)
	if !strings.Contains(init, `"subtype":"init"`) || !strings.Contains(init, `"subscriptionType":"Claude Max"`) {
		t.Fatalf("probe init line = %q", init)
	}

	// The real probe (claude/probe.go) sends initialize and reads the
	// account off the control_response payload.
	p.send(`{"type":"control_request","request_id":"ao-probe-init","request":{"subtype":"initialize"}}`)
	resp := p.expectLine(testTimeout)
	if !strings.Contains(resp, `"request_id":"ao-probe-init"`) ||
		!strings.Contains(resp, `"subtype":"success"`) ||
		!strings.Contains(resp, `"subscriptionType":"Claude Max"`) {
		t.Fatalf("probe initialize response = %q", resp)
	}
	p.closeStdinAndExpectExit(0, testTimeout)
}
