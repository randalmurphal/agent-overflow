package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
	"agent-overflow/internal/workflowhost"
)

// A workflow continuation of a dormant Claude thread runs through the real
// runner, the App's start door and the Claude provider, against a mock CLI.

const workflowResumeSession = "5d1b0c3e-7a2f-4c55-9d1e-2f6a8b9c0d11"

type workflowResumeProfiles struct{}

func (workflowResumeProfiles) Profile(context.Context, string) (*profile.Profile, error) {
	return &profile.Profile{}, nil
}

type workflowResumeFixture struct {
	t        *testing.T
	app      *App
	runner   *workflowhost.Runner
	project  store.Project
	thread   store.Thread
	itemID   string
	phase    def.Phase
	stdinLog string
	pidFile  string
}

func newWorkflowResumeFixture(t *testing.T) *workflowResumeFixture {
	t.Helper()
	app, _ := setupE2EApp(t)
	project := testutil.EnsureProject(t, app.store, t.TempDir())
	f := &workflowResumeFixture{
		t: t, app: app, project: project, itemID: "resume-item",
		stdinLog: filepath.Join(t.TempDir(), "stdin.log"),
		pidFile:  filepath.Join(t.TempDir(), "pid"),
		phase: def.Phase{
			ID: "work", Driver: def.DriverAgent, Shape: def.ShapeSingle,
			Provider: string(provider.Claude), Model: "claude-opus-4-7", Prompt: "do the work",
			Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}},
		},
	}
	f.runner = newWorkflowAppRunner(app, t.TempDir(), workflowResumeProfiles{})
	app.workflowApplication().SetRunnerForTesting(f.runner)
	if err := app.store.CreateWorkItem(store.WorkItem{
		ID: f.itemID, ProjectID: project.ID, Goal: "resume the work",
		WorkflowID: "flow", WorkflowScope: "shared",
		State: string(engine.StateRunning), Source: "manual", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: f.itemID, PhaseID: f.phase.ID, Attempt: 1, Status: "running", StartedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	f.thread = store.Thread{
		ID: "dormant-workflow-thread", ProjectID: project.ID, ProjectPath: project.Path,
		Title: "work", Mode: "workflow", Provider: string(provider.Claude),
		Model:         provider.NormalizeModelSlug(string(provider.Claude), f.phase.Model),
		WorkspacePath: project.Path, SessionRef: workflowResumeSession,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := app.store.CreateThread(f.thread); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.runner.Stop(context.Background(), f.key())
		stopSessionChecked(t, app, f.thread.ID)
	})
	return f
}

func (f *workflowResumeFixture) key() engine.RunKey {
	return engine.RunKey{ItemID: f.itemID, PhaseID: f.phase.ID, Attempt: 1}
}

// resumeCLI is how the mock Claude CLI of a workflow resume behaves.
type resumeCLI struct {
	// startup is printed before the CLI reads any input.
	startup []string
	// exitAtOnce exits 1 after startup; otherwise every stdin line is
	// recorded until stdin closes.
	exitAtOnce bool
	// loaded answers control requests, as the CLI's message loop does once
	// its resume load returns; a CLI still loading answers none.
	loaded bool
	// contextUsage is the get_context_usage answer's payload: a conversation
	// holding a user message when empty, never answered when "hold", and
	// answered as an error when "refuse".
	contextUsage string
	// conversationUsers, when set, makes get_context_usage count the
	// conversation's user messages as the CLI does: this many resumed ones
	// plus every user input read before the request, 4 tokens each.
	conversationUsers *int
	// gated holds every control answer until the fixture's gate file exists.
	gated bool
	// onProbe is printed when the CLI reads an mcp_status request, before
	// its answer.
	onProbe string
	// initOnInput answers a user input with an init on the resumed session
	// and a successful result.
	initOnInput bool
}

