package codex

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
)

var testSessionStartHooks = []provider.SessionStartHook{
	{Label: "first hook", Command: `'/opt/ao' deliver 'first' '/h'`, Heading: "## First hook", Ending: "[End of first hook]", Stamp: "[Delivered at "},
	{Label: "second hook", Command: `'/opt/ao' deliver 'second' '/h'`, Heading: "## Second hook", Ending: "[End of second hook]", Stamp: "[Delivered at "},
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

// wantSessionStartHooks is the `hooks` config registering
// testSessionStartHooks, each in a group of its own whose matcher is the
// resume one where resume marks it.
func wantSessionStartHooks(t *testing.T, resume ...bool) string {
	t.Helper()
	groups := []any{}
	state := map[string]any{}
	for i, hook := range testSessionStartHooks {
		matcher := provider.SessionStartMatcher(i < len(resume) && resume[i])
		groups = append(groups, map[string]any{
			"matcher": matcher,
			"hooks":   []any{map[string]any{"type": "command", "command": hook.Command, "timeout": 60}},
		})
		state["/<session-flags>/config.toml:session_start:"+strconv.Itoa(i)+":0"] = map[string]any{"trusted_hash": trustHash(hook.Command, matcher)}
	}
	return sessionStartHooksJSON(t, map[string]any{"SessionStart": groups, "state": state})
}

// The thread config registers each hook as the command handler of a
// SessionStart group of its own and trusts each by its hash under the key
// Codex gives the session-flags layer's handlers.
func TestThreadParamsCarryTheSessionStartHooks(t *testing.T) {
	t.Parallel()
	config := buildThreadParams(Config{SessionStartHooks: testSessionStartHooks}, "0.157.1")["config"].(map[string]any)
	if got, want := sessionStartHooksJSON(t, config["hooks"]), wantSessionStartHooks(t); got != want {
		t.Fatalf("hooks = %s\nwant    %s", got, want)
	}
	if !strings.Contains(sessionStartHooksJSON(t, config["hooks"]), `"matcher":"startup|compact|clear"`) {
		t.Fatalf("hooks = %s", sessionStartHooksJSON(t, config["hooks"]))
	}
	if trustHash(testSessionStartHooks[0].Command, provider.SessionStartHookSources) == trustHash(testSessionStartHooks[1].Command, provider.SessionStartHookSources) {
		t.Fatal("two commands share a hash")
	}
	without := buildThreadParams(Config{}, "0.157.1")["config"].(map[string]any)
	if _, ok := without["hooks"]; ok {
		t.Fatalf("a thread without hooks carries %v", without["hooks"])
	}
	if len(without) == 0 || len(config) != len(without)+1 {
		t.Fatalf("overrides with hooks %v, without %v", config, without)
	}
	for key, value := range without {
		if sessionStartHooksJSON(t, config[key]) != sessionStartHooksJSON(t, value) {
			t.Errorf("the hooks changed the override %s: %v, was %v", key, config[key], value)
		}
	}

	// A load that runs a hook at its resume copies the params rather than
	// editing the ones Resume keeps.
	params := buildThreadParams(Config{SessionStartHooks: testSessionStartHooks}, "0.157.1")
	resumed := withSessionStartHooks(params, testSessionStartHooks, []bool{false, true})
	if got, want := sessionStartHooksJSON(t, resumed["config"].(map[string]any)["hooks"]), wantSessionStartHooks(t, false, true); got != want {
		t.Fatalf("resume hooks = %s\nwant          %s", got, want)
	}
	if !strings.Contains(sessionStartHooksJSON(t, resumed["config"].(map[string]any)["hooks"]), `"matcher":"startup|compact|clear|resume|fork"`) {
		t.Fatalf("resume hooks = %s", sessionStartHooksJSON(t, resumed["config"].(map[string]any)["hooks"]))
	}
	if got, want := sessionStartHooksJSON(t, params["config"].(map[string]any)["hooks"]), wantSessionStartHooks(t); got != want {
		t.Fatalf("withSessionStartHooks edited its input: %s", got)
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
		Keep: func(sessionRef, label, context string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.records = append(r.records, "keep "+sessionRef+" "+label+" "+context)
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

// A fresh thread registers every hook for its start and expects them all. A
// resumed one registers for its resume only the hooks whose Resume is set,
// and expects those.
func TestNewSessionRegistersTheHooksTheLoadRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, resume string
		hooks                []provider.SessionStartHook
		resumeHooks          []bool
		expecting            provider.SessionStartEvent
	}{
		{"fresh", "thread/start", "", testSessionStartHooks, nil, provider.SessionStartAfterStart},
		// A fresh thread holds no context to resume past.
		{"fresh with a resuming hook", "thread/start", "", hooksResumingAt(1), nil, provider.SessionStartAfterStart},
		{"resume", "thread/resume", "mock-thread", testSessionStartHooks, nil, ""},
		{"resume of a hook", "thread/resume", "mock-thread", hooksResumingAt(1), []bool{false, true}, provider.SessionStartAfterResume},
		{"resume of both", "thread/resume", "mock-thread", hooksResumingAt(0, 1), []bool{true, true}, provider.SessionStartAfterResume},
		{"fresh without hooks", "thread/start", "", nil, nil, ""},
		{"resume without hooks", "thread/resume", "mock-thread", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			requestLog := filepath.Join(dir, "requests.jsonl")
			binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
			recorder := &receiptRecorder{}
			sess, err := NewSession(t.Context(), "thread-1", Config{
				Binary: binary, WorkDir: dir, ResumeThreadID: tc.resume, SessionStartHooks: tc.hooks, SessionStartReceipts: recorder.receipts(),
			}, recorder.onEvent)
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			config, _ := findRequestParams(t, requestLog, tc.method)["config"].(map[string]any)
			want := "null"
			if tc.hooks != nil {
				want = wantSessionStartHooks(t, tc.resumeHooks...)
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

// hooksResumingAt are the test hooks, those at indexes running at a resume.
func hooksResumingAt(indexes ...int) []provider.SessionStartHook {
	hooks := append([]provider.SessionStartHook{}, testSessionStartHooks...)
	for _, i := range indexes {
		hooks[i].Resume = true
	}
	return hooks
}

// Hooks a delivery could not tell apart are refused before Codex starts.
func TestNewSessionRefusesHooksItCannotFollow(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "codex-never-run")
	_, err := NewSession(t.Context(), "thread-1", Config{
		Binary: binary, WorkDir: t.TempDir(),
		SessionStartHooks: []provider.SessionStartHook{{Label: "second hook", Command: "hook"}},
	}, func(provider.ProviderEvent) {})
	if err == nil || !strings.Contains(err.Error(), "heading") {
		t.Fatalf("NewSession error = %v, want the refused hook", err)
	}
}

// A turn the app-server starts as it answers the resume, and the hooks it
// runs, may reach the read loop before NewSession reads the answer: the
// expectation is armed before the request, so that turn settles it and
// keeps its receipts.
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
		"entries": []any{map[string]any{"kind": "context", "text": instructionsContext}, map[string]any{"kind": "context", "text": memoryContext}},
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
		SessionStartHooks: hooksResumingAt(0, 1), SessionStartReceipts: recorder.receipts(),
	}, recorder.onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if got := sess.sessionStart.Expecting(); got != "" {
		t.Fatalf("still expecting %q after the turn that ran the hooks", got)
	}
	records, errs := recorder.take()
	if want := []string{"keep " + root + " first hook " + instructionsContext, "keep " + root + " second hook " + memoryContext}; strings.Join(records, "\n") != strings.Join(want, "\n") || len(errs) > 0 {
		t.Fatalf("records %q, errors %v; want records %q", records, errs, want)
	}
}

// A rehydrate is a cold load: Codex drops the hooks the earlier load
// queued, so it registers for the resume the hooks the open expectation
// still waits for, and once the delivery settled it registers them as the
// thread's start did. The settled delivery keeps its receipts under the
// Codex thread.
func TestResumeRegistersTheHooksTheDeliveryStillWaitsFor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
	recorder := &receiptRecorder{}
	sess, err := NewSession(t.Context(), "thread-1", Config{
		Binary: binary, WorkDir: dir, SessionStartHooks: testSessionStartHooks, SessionStartReceipts: recorder.receipts(),
	}, recorder.onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	turnStarted(t, sess, "t1")
	hookRun(t, sess, "t1", "sessionFlags", "completed", instructionsContext)
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
	for i, resume := range [][]bool{{false, true}, nil} {
		config, _ := resumes[i]["config"].(map[string]any)
		if got, want := sessionStartHooksJSON(t, config["hooks"]), wantSessionStartHooks(t, resume...); got != want {
			t.Errorf("resume %d hooks = %s\nwant       %s", i, got, want)
		}
	}
	records, errs := recorder.take()
	if want := []string{"keep mock-thread first hook " + instructionsContext, "keep mock-thread second hook " + memoryContext}; strings.Join(records, "\n") != strings.Join(want, "\n") || len(errs) > 0 {
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
	return sessionStartSessionWith(t, expect, testSessionStartHooks, nil)
}

func sessionStartSessionWith(t *testing.T, expect provider.SessionStartEvent, hooks []provider.SessionStartHook, receipts *provider.SessionStartReceipts) (*Session, *sessionStartEvents) {
	t.Helper()
	delivery, err := provider.NewSessionStartDelivery(hooks, receipts)
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

const (
	instructionsContext = "## First hook (first hook in your home)\n\nRole\n\n[End of first hook]"
	memoryContext       = "## Second hook (second hook in your home)\n\n- a\n\n[End of second hook]"
)

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
	hookRun(t, s, "t1", "sessionFlags", "completed", instructionsContext)
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	events := rec.take()
	if warnings := warningsBeforeCompletion(t, events); len(warnings) > 0 {
		t.Fatalf("warnings after both delivered: %v", warnings)
	}
	if kindIndexOf(events, provider.EventTurnComplete) < 0 {
		t.Fatalf("the turn did not complete: %+v", events)
	}

	// A failed hook and a missing one are each reported, once.
	s, rec = sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "failed", instructionsContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		warnings[0] != "first hook and second hook did not reach this session after its start: a SessionStart hook failed (status failed)." {
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
	hookRun(t, s, "t1", "sessionFlags", "completed", instructionsContext)
	hookRunWith(t, s, "t1", "sessionFlags", "failed", []any{
		map[string]any{"kind": "warning", "text": "not the cause"},
		map[string]any{"kind": "error", "text": "hook exited with code 127"},
		map[string]any{"kind": "error", "text": "sh: /gone/agent-overflow: not found"},
	})
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		warnings[0] != "second hook did not reach this session after its start: a SessionStart hook failed (hook exited with code 127; sh: /gone/agent-overflow: not found)." {
		t.Fatalf("warnings = %v", warnings)
	}
}

// Only the turn that gave the hooks their chance settles: a review turn
// runs none, and a turn/completed of another turn is not that turn's.
func TestCodexSettlesOnlyAtTheTurnThatRanTheHooks(t *testing.T) {
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
	hookRun(t, s, "t1", "sessionFlags", "completed", instructionsContext)
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 || s.sessionStart.Expecting() != "" {
		t.Fatalf("the turn that ran the hooks: warnings %v, expecting %q", warnings, s.sessionStart.Expecting())
	}
}

// The turn that settles a delivery keeps its receipts under the Codex
// thread; a compaction forgets the receipts first. A receipt or a compaction
// the store could not record is a non-fatal row of the thread.
func TestCodexKeepsTheReceiptsOfWhatArrived(t *testing.T) {
	t.Parallel()
	recorder := &receiptRecorder{}
	s, rec := sessionStartSessionWith(t, provider.SessionStartAfterStart, testSessionStartHooks, recorder.receipts())
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", instructionsContext, memoryContext)
	compacted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "first hook did not reach this session after its compaction") {
		t.Fatalf("warnings = %v", warnings)
	}
	records, _ := recorder.take()
	if want := []string{"forget", "keep codex-thread-1 second hook " + memoryContext}; strings.Join(records, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records %q, want %q", records, want)
	}

	recorder.keepErr, recorder.forgetErr = errors.New("database is locked"), errors.New("disk full")
	turnStarted(t, s, "t2")
	compacted(t, s, "t2")
	hookRun(t, s, "t2", "sessionFlags", "completed", instructionsContext, memoryContext)
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
		!strings.HasPrefix(failures[1], "Agent Overflow could not record that first hook and second hook reached this session") {
		t.Fatalf("record failure rows = %v", failures)
	}
}

// A resume expects only the hooks it registered, and the turn that ran them
// settles it.
func TestCodexSettlesAResumeAtItsFirstCompletedTurn(t *testing.T) {
	t.Parallel()
	recorder := &receiptRecorder{}
	s, rec := sessionStartSessionWith(t, provider.SessionStartAfterResume, hooksResumingAt(1), recorder.receipts())
	turnStarted(t, s, "t1")
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "second hook did not reach this session after its resume") {
		t.Fatalf("warnings = %v", warnings)
	}

	s, rec = sessionStartSessionWith(t, provider.SessionStartAfterResume, hooksResumingAt(1), recorder.receipts())
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if records, _ := recorder.take(); len(records) != 1 || records[0] != "keep codex-thread-1 second hook "+memoryContext {
		t.Fatalf("records = %q", records)
	}
}

// An interrupted or failed turn may stop before Codex runs the hooks, which
// stay queued for the next turn, and so does the expectation.
func TestCodexKeepsTheExpectationPastAnUnfinishedTurn(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"interrupted", "failed"} {
		s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
		turnStarted(t, s, "t1")
		turnCompleted(t, s, "t1", status)
		if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
			t.Fatalf("%s: warned at an unfinished turn: %v", status, warnings)
		}
		turnStarted(t, s, "t2")
		hookRun(t, s, "t2", "sessionFlags", "completed", memoryContext)
		turnCompleted(t, s, "t2", "completed")
		if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "first hook did not reach") {
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
	s, rec := sessionStartSessionWith(t, provider.SessionStartAfterResume, hooksResumingAt(1), recorder.receipts())
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "interrupted")
	rec.take()
	s.DropSessionStartDeliveries()
	if pending := s.sessionStart.Pending(); len(pending) != 2 || !pending[1] {
		t.Fatalf("pending after the revert = %v", pending)
	}
	turnStarted(t, s, "t2")
	turnCompleted(t, s, "t2", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "second hook did not reach this session after its resume") {
		t.Fatalf("warnings = %v", warnings)
	}
	if records, _ := recorder.take(); len(records) != 0 {
		t.Fatalf("records = %q", records)
	}
}

// A compaction requested on its own is a turn that runs no hooks; the next
// turn runs them and settles. Inside a turn, Codex runs them right after the
// compaction, and that turn settles.
func TestCodexSettlesACompactionAtTheTurnThatRanItsHooks(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, "")
	turnStarted(t, s, "compact")
	compacted(t, s, "compact")
	turnCompleted(t, s, "compact", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("the compaction turn settled: %v", warnings)
	}
	turnStarted(t, s, "t2")
	hookRun(t, s, "t2", "sessionFlags", "completed", instructionsContext)
	hookRun(t, s, "t2", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t2", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 {
		t.Fatalf("warnings after both delivered: %v", warnings)
	}

	// The legacy notification arms the same way, and a turn without the
	// hooks reports them.
	notify(t, s, "thread/compacted", map[string]any{"turnId": "compact-2"})
	turnStarted(t, s, "t3")
	turnCompleted(t, s, "t3", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		!strings.HasPrefix(warnings[0], "first hook and second hook did not reach this session after its compaction") {
		t.Fatalf("warnings = %v", warnings)
	}

	// Mid-turn: the hooks run after the compaction inside the same turn.
	s, rec = sessionStartSession(t, "")
	turnStarted(t, s, "t1")
	compacted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", instructionsContext)
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) > 0 || s.sessionStart.Expecting() != "" {
		t.Fatalf("mid-turn compaction: warnings %v, still expecting %q", warnings, s.sessionStart.Expecting())
	}
}

