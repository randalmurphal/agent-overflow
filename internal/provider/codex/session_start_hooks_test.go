package codex

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
)

var testSessionStartHook = provider.SessionStartHook{Label: "the memory", Command: `'/opt/ao' deliver '/h'`, Heading: "## Memory", Ending: "(End of memory)", Stamp: "(Delivered at "}

// testHook is a copy of testSessionStartHook that runs at a resume when
// resume is set.
func testHook(resume bool) *provider.SessionStartHook {
	hook := testSessionStartHook
	hook.Resume = resume
	return &hook
}

func trustHash(command, matcher string) string {
	return newSessionStartHandler(provider.SessionStartHook{Command: command}).trustHash(matcher)
}

// The vector is what `hooks/list` reported as currentHash for this command
// in a SessionStart group of matcher "startup|compact|clear" and timeout 60,
// on codex-cli 0.157.1 (docs/references/codex-wire.md, SessionStart hooks).
// The command holds every byte encoding/json and serde_json write
// differently, and the characters both escape.
func TestSessionStartHookTrustHashMatchesCodex(t *testing.T) {
	t.Parallel()
	const command = "'/opt/a o/it'\\''s \"q\"\\\\ <a>&b é\u2028\U0001F600'\t\x01\x7f\n __agent-home-context 'MEMORY.md' '/h'"
	if got, want := trustHash(command, provider.SessionStartHookSources), "sha256:5cfcd58168640bf35ec346e3d67278caecbb107f6d08dd0f693c088e6e67d886"; got != want {
		t.Fatalf("trust hash = %s, want %s", got, want)
	}
	// The group's matcher is part of the identity: a hook registered for a
	// resume is trusted by its own hash.
	if trustHash(command, provider.SessionStartResumeSources) == trustHash(command, provider.SessionStartHookSources) {
		t.Fatal("the matcher does not reach the hash")
	}
}

func TestWriteSerdeJSONString(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"":                 `""`,
		`plain`:            `"plain"`,
		`"q" \ /`:          `"\"q\" \\ /"`,
		"\b\f\n\r\t":       `"\b\f\n\r\t"`,
		"\x00\x01\x1f\x0b": `"\u0000\u0001\u001f\u000b"`,
		"<a>&b \x7f":       "\"<a>&b \x7f\"",
		"é\u2028\u2029":    "\"é\u2028\u2029\"",
		"\U0001F600":       "\"\U0001F600\"",
	} {
		var b strings.Builder
		writeSerdeJSONString(&b, text)
		if b.String() != want {
			t.Errorf("writeSerdeJSONString(%q) = %q, want %q", text, b.String(), want)
		}
	}
}