const resumeContextUsage = `{"totalTokens":100,"maxTokens":200000,"messageBreakdown":{"toolCallTokens":23,"toolResultTokens":68,"attachmentTokens":0,"assistantMessageTokens":12,"userMessageTokens":4}}`

// useCLI installs a mock Claude CLI that records its pid and behaves as cli
// says.
func (f *workflowResumeFixture) useCLI(cli resumeCLI) {
	f.t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("echo $$ > " + stringsx.ShellQuote(f.pidFile) + "\n")
	for _, line := range cli.startup {
		b.WriteString("printf '%s\\n' " + stringsx.ShellQuote(line) + "\n")
	}
	if cli.exitAtOnce {
		b.WriteString("echo 'startup failed' >&2\nexit 1\n")
	} else {
		usage := cli.contextUsage
		if usage == "" {
			usage = resumeContextUsage
		}
		// answer <request id> <subtype> <payload>, held until the gate opens
		// when the CLI is gated.
		b.WriteString("answer() {\n")
		answer := `printf '{"type":"control_response","response":{"subtype":"%s","request_id":"%s",%s}}\n' "$2" "$1" "$3"`
		if cli.gated {
			b.WriteString("  ( while [ ! -f " + stringsx.ShellQuote(f.gateFile()) + " ]; do sleep 0.02; done; " + answer + " ) &\n")
		} else {
			b.WriteString("  " + answer + "\n")
		}
		b.WriteString("}\n")
		b.WriteString("users=0\n")
		b.WriteString("while IFS= read -r line; do\n")
		b.WriteString("  printf '%s\\n' \"$line\" >> " + stringsx.ShellQuote(f.stdinLog) + "\n")
		if cli.loaded {
			b.WriteString("  reqid=$(printf '%s' \"$line\" | sed -n 's/.*\"request_id\":\"\\([^\"]*\\)\".*/\\1/p')\n")
			b.WriteString("  case \"$line\" in\n")
			b.WriteString("  *'\"type\":\"user\"'*) users=$((users+1))")
			if cli.initOnInput {
				b.WriteString("; printf '%s\\n' " + stringsx.ShellQuote(initLine(workflowResumeSession)) +
					"; printf '%s\\n' " + stringsx.ShellQuote(`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"done","session_id":"`+workflowResumeSession+`"}`))
			}
			b.WriteString(" ;;\n")
			switch {
			case cli.conversationUsers != nil:
				b.WriteString("  *'\"subtype\":\"get_context_usage\"'*) answer \"$reqid\" success " +
					"\"\\\"response\\\":{\\\"totalTokens\\\":100,\\\"messageBreakdown\\\":{\\\"assistantMessageTokens\\\":12,\\\"userMessageTokens\\\":$(( (" +
					strconv.Itoa(*cli.conversationUsers) + " + users) * 4 ))}}\" ;;\n")
			case usage == "hold":
				b.WriteString("  *'\"subtype\":\"get_context_usage\"'*) ;;\n")
			case usage == "refuse":
				b.WriteString("  *'\"subtype\":\"get_context_usage\"'*) answer \"$reqid\" error " +
					stringsx.ShellQuote(`"error":"Unsupported control request subtype: get_context_usage"`) + " ;;\n")
			default:
				b.WriteString("  *'\"subtype\":\"get_context_usage\"'*) answer \"$reqid\" success " +
					stringsx.ShellQuote(`"response":`+usage) + " ;;\n")
			}
			b.WriteString("  *'\"subtype\":\"mcp_status\"'*) ")
			if cli.onProbe != "" {
				b.WriteString("printf '%s\\n' " + stringsx.ShellQuote(cli.onProbe) + "; ")
			}
			b.WriteString("answer \"$reqid\" success '\"response\":{}' ;;\n")
			b.WriteString("  *'\"type\":\"control_request\"'*) answer \"$reqid\" success '\"response\":{}' ;;\n")
			b.WriteString("  esac\n")
		}
		b.WriteString("done\n")
	}
	binary := mockexec.Write(f.t, filepath.Join(f.t.TempDir(), "claude-resume.sh"), b.String())
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		f.t.Fatal(err)
	}
}