// A start's delivery does not count for a compaction later in the same turn:
// Codex runs the compaction's hooks after it.
func TestCodexCountsOnlyTheHooksAfterACompaction(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", instructionsContext)
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	compacted(t, s, "t1")
	hookRun(t, s, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, s, "t1", "completed")
	if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 ||
		!strings.HasPrefix(warnings[0], "first hook did not reach this session after its compaction") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// Runs of the user's own SessionStart hooks keep their rows and deliver
// nothing for AO's.
func TestCodexLeavesOtherHookRunsAlone(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, provider.SessionStartAfterStart)
	turnStarted(t, s, "t1")
	hookRun(t, s, "t1", "user", "completed", instructionsContext)
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
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "first hook and second hook did not reach") {
		t.Fatalf("warnings = %v", warnings)
	}
}

// A session without hooks leaves every hook run as a row and warns about
// nothing.
func TestCodexWithoutSessionStartHooks(t *testing.T) {
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
			t.Fatalf("warned without hooks: %q", event.Content)
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

// A subagent thread runs no SessionStart hooks of its own; a run reported on
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
		"entries": []any{map[string]any{"kind": "context", "text": instructionsContext}, map[string]any{"kind": "context", "text": memoryContext}},
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
		!strings.HasPrefix(warnings[0], "first hook and second hook did not reach this session after its start") {
		t.Fatalf("warnings = %v", warnings)
	}
}
