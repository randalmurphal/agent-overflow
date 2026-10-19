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
)

var testSessionStartHooks = []provider.SessionStartHook{
	{Label: "instructions.md", Command: `'/opt/ao' __agent-home-context 'instructions.md' '/h'`, Heading: "## Your standing instructions"},
	{Label: "MEMORY.md", Command: `'/opt/ao' __agent-home-context 'MEMORY.md' '/h'`, Heading: "## Your memory index"},
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
	// The group's matcher is part of the identity: the hooks registered for
	// a resume are trusted by their own hash.
	if trustHash(command, sessionStartHookSourcesOwed) == trustHash(command, provider.SessionStartHookSources) {
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
// testSessionStartHooks in one group of matcher.
func wantSessionStartHooks(t *testing.T, matcher string) string {
	t.Helper()
	return sessionStartHooksJSON(t, map[string]any{
		"SessionStart": []any{map[string]any{
			"matcher": matcher,
			"hooks": []any{
				map[string]any{"type": "command", "command": testSessionStartHooks[0].Command, "timeout": 60},
				map[string]any{"type": "command", "command": testSessionStartHooks[1].Command, "timeout": 60},
			},
		}},
		"state": map[string]any{
			"/<session-flags>/config.toml:session_start:0:0": map[string]any{"trusted_hash": trustHash(testSessionStartHooks[0].Command, matcher)},
			"/<session-flags>/config.toml:session_start:0:1": map[string]any{"trusted_hash": trustHash(testSessionStartHooks[1].Command, matcher)},
		},
	})
}

// The thread config registers each hook as a command handler of one
// SessionStart group and trusts each by its hash under the key Codex gives
// the session-flags layer's handlers.
func TestThreadParamsCarryTheSessionStartHooks(t *testing.T) {
	t.Parallel()
	config := buildThreadParams(Config{SessionStartHooks: testSessionStartHooks}, "0.157.1")["config"].(map[string]any)
	if got, want := sessionStartHooksJSON(t, config["hooks"]), wantSessionStartHooks(t, "startup|compact|clear"); got != want {
		t.Fatalf("hooks = %s\nwant    %s", got, want)
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

	// A load that owes a delivery copies the params rather than editing
	// the ones Resume keeps.
	params := buildThreadParams(Config{SessionStartHooks: testSessionStartHooks}, "0.157.1")
	owed := withSessionStartHooks(params, testSessionStartHooks, true)
	if got, want := sessionStartHooksJSON(t, owed["config"].(map[string]any)["hooks"]), wantSessionStartHooks(t, "startup|compact|clear|resume"); got != want {
		t.Fatalf("owed hooks = %s\nwant         %s", got, want)
	}
	if got, want := sessionStartHooksJSON(t, params["config"].(map[string]any)["hooks"]), wantSessionStartHooks(t, "startup|compact|clear"); got != want {
		t.Fatalf("withSessionStartHooks edited its input: %s", got)
	}
}

// startRecorder records a session's SessionStart expectations and errors.
type startRecorder struct {
	mu      sync.Mutex
	records []string
	errors  []string
	fail    error
}

func (r *startRecorder) pending(after provider.SessionStartEvent) *SessionStartPending {
	return &SessionStartPending{After: after, Record: func(codexThreadID string, after provider.SessionStartEvent) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.records = append(r.records, codexThreadID+" "+string(after))
		return r.fail
	}}
}

func (r *startRecorder) onEvent(evt provider.ProviderEvent) {
	if evt.Kind != provider.EventError {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, evt.Content)
}

func (r *startRecorder) take() (records, errs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, errs = r.records, r.errors
	r.records, r.errors = nil, nil
	return records, errs
}

// A fresh thread expects the hooks from its start and records that. A
// resumed one carries their earlier output and registers them only for its
// next compaction, unless its last session left a delivery owed: then it
// registers them for the resume too and expects them again.
func TestNewSessionRegistersTheHooksForWhatTheThreadOwes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, resume string
		pending              bool
		owed                 provider.SessionStartEvent
		matcher              string
		expecting            provider.SessionStartEvent
		records              []string
	}{
		{"fresh", "thread/start", "", true, "", "startup|compact|clear", provider.SessionStartAfterStart, []string{"mock-thread start"}},
		{"fresh without a record", "thread/start", "", false, "", "startup|compact|clear", provider.SessionStartAfterStart, nil},
		{"resume", "thread/resume", "mock-thread", true, "", "startup|compact|clear", "", nil},
		{"resume owing a start", "thread/resume", "mock-thread", true, provider.SessionStartAfterStart, "startup|compact|clear|resume", provider.SessionStartAfterStart, nil},
		{"resume owing a compaction", "thread/resume", "mock-thread", true, provider.SessionStartAfterCompaction, "startup|compact|clear|resume", provider.SessionStartAfterCompaction, nil},
		// A record without hooks has nothing to follow.
		{"fresh without hooks", "thread/start", "", true, "", "", "", nil},
		{"resume without hooks", "thread/resume", "mock-thread", true, provider.SessionStartAfterStart, "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			requestLog := filepath.Join(dir, "requests.jsonl")
			binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
			recorder := &startRecorder{}
			cfg := Config{Binary: binary, WorkDir: dir, ResumeThreadID: tc.resume}
			if tc.matcher != "" {
				cfg.SessionStartHooks = testSessionStartHooks
			}
			if tc.pending {
				cfg.SessionStartPending = recorder.pending(tc.owed)
			}
			sess, err := NewSession(t.Context(), "thread-1", cfg, recorder.onEvent)
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			config, _ := findRequestParams(t, requestLog, tc.method)["config"].(map[string]any)
			want := "null"
			if tc.matcher != "" {
				want = wantSessionStartHooks(t, tc.matcher)
			}
			if got := sessionStartHooksJSON(t, config["hooks"]); got != want {
				t.Errorf("hooks = %s\nwant    %s", got, want)
			}
			if got := sess.sessionStart.Expecting(); got != tc.expecting {
				t.Errorf("expecting = %q, want %q", got, tc.expecting)
			}
			records, errs := recorder.take()
			if strings.Join(records, ",") != strings.Join(tc.records, ",") || len(errs) > 0 {
				t.Errorf("records %v, errors %v; want records %v", records, errs, tc.records)
			}
		})
	}
}