// gateFile is the file whose creation releases a gated CLI's answers.
func (f *workflowResumeFixture) gateFile() string {
	return f.stdinLog + ".gate"
}

func (f *workflowResumeFixture) openGate() {
	f.t.Helper()
	if err := os.WriteFile(f.gateFile(), nil, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// stdinLines is what the CLI has read so far.
func (f *workflowResumeFixture) stdinLines() []string {
	data, _ := os.ReadFile(f.stdinLog)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// stdinIndex is the position of the first line the CLI read containing
// part, or -1.
func (f *workflowResumeFixture) stdinIndex(part string) int {
	for i, line := range f.stdinLines() {
		if strings.Contains(line, part) {
			return i
		}
	}
	return -1
}

func (f *workflowResumeFixture) waitStdin(part string) {
	f.t.Helper()
	waitFor(f.t, "the CLI to read "+part, func() bool { return f.stdinIndex(part) >= 0 })
}

// registerSessionOutsideTheRunner starts the thread's session through the
// App's start door, as a reconnect or a person's send does, without the
// runner, and returns with the session registered.
func (f *workflowResumeFixture) registerSessionOutsideTheRunner(sendFirst string) {
	f.t.Helper()
	// A workflow thread starts only with its phase schema registered; the
	// start this stands for is not the runner's.
	f.setMode("")
	if err := f.app.startSessionTakingLock(f.t.Context(), f.thread.ID); err != nil {
		f.t.Fatalf("start the session outside the runner: %v", err)
	}
	if sendFirst != "" {
		if err := f.app.sendMessage(f.thread.ID, sendFirst, nil); err != nil {
			f.t.Fatalf("send before the continuation: %v", err)
		}
		f.waitStdin(sendFirst)
	}
	f.setMode("workflow")
	if _, ok := f.app.sessionManager().get(f.thread.ID); !ok {
		f.t.Fatal("no session registered outside the runner")
	}
}

func (f *workflowResumeFixture) setMode(mode string) {
	f.t.Helper()
	thread, err := f.app.store.GetThread(f.thread.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	thread.Mode = mode
	if err := f.app.store.UpdateThread(thread); err != nil {
		f.t.Fatal(err)
	}
}

// writeTranscript puts a transcript for the thread's session where the CLI
// would read it.
func (f *workflowResumeFixture) writeTranscript(jsonl string) string {
	f.t.Helper()
	return writeClaudeProjectSession(f.t, testProviderHome(f.t, f.app), f.project.Path, workflowResumeSession, jsonl)
}

func (f *workflowResumeFixture) start(ctx context.Context) error {
	f.t.Helper()
	launch, err := engine.ContinueThread(f.thread.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.runner.Start(ctx, engine.RunRequest{
		Key:      f.key(),
		Item:     store.WorkItem{ID: f.itemID, ProjectID: f.project.ID},
		Workflow: def.Workflow{ID: "flow", Name: "Flow", Phases: []def.Phase{f.phase}},
		Phase:    f.phase, Vars: map[string]any{}, Launch: launch,
		WorkspaceNeed: def.WorkspaceProjectRoot,
	}, func() {}, func(engine.Outcome) {})
}

// requireNothingLeft checks that the thread has no registered session, no
// temporary schema and no live CLI process.
func (f *workflowResumeFixture) requireNothingLeft() {
	f.t.Helper()
	if _, ok := f.app.sessionManager().get(f.thread.ID); ok {
		f.t.Fatal("a session is still registered for the thread")
	}
	if _, registered := f.runner.SessionSchemaForThread(f.thread.ID); registered {
		f.t.Fatal("the temporary schema is still registered")
	}
	pid := f.mockPID()
	waitFor(f.t, "the mock CLI to exit", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

// mockPID is the pid the mock CLI recorded.
func (f *workflowResumeFixture) mockPID() int {
	f.t.Helper()
	data, err := os.ReadFile(f.pidFile)
	if err != nil {
		f.t.Fatalf("read mock pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		f.t.Fatalf("parse mock pid: %v", err)
	}
	return pid
}

func initLine(sessionID string) string {
	return `{"type":"system","subtype":"init","session_id":"` + sessionID + `","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"2.1.325"}`
}

// The transcript is never read: one the leaf scanner refuses (more rows than
// its bound) or cannot open does not stop a continuation the CLI starts.
func TestWorkflowContinuationOfAClaudeThreadNeverReadsItsTranscript(t *testing.T) {
	t.Parallel()
	shapes := map[string]func(t *testing.T, path string){
		"over the scanner's row bound": func(*testing.T, string) {},
		"unreadable": func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its mode")
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWorkflowResumeFixture(t)
			path := f.writeTranscript(`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"do the work"}}` + "\n" +
				strings.Repeat("\n", 500_000))
			prepare(t, path)
			// No init before input, as the CLI: the answer is the proof.
			f.useCLI(resumeCLI{loaded: true})

			if err := f.start(t.Context()); err != nil {
				t.Fatalf("continuation start = %v, want it started on the resumed session", err)
			}
			if _, ok := f.app.sessionManager().get(f.thread.ID); !ok {
				t.Fatal("no session registered after the continuation started")
			}
			waitFor(t, "the continuation prompt to reach the CLI", func() bool {
				data, _ := os.ReadFile(f.stdinLog)
				return strings.Contains(string(data), `"type":"user"`)
			})
		})
	}
}

// The CLI's report that the conversation is gone makes the provider context
// unavailable, whether the process then exits right after the frame (2.1.88
// and 2.1.325) or stays alive, and the failed start leaves nothing behind. A
// transcript is on disk, so nothing but the CLI's own answer can say the
// conversation is gone.
func TestWorkflowContinuationMapsClaudeSessionNotFoundToUnavailable(t *testing.T) {
	t.Parallel()
	notFound := `{"type":"result","subtype":"error_during_execution","duration_ms":0,"is_error":true,"num_turns":0,"errors":["No conversation found with session ID: ` + workflowResumeSession + `"]}`
	for name, exitAtOnce := range map[string]bool{"stays alive": false, "exits": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWorkflowResumeFixture(t)
			f.writeTranscript(`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"do the work"}}` + "\n")
			f.useCLI(resumeCLI{startup: []string{notFound}, exitAtOnce: exitAtOnce, loaded: true})

			err := f.start(t.Context())
			if !errors.Is(err, engine.ErrProviderContextUnavailable) {
				t.Fatalf("continuation start = %v, want the provider context unavailable", err)
			}
			f.requireNothingLeft()
		})
	}
}

// Every other way a start can fail stays the start's own visible error, and
// leaves nothing behind.
func TestWorkflowContinuationKeepsOtherClaudeStartFailuresVisible(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		startup    []string
		exitAtOnce bool
		want       string
	}{
		"process exits before init": {exitAtOnce: true, want: "exited before it initialized"},
		"init for a different session": {
			startup: []string{initLine("0f0e0d0c-0000-4000-8000-000000000000")},
			want:    "initialized on a different session",
		},
		"other startup error": {
			startup: []string{`{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"errors":["No message found with message.uuid of: abc"]}`},
			want:    "failed to start",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWorkflowResumeFixture(t)
			f.useCLI(resumeCLI{startup: tc.startup, exitAtOnce: tc.exitAtOnce, loaded: true})

			err := f.start(t.Context())
			if err == nil || errors.Is(err, engine.ErrProviderContextUnavailable) {
				t.Fatalf("continuation start = %v, want a visible start failure", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("continuation start = %v, want it to say %q", err, tc.want)
			}
			f.requireNothingLeft()
		})
	}
}

// A start cancelled while the CLI is still loading, or before it reports the
// loaded conversation's content, is a cancelled start, not a missing
// conversation, and it stops the process it launched.
func TestWorkflowContinuationCancelledWhileClaudeLoads(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		cli     resumeCLI
		waiting string
	}{
		"still loading":        {cli: resumeCLI{}, waiting: `"subtype":"mcp_status"`},
		"content not answered": {cli: resumeCLI{loaded: true, contextUsage: "hold"}, waiting: `"subtype":"get_context_usage"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWorkflowResumeFixture(t)
			f.useCLI(tc.cli)

			ctx, cancel := context.WithCancel(t.Context())
			errs := make(chan error, 1)
			go func() { errs <- f.start(ctx) }()
			waitFor(t, "the request the start waits on", func() bool {
				data, _ := os.ReadFile(f.stdinLog)
				return strings.Contains(string(data), tc.waiting)
			})
			select {
			case err := <-errs:
				t.Fatalf("start returned %v while its request was unanswered", err)
			case <-time.After(100 * time.Millisecond):
			}
			cancel()
			err := <-errs
			if err == nil || errors.Is(err, engine.ErrProviderContextUnavailable) || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled start = %v, want the cancellation", err)
			}
			f.requireNothingLeft()
			// The proof held the thread; a cancelled one lets it go.
			lockCtx, cancelLock := context.WithTimeout(t.Context(), 5*time.Second)
			unlock, err := f.app.threadLocks().LockCtx(lockCtx, f.thread.ID)
			cancelLock()
			if err != nil {
				t.Fatalf("the thread action lock after a cancelled proof: %v", err)
			}
			unlock()
		})
	}
}

// A resume is reused only when the CLI counts a user message in the
// conversation it loaded. One with none (system and attachment rows only, or
// a chain the CLI's filters emptied), and an answer that cannot say, are
// unavailable and leave nothing behind, so the workflow rebuilds the round;
// the error names why.
func TestWorkflowContinuationRefusesAClaudeConversationWithNoUserMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		usage string
		// cause is in the error of an unavailable context; empty when the
		// session is kept.
		cause string
	}{
		"no user message": {
			usage: `{"totalTokens":73,"maxTokens":200000,"messageBreakdown":{"toolCallTokens":0,"toolResultTokens":0,"attachmentTokens":61,"assistantMessageTokens":12,"userMessageTokens":0}}`,
			cause: "0 user-message tokens",
		},
		"a user message": {usage: resumeContextUsage},
		"malformed":      {usage: `{"messageBreakdown":{"userMessageTokens":"none"}}`, cause: "unreadable"},
		"refused":        {usage: "refuse", cause: "Unsupported control request subtype: get_context_usage"},
		"no breakdown":   {usage: `{"totalTokens":100,"maxTokens":200000}`, cause: "no messageBreakdown"},
		"no count":       {usage: `{"messageBreakdown":{"assistantMessageTokens":12}}`, cause: "no userMessageTokens"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newWorkflowResumeFixture(t)
			f.useCLI(resumeCLI{loaded: true, contextUsage: tc.usage})

			err := f.start(t.Context())
			if tc.cause != "" {
				if !errors.Is(err, engine.ErrProviderContextUnavailable) || !strings.Contains(err.Error(), tc.cause) {
					t.Fatalf("continuation start = %v, want the provider context unavailable naming %q", err, tc.cause)
				}
				f.requireNothingLeft()
				return
			}
			if err != nil {
				t.Fatalf("continuation start = %v, want the resumed session kept", err)
			}
			waitFor(t, "the continuation prompt to reach the CLI", func() bool {
				data, _ := os.ReadFile(f.stdinLog)
				return strings.Contains(string(data), `"type":"user"`)
			})
		})
	}
}