func sessionStartHooksJSON(t *testing.T, hooks any) string {
	t.Helper()
	data, err := json.Marshal(hooks)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// wantSessionStartHook is the `hooks` config registering
// testSessionStartHook in a group of its own, whose matcher is the resume
// one when resume is set.
func wantSessionStartHook(t *testing.T, resume bool) string {
	t.Helper()
	matcher := provider.SessionStartMatcher(resume)
	return sessionStartHooksJSON(t, map[string]any{
		"SessionStart": []any{map[string]any{
			"matcher": matcher,
			"hooks":   []any{map[string]any{"type": "command", "command": testSessionStartHook.Command, "timeout": 60}},
		}},
		"state": map[string]any{"/<session-flags>/config.toml:session_start:0:0": map[string]any{"trusted_hash": trustHash(testSessionStartHook.Command, matcher)}},
	})
}

// The thread config registers the hook as the command handler of a
// SessionStart group of its own and trusts it by its hash under the key
// Codex gives the session-flags layer's one handler.
func TestThreadParamsCarryTheSessionStartHook(t *testing.T) {
	t.Parallel()
	config := buildThreadParams(Config{SessionStartHook: testHook(false)}, "0.157.1")["config"].(map[string]any)
	if got, want := sessionStartHooksJSON(t, config["hooks"]), wantSessionStartHook(t, false); got != want {
		t.Fatalf("hooks = %s\nwant    %s", got, want)
	}
	if !strings.Contains(sessionStartHooksJSON(t, config["hooks"]), `"matcher":"startup|compact|clear"`) {
		t.Fatalf("hooks = %s", sessionStartHooksJSON(t, config["hooks"]))
	}
	without := buildThreadParams(Config{}, "0.157.1")["config"].(map[string]any)
	if _, ok := without["hooks"]; ok {
		t.Fatalf("a thread without the hook carries %v", without["hooks"])
	}
	if len(without) == 0 || len(config) != len(without)+1 {
		t.Fatalf("overrides with the hook %v, without %v", config, without)
	}
	for key, value := range without {
		if sessionStartHooksJSON(t, config[key]) != sessionStartHooksJSON(t, value) {
			t.Errorf("the hook changed the override %s: %v, was %v", key, config[key], value)
		}
	}

	// A load that runs the hook at its resume copies the params rather than
	// editing the ones Resume keeps.
	params := buildThreadParams(Config{SessionStartHook: testHook(false)}, "0.157.1")
	resumed := withSessionStartHook(params, testHook(false), true)
	if got, want := sessionStartHooksJSON(t, resumed["config"].(map[string]any)["hooks"]), wantSessionStartHook(t, true); got != want {
		t.Fatalf("resume hooks = %s\nwant          %s", got, want)
	}
	if !strings.Contains(sessionStartHooksJSON(t, resumed["config"].(map[string]any)["hooks"]), `"matcher":"startup|compact|clear|resume|fork"`) {
		t.Fatalf("resume hooks = %s", sessionStartHooksJSON(t, resumed["config"].(map[string]any)["hooks"]))
	}
	if got, want := sessionStartHooksJSON(t, params["config"].(map[string]any)["hooks"]), wantSessionStartHook(t, false); got != want {
		t.Fatalf("withSessionStartHook edited its input: %s", got)
	}
	if got := withSessionStartHook(params, nil, true); sessionStartHooksJSON(t, got) != sessionStartHooksJSON(t, params) {
		t.Fatalf("withSessionStartHook without a hook = %v", got)
	}
}

// receiptRecorder records the receipts a session keeps and forgets, and the
// errors it reports.
type receiptRecorder struct {
	mu        sync.Mutex
	records   []string
	errors    []string
	keepErr   error
	forgetErr error
}

func (r *receiptRecorder) receipts() *provider.SessionStartReceipts {
	return &provider.SessionStartReceipts{
		Keep: func(sessionRef, context string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.records = append(r.records, "keep "+sessionRef+" "+context)
			return r.keepErr
		},
		Forget: func() error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.records = append(r.records, "forget")
			return r.forgetErr
		},
	}
}

func (r *receiptRecorder) onEvent(evt provider.ProviderEvent) {
	if evt.Kind != provider.EventError {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, evt.Content)
}

func (r *receiptRecorder) take() (records, errs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, errs = r.records, r.errors
	r.records, r.errors = nil, nil
	return records, errs
}

// A fresh thread registers the hook for its start and expects it. A resumed
// one registers it for its resume only when its Resume is set, and expects
// it then.
func TestNewSessionRegistersTheHookTheLoadRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, resume string
		hook                 *provider.SessionStartHook
		resumeHook           bool
		expecting            provider.SessionStartEvent
	}{
		{"fresh", "thread/start", "", testHook(false), false, provider.SessionStartAfterStart},
		// A fresh thread holds no context to resume past.
		{"fresh with a resuming hook", "thread/start", "", testHook(true), false, provider.SessionStartAfterStart},
		{"resume", "thread/resume", "mock-thread", testHook(false), false, ""},
		{"resume of the hook", "thread/resume", "mock-thread", testHook(true), true, provider.SessionStartAfterResume},
		{"fresh without the hook", "thread/start", "", nil, false, ""},
		{"resume without the hook", "thread/resume", "mock-thread", nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			requestLog := filepath.Join(dir, "requests.jsonl")
			binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
			recorder := &receiptRecorder{}
			sess, err := NewSession(t.Context(), "thread-1", Config{
				Binary: binary, WorkDir: dir, ResumeThreadID: tc.resume, SessionStartHook: tc.hook, SessionStartReceipts: recorder.receipts(),
			}, recorder.onEvent)
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			config, _ := findRequestParams(t, requestLog, tc.method)["config"].(map[string]any)
			want := "null"
			if tc.hook != nil {
				want = wantSessionStartHook(t, tc.resumeHook)
			}
			if got := sessionStartHooksJSON(t, config["hooks"]); got != want {
				t.Errorf("hooks = %s\nwant    %s", got, want)
			}
			if got := sess.sessionStart.Expecting(); got != tc.expecting {
				t.Errorf("expecting = %q, want %q", got, tc.expecting)
			}
			if records, errs := recorder.take(); len(records) > 0 || len(errs) > 0 {
				t.Errorf("records %v, errors %v", records, errs)
			}
		})
	}
}