// Hooks a delivery could not tell apart are refused before Codex starts.
func TestNewSessionRefusesHooksItCannotFollow(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "codex-never-run")
	_, err := NewSession(t.Context(), "thread-1", Config{
		Binary: binary, WorkDir: t.TempDir(),
		SessionStartHooks: []provider.SessionStartHook{{Label: "MEMORY.md", Command: "hook"}},
	}, func(provider.ProviderEvent) {})
	if err == nil || !strings.Contains(err.Error(), "heading") {
		t.Fatalf("NewSession error = %v, want the refused hook", err)
	}
}

// A rehydrate is a cold load: while a delivery is still expected it
// registers the hooks for the resume, and once the delivery settled it
// registers them as the thread's start did.
func TestResumeRegistersTheHooksWhileADeliveryIsOwed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
	recorder := &startRecorder{}
	sess, err := NewSession(t.Context(), "thread-1", Config{
		Binary: binary, WorkDir: dir, SessionStartHooks: testSessionStartHooks, SessionStartPending: recorder.pending(""),
	}, recorder.onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	turnStarted(t, sess, "t1")
	hookRun(t, sess, "t1", "sessionFlags", "completed", instructionsContext)
	hookRun(t, sess, "t1", "sessionFlags", "completed", memoryContext)
	turnCompleted(t, sess, "t1", "completed")
	if err := sess.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	resumes := requestsFor(t, requestLog, "thread/resume")
	if len(resumes) != 2 {
		t.Fatalf("thread/resume requests = %d, want 2", len(resumes))
	}
	for i, matcher := range []string{"startup|compact|clear|resume", "startup|compact|clear"} {
		config, _ := resumes[i]["config"].(map[string]any)
		if got, want := sessionStartHooksJSON(t, config["hooks"]), wantSessionStartHooks(t, matcher); got != want {
			t.Errorf("resume %d hooks = %s\nwant       %s", i, got, want)
		}
	}
	if records, errs := recorder.take(); strings.Join(records, ",") != "mock-thread start,mock-thread " || len(errs) > 0 {
		t.Fatalf("records %v, errors %v", records, errs)
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
	delivery, err := provider.NewSessionStartDelivery(testSessionStartHooks)
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
	instructionsContext = "## Your standing instructions (instructions.md in your home)\n\nRole"
	memoryContext       = "## Your memory index (MEMORY.md in your home)\n\n- a"
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
		warnings[0] != "instructions.md and MEMORY.md did not reach this session after its start: a SessionStart hook failed (status failed)." {
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
		warnings[0] != "MEMORY.md did not reach this session after its start: a SessionStart hook failed (hook exited with code 127; sh: /gone/agent-overflow: not found)." {
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

// Each expectation is recorded as it opens and cleared as it settles; a
// record that fails is put on the thread.
func TestCodexRecordsTheOpenExpectation(t *testing.T) {
	t.Parallel()
	s, rec := sessionStartSession(t, "")
	recorder := &startRecorder{}
	s.sessionStartRecord = recorder.pending("").Record
	s.expectSessionStart()
	turnStarted(t, s, "t1")
	compacted(t, s, "t1")
	turnCompleted(t, s, "t1", "completed")
	turnStarted(t, s, "t2")
	hookRun(t, s, "t2", "sessionFlags", "completed", instructionsContext, memoryContext)
	turnCompleted(t, s, "t2", "completed")
	rec.take()
	if records, _ := recorder.take(); strings.Join(records, ",") != "codex-thread-1 start,codex-thread-1 compaction,codex-thread-1 " {
		t.Fatalf("records = %v", records)
	}

	recorder.fail = errors.New("disk full")
	notify(t, s, "thread/compacted", map[string]any{"turnId": "t3"})
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
	if len(failures) != 1 || !strings.Contains(failures[0], "could not record") || !strings.HasSuffix(failures[0], ": disk full") {
		t.Fatalf("record failure rows = %v", failures)
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
		if warnings := warningsBeforeCompletion(t, rec.take()); len(warnings) != 1 || !strings.HasPrefix(warnings[0], "instructions.md did not reach") {
			t.Fatalf("%s: warnings = %v", status, warnings)
		}
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
		!strings.HasPrefix(warnings[0], "instructions.md and MEMORY.md did not reach this session after its compaction") {
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
		!strings.HasPrefix(warnings[0], "instructions.md did not reach this session after its compaction") {
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
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "instructions.md and MEMORY.md did not reach") {
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
		!strings.HasPrefix(warnings[0], "instructions.md and MEMORY.md did not reach this session after its start") {
		t.Fatalf("warnings = %v", warnings)
	}
}