// A hook a delivery could not tell apart is refused before Codex starts.
func TestNewSessionRefusesAHookItCannotFollow(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "codex-never-run")
	_, err := NewSession(t.Context(), "thread-1", Config{
		Binary: binary, WorkDir: t.TempDir(),
		SessionStartHook: &provider.SessionStartHook{Label: "the memory", Command: "hook"},
	}, func(provider.ProviderEvent) {})
	if err == nil || !strings.Contains(err.Error(), "heading") {
		t.Fatalf("NewSession error = %v, want the refused hook", err)
	}
}

// A turn the app-server starts as it answers the resume, and the hook it
// runs, may reach the read loop before NewSession reads the answer: the
// expectation is armed before the request, so that turn settles it and
// keeps its receipt.
func TestATurnRunBeforeTheResumeAnswerSettlesTheResume(t *testing.T) {
	t.Parallel()
	const root = "codex-early-turn"
	line := func(method string, params map[string]any) string {
		params["threadId"] = root
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		// printf reads the template as its format: keep JSON's escapes.
		return strings.ReplaceAll(string(raw), `\`, `\\`) + "\n"
	}
	run := map[string]any{
		"id": "session_start:0:0", "eventName": "sessionStart", "handlerType": "command", "executionMode": "sync",
		"scope": "thread", "sourcePath": "/<session-flags>/config.toml", "source": "sessionFlags", "displayOrder": 0,
		"status": "completed", "startedAt": 1,
		"entries": []any{map[string]any{"kind": "context", "text": memoryContext}},
	}
	early := line("turn/started", map[string]any{"turn": map[string]any{"id": "t0", "status": "inProgress", "items": []any{}}}) +
		line("hook/completed", map[string]any{"turnId": "t0", "run": run}) +
		line("turn/completed", map[string]any{"turn": map[string]any{"id": "t0", "status": "completed", "items": []any{}}})
	binary := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:    `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/resume"`: early + `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"` + root + `"}}}`,
		"":                         `{"jsonrpc":"2.0","id":%s,"result":{}}`,
	})
	recorder := &receiptRecorder{}
	sess, err := NewSession(t.Context(), "thread-1", Config{
		Binary: binary, WorkDir: t.TempDir(), ResumeThreadID: root,
		SessionStartHook: testHook(true), SessionStartReceipts: recorder.receipts(),
	}, recorder.onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if got := sess.sessionStart.Expecting(); got != "" {
		t.Fatalf("still expecting %q after the turn that ran the hook", got)
	}
	records, errs := recorder.take()
	if want := []string{"keep " + root + " " + memoryContext}; strings.Join(records, "\n") != strings.Join(want, "\n") || len(errs) > 0 {
		t.Fatalf("records %q, errors %v; want records %q", records, errs, want)
	}
}

// A rehydrate is a cold load: Codex drops the hook run the earlier load
// queued, so it registers the hook for the resume while the open
// expectation still waits for it, and once the delivery settled it
// registers it as the thread's start did. The settled delivery keeps its
// receipt under the Codex thread.
func TestResumeRegistersTheHookWhileTheDeliveryWaitsForIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
	recorder := &receiptRecorder{}
	sess, err := NewSession(t.Context(), "thread-1", Config{
		Binary: binary, WorkDir: dir, SessionStartHook: testHook(false), SessionStartReceipts: recorder.receipts(),
	}, recorder.onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	turnStarted(t, sess, "t1")
	if err := sess.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	turnStarted(t, sess, "t2")
	hookRun(t, sess, "t2", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, sess, "t2", "completed")
	if err := sess.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	resumes := requestsFor(t, requestLog, "thread/resume")
	if len(resumes) != 2 {
		t.Fatalf("thread/resume requests = %d, want 2", len(resumes))
	}
	for i, resume := range []bool{true, false} {
		config, _ := resumes[i]["config"].(map[string]any)
		if got, want := sessionStartHooksJSON(t, config["hooks"]), wantSessionStartHook(t, resume); got != want {
			t.Errorf("resume %d hooks = %s\nwant       %s", i, got, want)
		}
	}
	records, errs := recorder.take()
	if want := []string{"keep mock-thread " + memoryContext}; strings.Join(records, "\n") != strings.Join(want, "\n") || len(errs) > 0 {
		t.Fatalf("records %q, errors %v; want records %q", records, errs, want)
	}
}

type sessionStartEvents struct {
	mu     sync.Mutex
	events []provider.ProviderEvent
}

func (r *sessionStartEvents) record(event provider.ProviderEvent) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *sessionStartEvents) take() []provider.ProviderEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	events := r.events
	r.events = nil
	return events
}

const sessionStartRootThread = "codex-thread-1"

func sessionStartSession(t *testing.T, expect provider.SessionStartEvent) (*Session, *sessionStartEvents) {
	t.Helper()
	return sessionStartSessionWith(t, expect, testHook(false), nil)
}

func sessionStartSessionWith(t *testing.T, expect provider.SessionStartEvent, hook *provider.SessionStartHook, receipts *provider.SessionStartReceipts) (*Session, *sessionStartEvents) {
	t.Helper()
	delivery, err := provider.NewSessionStartDelivery(hook, receipts)
	if err != nil {
		t.Fatal(err)
	}
	rec := &sessionStartEvents{}
	s := &Session{threadID: testThread, onEvent: rec.record, sessionStart: delivery}
	s.setRootThreadID(sessionStartRootThread)
	if expect != "" {
		s.sessionStart.Expect(expect)
	}
	return s, rec
}

func notify(t *testing.T, s *Session, method string, params map[string]any) {
	t.Helper()
	params["threadId"] = s.rootThreadID()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	s.dispatchNotification(method, raw)
}

func turnStarted(t *testing.T, s *Session, turn string) {
	t.Helper()
	notify(t, s, "turn/started", map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress", "items": []any{}}})
}

func turnCompleted(t *testing.T, s *Session, turn, status string) {
	t.Helper()
	notify(t, s, "turn/completed", map[string]any{"turn": map[string]any{"id": turn, "status": status, "items": []any{}}})
}

func hookRun(t *testing.T, s *Session, turn, source, status string, contexts ...string) {
	t.Helper()
	entries := []any{}
	for _, context := range contexts {
		entries = append(entries, map[string]any{"kind": "context", "text": context})
	}
	hookRunWith(t, s, turn, source, status, entries)
}

func hookRunWith(t *testing.T, s *Session, turn, source, status string, entries []any) {
	t.Helper()
	run := map[string]any{
		"id": "session_start:0:0", "eventName": "sessionStart", "handlerType": "command", "executionMode": "sync",
		"scope": "thread", "sourcePath": "/<session-flags>/config.toml", "source": source, "displayOrder": 0,
		"status": "running", "startedAt": 1, "entries": []any{},
	}
	notify(t, s, "hook/started", map[string]any{"turnId": turn, "run": run})
	completed := maps.Clone(run)
	completed["status"], completed["entries"] = status, entries
	notify(t, s, "hook/completed", map[string]any{"turnId": turn, "run": completed})
}

func compacted(t *testing.T, s *Session, turn string) {
	t.Helper()
	notify(t, s, "item/completed", map[string]any{"turnId": turn, "item": map[string]any{"type": "contextCompaction", "id": "compact-" + turn}})
}

const memoryContext = "## Memory\n\n- a\n\n(End of memory)"

// warningsBeforeCompletion are the delivery warnings among events; each must
// be non-fatal and come before the turn's completion.
func warningsBeforeCompletion(t *testing.T, events []provider.ProviderEvent) []string {
	t.Helper()
	var warnings []string
	completed := false
	for _, event := range events {
		switch {
		case event.Kind == provider.EventTurnComplete:
			completed = true
		case event.Kind == provider.EventError && strings.Contains(event.Content, "did not reach this session"):
			var meta map[string]any
			if json.Unmarshal(event.Meta, &meta) != nil || meta["fatal"] != false {
				t.Errorf("warning is not non-fatal: %s", event.Meta)
			}
			if completed {
				t.Errorf("warning after the turn completed: %q", event.Content)
			}
			warnings = append(warnings, event.Content)
		case event.Kind == provider.EventNotification && strings.Contains(event.Content, "sessionStart hook"):
			t.Errorf("AO's hook run rendered a row: %+v", event)
		}
	}
	return warnings
}

func TestCodexSettlesTheStartAtItsFirstCompletedTurn(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	events := rec.take()
	if warnings := warningsBeforeCompletion(t, events); len(warnings) > 0 || kindIndexOf(events, provider.EventError) >= 0 {
		t.Fatalf("warnings after the hook delivered: %v, events %+v", warnings, events)
	}
	if kindIndexOf(events, provider.EventTurnComplete) < 0 {
		t.Fatalf("the turn did not complete: %+v", events)
	}

	// A failed run is reported, once.
	s, rec = sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "failed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		warnings[0] != "the memory did not reach this session after its start: a SessionStart hook failed (status failed)." {
		t.Fatalf("warnings = %v", warnings)
	}
	turnStarted(t, s, "t2")
	turnCompleted(t, s, "t2", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("settled twice: %v", warnings)
	}
}

// A failed run's error entries are the cause the warning quotes.
func TestCodexQuotesAFailedRun(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRunWith(t, s, "t1", "sessionFlags", "failed", []any{
		map[string]any{"kind": "warning", "text": "not the cause"},
		map[string]any{"kind": "error", "text": "hook exited with code 127"},
		map[string]any{"kind": "error", "text": "sh: /gone/agent-overflow: not found"},
	})
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		warnings[0] != "the memory did not reach this session after its start: a SessionStart hook failed (hook exited with code 127; sh: /gone/agent-overflow: not found)." {
		t.Fatalf("warnings = %v", warnings)
	}
}

// Only the turn that gave the hook its chance settles: a review turn runs
// none, and a turn/completed of another turn is not that turn's.
func TestCodexSettlesOnlyAtTheTurnThatRanTheHook(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	// An inline review starts its private turn and completes its outer one.
	s.mu.Lock()
	s.review = &reviewRun{outerTurnID: "review-outer", responseBound: true}
	s.mu.Unlock()
	turnStarted(t, s, "review-private")
	turnCompleted(t, s, "review-outer", "completed")
	s.mu.Lock()
	review := s.review
	s.mu.Unlock()
	if review != nil {
		t.Fatal("the review did not complete")
	}
	turnStarted(t, s, "t1")
	turnCompleted(t, s, "t0", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 || s.sessionStart.Expecting() != provider.SessionStartAfterStart {
		t.Fatalf("settled early: warnings %v, expecting %q", warnings, s.sessionStart.Expecting())
	}
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 || s.sessionStart.Expecting() != "" {
		t.Fatalf("the turn that ran the hook: warnings %v, expecting %q", warnings, s.sessionStart.Expecting())
	}
}

// The turn that settles a delivery keeps its receipt under the Codex
// thread; a compaction forgets the receipt first. A receipt or a compaction
// the store could not record is a non-fatal row of the thread.
func TestCodexKeepsTheReceiptOfWhatArrived(t *testing.T) {
	t.Parallel()
	recorder := &receiptRecorder{}
	s, rec := sessionStartSessionWith(t, provider.SessionStartAfterStart, testHook(false), recorder.receipts())
	afterCompaction := strings.Replace(memoryContext, "- a", "- b", 1)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	compacted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", afterCompaction)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	records, _ := recorder.take()
	if want := []string{"forget", "keep codex-thread-1 " + afterCompaction}; strings.Join(records, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records %q, want %q", records, want)
	}

	recorder.keepErr, recorder.forgetErr = errors.New("database is locked"), errors.New("disk full")
	turnStarted(t, s, "t2")
	compacted(t, s, "t2")
	hookRun(t, s, "t2", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t2", "completed")
	var failures []string
	for _, event := range rec.take() {
		if event.Kind == provider.EventError {
			var meta map[string]any
			if json.Unmarshal(event.Meta, &meta) != nil || meta["fatal"] != false {
				t.Errorf("record failure is not non-fatal: %s", event.Meta)
			}
			failures = append(failures, event.Content)
		}
	}
	if len(failures) != 2 || !strings.HasPrefix(failures[0], "Agent Overflow could not record that this compaction") || !strings.HasSuffix(failures[0], ": disk full") ||
		!strings.HasPrefix(failures[1], "Agent Overflow could not record that the memory reached this session") {
		t.Fatalf("record failure rows = %v", failures)
	}
}

// A resume of a hook whose Resume is set expects it, and the turn that ran
// it settles the resume.
func TestCodexSettlesAResumeAtItsFirstCompletedTurn(t *testing.T) {
	t.Parallel()
	recorder := &receiptRecorder{}
	s, rec := sessionStartSessionWith(t, provider.SessionStartAfterResume, testHook(true), recorder.receipts())
	turnStarted(t, s, "t1")
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "the memory did not reach this session after its resume") {
		t.Fatalf("warnings = %v", warnings)
	}

	s, rec = sessionStartSessionWith(t, provider.SessionStartAfterResume, testHook(true), recorder.receipts())
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if records, _ := recorder.take(); len(records) != 1 || records[0] != "keep codex-thread-1 "+memoryContext {
		t.Fatalf("records = %q", records)
	}
}

// An interrupted or failed turn may stop before Codex runs the hook, which
// stays queued for the next turn, and so does the expectation.
func TestCodexKeepsTheExpectationPastAnUnfinishedTurn(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"interrupted", "failed"} {
		s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
		turnStarted(t, s, "t1")
		turnCompleted(t, s, "t1", status)
		if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 || s.sessionStart.Expecting() != provider.SessionStartAfterStart {
			t.Fatalf("%s: warned or settled at an unfinished turn: %v", status, warnings)
		}
		turnStarted(t, s, "t2")
		turnCompleted(t, s, "t2", "completed")
		if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "the memory did not reach") {
			t.Fatalf("%s: warnings = %v", status, warnings)
		}
	}
}

// A revert in place that cut a turn whose delivery an interrupt kept from
// settling drops it: the next completed turn warns and keeps no receipt,
// and a rehydrate registers the hook again.
func TestCodexDropsTheDeliveriesARevertCut(t *testing.T) {
	t.Parallel()
	recorder := &receiptRecorder{}
	s, rec := sessionStartSessionWith(t, provider.SessionStartAfterResume, testHook(true), recorder.receipts())
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "interrupted")
	rec.take()
	if s.sessionStart.Pending() {
		t.Fatal("pending after the delivery")
	}
	s.DropSessionStartDeliveries()
	if !s.sessionStart.Pending() {
		t.Fatal("not pending after the revert")
	}
	turnStarted(t, s, "t2")
	turnCompleted(t, s, "t2", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "the memory did not reach this session after its resume") {
		t.Fatalf("warnings = %v", warnings)
	}
	if records, _ := recorder.take(); len(records) != 0 {
		t.Fatalf("records = %q", records)
	}
}

// A compaction requested on its own is a turn that runs no hooks; the next
// turn runs them and settles. Inside a turn, Codex runs them right after the
// compaction, and that turn settles.
func TestCodexSettlesACompactionAtTheTurnThatRanItsHook(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, "")
	turnStarted(t, s, "compact")
	compacted(t, s, "compact")
	turnCompleted(t, s, "compact", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("the compaction turn settled: %v", warnings)
	}
	turnStarted(t, s, "t2")
	hookRun(t, s, "t2", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t2", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("warnings after the hook delivered: %v", warnings)
	}

	// The legacy notification arms the same way, and a turn without the
	// hook's run reports it.
	notify(t, s, "thread/compacted", map[string]any{"turnId": "compact-2"})
	turnStarted(t, s, "t3")
	turnCompleted(t, s, "t3", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		!strings.HasPrefix(warnings[0], "the memory did not reach this session after its compaction") {
		t.Fatalf("warnings = %v", warnings)
	}

	// Mid-turn: the hook runs after the compaction inside the same turn.
	s, rec = sessionStartSession(t, "")
	turnStarted(t, s, "t1")
	compacted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 || s.sessionStart.Expecting() != "" {
		t.Fatalf("mid-turn compaction: warnings %v, still expecting %q", warnings, s.sessionStart.Expecting())
	}
}

// A start's delivery does not count for a compaction later in the same turn:
// Codex runs the compaction's hooks after it, and that run's output is the
// one that counts.
func TestCodexCountsOnlyTheRunAfterACompaction(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	compacted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "failed")
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		warnings[0] != "the memory did not reach this session after its compaction: a SessionStart hook failed (status failed)." {
		t.Fatalf("warnings = %v", warnings)
	}
}

// Runs of the user's own SessionStart hooks keep their rows and deliver
// nothing for AO's.
func TestCodexLeavesOtherHookRunsAlone(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "user", "completed", "## The user's own hook")
	hookRun(t, s, "t1", "user", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	events := rec.take()
	rows := 0
	for _, event := range events {
		if event.Kind == provider.EventNotification && strings.Contains(event.Content, "sessionStart hook") {
			rows++
		}
	}
	if rows != 4 {
		t.Fatalf("user hook rows = %d, want 4: %+v", rows, events)
	}
	var warnings []string
	for _, event := range events {
		if event.Kind == provider.EventError {
			warnings = append(warnings, event.Content)
		}
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "the memory did not reach") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// A session without the hook leaves every hook run as a row and warns about
// nothing.
func TestCodexWithoutTheSessionStartHook(t *testing.T) {
	t.Parallel()
	rec := &sessionStartEvents{}
	s := &Session{threadID: testThread, onEvent: rec.record}
	s.setRootThreadID(sessionStartRootThread)
	turnStarted(t, s, "t1")
	compacted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	rows := 0
	for _, event := range rec.take() {
		if event.Kind == provider.EventError {
			t.Fatalf("warned without the hook: %q", event.Content)
		}
		if event.Kind == provider.EventNotification && strings.Contains(event.Content, "sessionStart hook") {
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("hook rows = %d, want 2", rows)
	}
}

func kindIndexOf(events []provider.ProviderEvent, kind provider.EventKind) int {
	for i, event := range events {
		if event.Kind == kind {
			return i
		}
	}
	return -1
}

// A subagent thread runs no SessionStart hook of its own; a run reported on
// one keeps its row on the spawn and neither arms, delivers nor settles the
// parent thread's.
func TestCodexIgnoresAChildThreadsHookRuns(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	if !s.registerChildOwnership(sessionStartRootThread, "child-thread", "/root/reviewer", "spawn-1") {
		t.Fatal("the child was not registered")
	}
	child := func(method string, params map[string]any) {
		t.Helper()
		params["threadId"] = "child-thread"
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		s.dispatchNotification(method, raw)
	}
	run := map[string]any{
		"id": "session_start:0:0", "eventName": "sessionStart", "handlerType": "command", "executionMode": "sync",
		"scope": "thread", "sourcePath": "/<session-flags>/config.toml", "source": "sessionFlags", "displayOrder": 0,
		"status": "completed", "startedAt": 1,
		"entries": []any{map[string]any{"kind": "context", "text": memoryContext}},
	}
	turnStarted(t, s, "t1")
	child("turn/started", map[string]any{"turn": map[string]any{"id": "c1", "status": "inProgress", "items": []any{}}})
	child("hook/started", map[string]any{"turnId": "c1", "run": run})
	child("hook/completed", map[string]any{"turnId": "c1", "run": run})
	child("turn/completed", map[string]any{"turn": map[string]any{"id": "c1", "status": "completed", "items": []any{}}})
	rows := 0
	for _, event := range rec.take() {
		switch {
		case event.Kind == provider.EventError:
			t.Fatalf("a child's turn settled the parent's start: %q", event.Content)
		case event.Kind == provider.EventNotification && strings.Contains(event.Content, "sessionStart hook"):
			if event.ParentToolUseID != "spawn-1" {
				t.Fatalf("the child's hook row is not on its spawn: %+v", event)
			}
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("child hook rows = %d, want 2", rows)
	}
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		!strings.HasPrefix(warnings[0], "the memory did not reach this session after its start") {
		t.Fatalf("warnings = %v", warnings)
	}
}
