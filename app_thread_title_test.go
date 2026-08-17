package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
	"agent-overflow/internal/triage"
)

// waitForThreadTitle polls the stored title until it matches want. The
// title lands from a detached goroutine, so a poll is the only honest
// way to observe it without wiring an event channel into every test.
func waitForThreadTitle(t *testing.T, app *App, threadID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, err := app.store.GetThread(threadID)
		if err != nil {
			t.Fatalf("GetThread() error = %v", err)
		}
		if stored.Title == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored title = %q, want %q", stored.Title, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// threadTitleEvents records the two frames a title generation emits, in
// order. Every generation is detached now — auto, heal, and the async
// regeneration RPC — so the completion frame is what a test waits on,
// and the recorded order is what proves the patch preceded it.
type threadTitleEvents struct {
	mu      sync.Mutex
	names   []string
	updates []triage.ThreadUpdateEvent
	done    []ThreadTitleGenerationEvent
}

// captureThreadTitleEvents installs the recorder over app.emitEventFn.
func captureThreadTitleEvents(t *testing.T, app *App) *threadTitleEvents {
	t.Helper()
	events := &threadTitleEvents{}
	app.emitEventFn = func(name string, data any) {
		events.mu.Lock()
		defer events.mu.Unlock()
		switch name {
		case "thread:updated":
			update, ok := data.(triage.ThreadUpdateEvent)
			if !ok {
				t.Errorf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", data)
				return
			}
			events.names = append(events.names, name)
			events.updates = append(events.updates, update)
		case "thread:title_generation":
			completion, ok := data.(ThreadTitleGenerationEvent)
			if !ok {
				t.Errorf("thread:title_generation payload type = %T, want ThreadTitleGenerationEvent", data)
				return
			}
			events.names = append(events.names, name)
			events.done = append(events.done, completion)
		}
	}
	return events
}

func (e *threadTitleEvents) order() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.names)
}

func (e *threadTitleEvents) titleUpdates() []triage.ThreadUpdateEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.updates)
}

func (e *threadTitleEvents) completionCountFor(threadID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, completion := range e.done {
		if completion.ThreadID == threadID {
			count++
		}
	}
	return count
}

func (e *threadTitleEvents) completionFor(threadID string) (ThreadTitleGenerationEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, completion := range e.done {
		if completion.ThreadID == threadID {
			return completion, true
		}
	}
	return ThreadTitleGenerationEvent{}, false
}

// waitForTitleGenerationEvent blocks until the thread's generation
// goroutine reports it finished, whatever the outcome. Every path emits
// exactly one, so this is the end of the run for assertion purposes.
func waitForTitleGenerationEvent(t *testing.T, events *threadTitleEvents, threadID string) ThreadTitleGenerationEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if completion, ok := events.completionFor(threadID); ok {
			return completion
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for thread:title_generation for %s", threadID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMaybeGenerateThreadTitleAppliesGeneratedTitleAndEmits covers the happy
// path of maybeGenerateThreadTitleWithAttachments → generatedThreadTitle →
// applyThreadTitleIfCurrent. The thread title advances from the default to
// the generated value, thread:updated is emitted so the frontend sidebar
// refreshes, and the completion frame lands after it.
func TestMaybeGenerateThreadTitleAppliesGeneratedTitleAndEmits(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-happy")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(got store.Thread, message string, _ []store.Attachment) (string, error) {
		if got.ID != thread.ID {
			t.Fatalf("generate called with thread %q, want %q", got.ID, thread.ID)
		}
		if message != "fix the reconnect bug" {
			t.Fatalf("generate message = %q, want first user turn", message)
		}
		return "Reconnect spinner fix", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "fix the reconnect bug", nil, false)

	completion := waitForTitleGenerationEvent(t, events, thread.ID)
	if completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	updates := events.titleUpdates()
	if len(updates) != 1 {
		t.Fatalf("thread:updated emitted %d times, want 1", len(updates))
	}
	if updates[0].Action != "patch" {
		t.Fatalf("expected patch action, got %q", updates[0].Action)
	}
	if updates[0].ID != thread.ID {
		t.Fatalf("updated threadID = %q, want %q", updates[0].ID, thread.ID)
	}
	if updates[0].Title == nil || *updates[0].Title != "Reconnect spinner fix" {
		t.Fatalf("updated title = %v", updates[0].Title)
	}
	if order := events.order(); !slices.Equal(order, []string{"thread:updated", "thread:title_generation"}) {
		t.Fatalf("event order = %v, want the patch before the completion", order)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Reconnect spinner fix" {
		t.Fatalf("stored title = %q", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleGuardsAgainstConcurrentRuns: two rapid
// sends on a still-default thread must cost ONE generation. Without the
// claim each send fans out its own goroutine of up to two 3-minute CLI
// attempts.
func TestMaybeGenerateThreadTitleGuardsAgainstConcurrentRuns(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-guard")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var calls atomic.Int64
	release := make(chan struct{})
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls.Add(1)
		<-release
		return "Only generation", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "first send", nil, false)
	// Wait for the first run to be inside the seam so the second send
	// races the claim rather than the goroutine's scheduling.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first generation never started")
		}
		time.Sleep(time.Millisecond)
	}
	app.maybeGenerateThreadTitleWithAttachments(thread, "second send", nil, false)
	close(release)

	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	waitForThreadTitle(t, app, thread.ID, "Only generation")
	if got := calls.Load(); got != 1 {
		t.Fatalf("generateThreadTitleFn called %d times, want 1 (in-flight guard)", got)
	}
	// The claim is released after the run, so a later send can still heal.
	if !app.claimThreadTitleGeneration(thread.ID) {
		t.Fatal("claim still held after the generation finished")
	}
	app.releaseThreadTitleGeneration(thread.ID)
}

// TestMaybeGenerateThreadTitleRetriesAfterAFailedRun pins the claim's
// RELEASE transition, which the heal depends on: a failed generation must
// free the thread so the NEXT send can re-claim and retry. Holding the
// claim across a failure would make the very failure the auto-heal exists
// for (provider down on the first turn) permanent.
func TestMaybeGenerateThreadTitleRetriesAfterAFailedRun(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-retry")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var calls atomic.Int64
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		if calls.Add(1) == 1 {
			return "", errors.New("claude CLI failed: transient outage")
		}
		return "Second try title", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "first send", nil, false)
	first := waitForTitleGenerationEvent(t, events, thread.ID)
	if first.Error != "provider CLI failed" {
		t.Fatalf("first completion error = %q, want redacted CLI failure", first.Error)
	}

	app.maybeGenerateThreadTitleWithAttachments(thread, "second send", nil, false)
	waitForThreadTitle(t, app, thread.ID, "Second try title")
	if got := calls.Load(); got != 2 {
		t.Fatalf("generateThreadTitleFn called %d times, want 2 (failed run must release the claim)", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for events.completionCountFor(thread.ID) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("completion events = %d, want 2 (one per run)", events.completionCountFor(thread.ID))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRegenerateThreadTitleJoinsAnAutoGenerationRun pins the CROSS-BODY
// join: a click while a send-path (auto) generation is live must not start
// a second run, and the auto run's own completion frame is what answers
// the click — one generation, exactly one completion event.
func TestRegenerateThreadTitleJoinsAnAutoGenerationRun(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-cross-join")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var autoCalls, regenCalls atomic.Int64
	release := make(chan struct{})
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		autoCalls.Add(1)
		<-release
		return "Auto title", nil
	}
	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		regenCalls.Add(1)
		return "Regen title", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "first send", nil, false)
	deadline := time.Now().Add(2 * time.Second)
	for autoCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("auto generation never started")
		}
		time.Sleep(time.Millisecond)
	}

	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v, want nil (joined the live run)", err)
	}
	close(release)

	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	waitForThreadTitle(t, app, thread.ID, "Auto title")
	if got := regenCalls.Load(); got != 0 {
		t.Fatalf("regenerateThreadTitleFn called %d times, want 0 (joined caller must not start a run)", got)
	}
	if got := autoCalls.Load(); got != 1 {
		t.Fatalf("generateThreadTitleFn called %d times, want 1", got)
	}
	// Settle briefly: a second completion frame would arrive from a leaked
	// second goroutine, and the joined click must not produce one.
	time.Sleep(50 * time.Millisecond)
	if got := events.completionCountFor(thread.ID); got != 1 {
		t.Fatalf("completion events = %d, want exactly 1", got)
	}
}

// TestMaybeGenerateThreadTitleRunsForCodexThread enforces t3-code parity:
// automatic thread titles use the configured text-generation provider and
// must not be gated by the chat thread's provider.
func TestMaybeGenerateThreadTitleRunsForCodexThread(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-codex")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls <- struct{}{}
		return "Codex generated title", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "fix the reconnect bug", nil, false)

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("generateThreadTitleFn not called for Codex thread")
	}

	waitForTitleGenerationEvent(t, events, thread.ID)
	updates := events.titleUpdates()
	if len(updates) != 1 || updates[0].Title == nil || *updates[0].Title != "Codex generated title" {
		t.Fatalf("thread:updated payloads = %+v", updates)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Codex generated title" {
		t.Fatalf("stored title = %q", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleHealsOnLaterSend is the auto-heal
// contract: a thread whose first-turn generation failed still carries
// the default title, so the NEXT send retries rather than leaving it
// "New Thread" forever. The title-is-custom guard (below) is what keeps
// that from re-titling threads the user already named.
//
// The heal reads the whole conversation rather than titling the one
// message that happened to be sent: the thread HAS history by then, and
// the first-turn prompt would name the tangent instead of the thread.
func TestMaybeGenerateThreadTitleHealsOnLaterSend(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-prior")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// A prior turn already exists — the old "first turn only" gate would
	// have skipped generation here.
	if err := app.store.InsertItem(store.Item{
		ID: "user:0", ThreadID: thread.ID, TurnIndex: 0, Kind: "user_text",
		Role: "user", Status: "completed", Summary: "the first ask",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertItem() error = %v", err)
	}

	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		t.Error("heal must not use the first-turn prompt on a thread with history")
		return "", errors.New("wrong path")
	}
	contexts := make(chan string, 1)
	app.regenerateThreadTitleFn = func(_ store.Thread, threadContext string) (string, error) {
		contexts <- threadContext
		return "Healed title", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "another user message", nil, true)

	select {
	case threadContext := <-contexts:
		if !strings.Contains(threadContext, "the first ask") {
			t.Fatalf("heal context = %q, want the thread's own history", threadContext)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("regenerateThreadTitleFn not called on a later send of a still-default thread")
	}

	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	waitForThreadTitle(t, app, thread.ID, "Healed title")
}

// TestMaybeGenerateThreadTitleHealFallsBackWithoutContext: a thread
// whose rows are all tool traffic renders no transcript, so the heal
// falls back to the message in hand rather than leaving the thread
// "New Thread".
func TestMaybeGenerateThreadTitleHealFallsBackWithoutContext(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-heal-fallback")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID: "tool:0", ThreadID: thread.ID, TurnIndex: 0, Kind: "tool_call",
		Role: "assistant", Status: "completed", Summary: "ran a tool",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertItem() error = %v", err)
	}

	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		t.Error("no renderable history exists — the regeneration prompt has nothing to read")
		return "", errors.New("wrong path")
	}
	messages := make(chan string, 1)
	app.generateThreadTitleFn = func(_ store.Thread, message string, _ []store.Attachment) (string, error) {
		messages <- message
		return "Fallback title", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "another user message", nil, true)

	select {
	case message := <-messages:
		if message != "another user message" {
			t.Fatalf("fallback message = %q, want the latest send", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("generateThreadTitleFn not called as the heal fallback")
	}
	waitForTitleGenerationEvent(t, events, thread.ID)
	waitForThreadTitle(t, app, thread.ID, "Fallback title")
}

// TestMaybeGenerateThreadTitleCASesAgainstStoredBytes: the default-title
// gate trims, so a padded stored title reaches the generation — and the
// compare-and-swap matches the stored bytes exactly, so it must swap
// against those rather than against the Default constant.
func TestMaybeGenerateThreadTitleCASesAgainstStoredBytes(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-padded")
	thread.Title = "  " + threadtitle.Default + "  "
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		return "Padded thread titled", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "an ask", nil, false)

	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	waitForThreadTitle(t, app, thread.ID, "Padded thread titled")
}

// TestMaybeGenerateThreadTitleSkipsWhenTitleCustom ensures a thread that has
// already been renamed (title is NOT the default) is left alone.
func TestMaybeGenerateThreadTitleSkipsWhenTitleCustom(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-custom")
	thread.Title = "Custom user title"
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls <- struct{}{}
		return "Regenerated", nil
	}

	app.maybeGenerateThreadTitleWithAttachments(thread, "pick me", nil, false)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called when title is not default")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSkipsOnBlankContent covers the empty-message
// guard that prevents the configured text-generation provider from being asked
// to title a no-op message.
func TestMaybeGenerateThreadTitleSkipsOnBlankContent(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-blank")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	calls := make(chan struct{}, 1)
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		calls <- struct{}{}
		return "Unexpected", nil
	}

	app.maybeGenerateThreadTitleWithAttachments(thread, "   \n\t", nil, false)

	select {
	case <-calls:
		t.Fatal("generateThreadTitleFn called on blank content")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestMaybeGenerateThreadTitleSwallowsSubprocessError ensures a title-gen
// failure does NOT panic, does NOT update the title, and does NOT emit a
// rename event — it logs, reports the failure on the completion frame,
// and moves on. An automatic run's error is still emitted; only the
// frontend decides whether a user asked for this one.
func TestMaybeGenerateThreadTitleSwallowsSubprocessError(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-error")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		return "", errors.New("codex CLI failed: exit status 1")
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "fix the reconnect bug", nil, false)

	completion := waitForTitleGenerationEvent(t, events, thread.ID)
	if completion.Error != "provider CLI failed" {
		t.Fatalf("completion error = %q, want the redacted string", completion.Error)
	}
	if updates := events.titleUpdates(); len(updates) != 0 {
		t.Fatalf("thread:updated emitted despite subprocess error: %+v", updates)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != threadtitle.Default {
		t.Fatalf("stored title = %q, want unchanged", stored.Title)
	}
}

// TestMaybeGenerateThreadTitleIgnoresEmptyResponse enforces the behavior when
// the title generator returns an empty string: no rename event, no store
// update. (Sanitization converts empty input into the default title, which
// is also treated as "skip" by the callsite.)
func TestMaybeGenerateThreadTitleIgnoresEmptyResponse(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-empty-response")
	thread.Title = threadtitle.Default
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		return "", nil
	}

	events := captureThreadTitleEvents(t, app)
	app.maybeGenerateThreadTitleWithAttachments(thread, "something", nil, false)

	completion := waitForTitleGenerationEvent(t, events, thread.ID)
	if completion.Error != "" {
		t.Fatalf("completion error = %q, want empty (a no-op is not a failure)", completion.Error)
	}
	if updates := events.titleUpdates(); len(updates) != 0 {
		t.Fatalf("thread:updated emitted despite empty title response: %+v", updates)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != threadtitle.Default {
		t.Fatalf("stored title = %q, want unchanged default", stored.Title)
	}
}

func TestGeneratedThreadTitle_CodexPathHappy(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-title-codex-cli")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	attachmentsRoot := t.TempDir()
	attachments, err := attachmentstore.NewStore(attachmentstore.Config{RootDir: attachmentsRoot}, app.store)
	if err != nil {
		t.Fatalf("attachment store: %v", err)
	}
	app.attachments = attachments
	record, err := app.attachments.Upload(
		thread.ID,
		"screenshot.png",
		"image/png",
		base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}),
		time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	var gotSpec textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		gotSpec = spec
		outputPath := extractCodexOutputPath(spec.Args)
		if outputPath == "" {
			t.Fatalf("codex title args missing --output-last-message: %v", spec.Args)
		}
		if err := os.WriteFile(outputPath, []byte(`{"title":"  \"Reconnect title generation\"  "}`), 0o600); err != nil {
			return textgen.CLIResult{}, err
		}
		return textgen.CLIResult{ExitCode: 0}, nil
	}

	got, err := app.generatedThreadTitle(thread, "Fix title generation for Codex threads.", []store.Attachment{record})
	if err != nil {
		t.Fatalf("generatedThreadTitle() error = %v", err)
	}
	if got != "Reconnect title generation" {
		t.Fatalf("title = %q", got)
	}
	if !argsContain(gotSpec.Args, "exec") || !argsContain(gotSpec.Args, "--ephemeral") {
		t.Fatalf("codex args missing exec --ephemeral: %v", gotSpec.Args)
	}
	if modelArg := nextArgAfter(gotSpec.Args, "--model"); modelArg != textgen.DefaultCodexModel {
		t.Fatalf("codex model = %q, want %q", modelArg, textgen.DefaultCodexModel)
	}
	imagePath := nextArgAfter(gotSpec.Args, "--image")
	if imagePath == "" {
		t.Fatalf("codex args missing --image: %v", gotSpec.Args)
	}
	if !filepath.IsAbs(imagePath) {
		t.Fatalf("image path = %q, want absolute path", imagePath)
	}
	if !strings.Contains(gotSpec.Stdin, "Attachment metadata:") {
		t.Fatalf("prompt missing attachment metadata: %q", gotSpec.Stdin)
	}
}

func TestThreadTitleImagePathsRequireOwnershipAndExistingFile(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-title-image-owner")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	attachments, err := attachmentstore.NewStore(attachmentstore.Config{RootDir: t.TempDir()}, app.store)
	if err != nil {
		t.Fatalf("attachment store: %v", err)
	}
	app.attachments = attachments
	record, err := app.attachments.Upload(
		thread.ID,
		"screenshot.png",
		"image/png",
		base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}),
		time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	paths, err := app.threadTitleImagePaths("other-thread", []store.Attachment{record})
	if err != nil {
		t.Fatalf("threadTitleImagePaths(other) error = %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("cross-thread attachment leaked path: %v", paths)
	}

	_, path, ok, err := app.attachments.Get(record.ID)
	if err != nil || !ok {
		t.Fatalf("Get() = ok %v err %v", ok, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove attachment file: %v", err)
	}
	paths, err = app.threadTitleImagePaths(thread.ID, []store.Attachment{record})
	if err != nil {
		t.Fatalf("threadTitleImagePaths(stale) error = %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("stale attachment file should be skipped: %v", paths)
	}
}

func TestGeneratedThreadTitle_RoutesToClaudeWhenConfigured(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"textGenerationProvider": "claude",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	thread := testThread("thread-title-claude-cli")
	thread.WorkspacePath = t.TempDir()

	var gotSpec textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		gotSpec = spec
		return textgen.CLIResult{
			Stdout:   `{"structured_output":{"title":"Claude generated title"}}`,
			ExitCode: 0,
		}, nil
	}

	got, err := app.generatedThreadTitle(thread, "Fix title generation.", nil)
	if err != nil {
		t.Fatalf("generatedThreadTitle() error = %v", err)
	}
	if got != "Claude generated title" {
		t.Fatalf("title = %q", got)
	}
	if !argsContain(gotSpec.Args, "-p") || !argsContain(gotSpec.Args, "--json-schema") {
		t.Fatalf("claude args missing structured output flags: %v", gotSpec.Args)
	}
	if argsContain(gotSpec.Args, "--dangerously-skip-permissions") {
		t.Fatalf("claude title args must not bypass permissions: %v", gotSpec.Args)
	}
	if modelArg := nextArgAfter(gotSpec.Args, "--model"); modelArg != textgen.DefaultClaudeModel {
		t.Fatalf("claude model = %q, want %q", modelArg, textgen.DefaultClaudeModel)
	}
}

// ---- Layer 2 (runtime retry) tests for generatedThreadTitle ----

// TestGeneratedThreadTitle_Layer2PrimaryFailsAlternateSucceeds covers the
// canonical Layer 2 case: Codex is on PATH but its CLI fails (auth, rate
// limit, OpenAI down, etc.). The orchestrator must retry with Claude when
// Claude is also installed, and surface Claude's title.
func TestGeneratedThreadTitle_Layer2PrimaryFailsAlternateSucceeds(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	app.lookPathFn = fakeLookPath("claude", "codex") // both installed.

	thread := testThread("thread-title-l2-fallback")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var specs []textgen.CLISpec
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		specs = append(specs, spec)
		if spec.Binary == "codex" {
			// Simulate auth/rate-limit failure.
			return textgen.CLIResult{
				Stderr:   "Error: unauthorized — token expired",
				ExitCode: 1,
			}, nil
		}
		// Claude succeeds.
		return textgen.CLIResult{
			Stdout:   `{"structured_output":{"title":"Fallback title via Claude"}}`,
			ExitCode: 0,
		}, nil
	}

	got, err := app.generatedThreadTitle(thread, "Fix title gen.", nil)
	if err != nil {
		t.Fatalf("generatedThreadTitle: %v", err)
	}
	if got != "Fallback title via Claude" {
		t.Fatalf("title = %q, want fallback from Claude", got)
	}
	if len(specs) != 2 {
		t.Fatalf("executor called %d times, want 2 (primary + alternate)", len(specs))
	}
	if specs[0].Binary != "codex" {
		t.Fatalf("first invocation binary = %q, want codex", specs[0].Binary)
	}
	if specs[1].Binary != "claude" {
		t.Fatalf("retry invocation binary = %q, want claude", specs[1].Binary)
	}
	// The retry must use Claude's default model, not whatever was set for Codex.
	if model := nextArgAfter(specs[1].Args, "--model"); model != textgen.DefaultClaudeModel {
		t.Fatalf("retry model = %q, want %q (always default for alternate)", model, textgen.DefaultClaudeModel)
	}
}

func TestGeneratedThreadTitle_Layer2BothFailReturnsPrimaryError(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")

	thread := testThread("thread-title-l2-both-fail")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		stderr := "codex went boom"
		if spec.Binary == "claude" {
			stderr = "claude also went boom"
		}
		return textgen.CLIResult{Stderr: stderr, ExitCode: 1}, nil
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if err == nil {
		t.Fatal("expected error when both providers fail")
	}
	// Primary error surfaces — not the alternate's.
	if !strings.Contains(err.Error(), "codex went boom") {
		t.Fatalf("error should carry primary (codex) failure: %v", err)
	}
	if strings.Contains(err.Error(), "claude also went boom") {
		t.Fatalf("alternate error leaked: %v", err)
	}
}

func TestGeneratedThreadTitle_Layer2AlternateMissingNoRetry(t *testing.T) {
	app := newTestAppWithStore(t)
	// Configured codex; ONLY codex on PATH. Codex CLI fails. There's no
	// claude to fall back to, so the orchestrator must NOT call the
	// executor a second time.
	app.lookPathFn = fakeLookPath("codex")

	thread := testThread("thread-title-l2-no-alt")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	calls := 0
	app.textGenerationExecutor = func(_ context.Context, _ textgen.CLISpec) (textgen.CLIResult, error) {
		calls++
		return textgen.CLIResult{Stderr: "codex boom", ExitCode: 1}, nil
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("executor called %d times, want 1 (no alternate available)", calls)
	}
}

func TestGeneratedThreadTitle_Layer2ContextCanceledNoRetry(t *testing.T) {
	app := newTestAppWithStore(t)
	app.lookPathFn = fakeLookPath("claude", "codex")

	thread := testThread("thread-title-l2-canceled")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	calls := 0
	app.textGenerationExecutor = func(_ context.Context, _ textgen.CLISpec) (textgen.CLIResult, error) {
		calls++
		// Simulate the app shutting down mid-call: surface ctx.Canceled
		// from the executor. The orchestrator must NOT retry.
		return textgen.CLIResult{}, context.Canceled
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("executor called %d times, want 1 (ctx canceled = no retry)", calls)
	}
}

// TestGeneratedThreadTitle_Layer1SubstitutesThenLayer2NoAlternate is the
// composed-fallback case: configured Codex is missing (Layer 1 substitutes
// Claude), Claude runs and fails, the Layer 2 orchestrator asks for the
// alternate (Codex) and finds its binary still missing → no retry. This
// guards against a regression where the substitution mutates Layer 2's
// "alternate" search and accidentally points back at Claude (self-retry)
// or where the alternate-missing branch loses its `ok=false` guard.
func TestGeneratedThreadTitle_Layer1SubstitutesThenLayer2NoAlternate(t *testing.T) {
	app := newTestAppWithStore(t)
	resetProviderBinarySettings(t, app)
	// Default settings prefer Codex; only Claude is on PATH.
	app.lookPathFn = fakeLookPath("claude")

	thread := testThread("thread-title-l1l2-no-alt")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	calls := 0
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		calls++
		if spec.Binary != "claude" {
			t.Fatalf("unexpected binary %q — Layer 1 should have substituted to claude", spec.Binary)
		}
		return textgen.CLIResult{Stderr: "claude boom", ExitCode: 1}, nil
	}

	_, err := app.generatedThreadTitle(thread, "anything", nil)
	if err == nil {
		t.Fatal("expected error from claude failing")
	}
	if calls != 1 {
		t.Fatalf("executor called %d times, want 1 (Layer 1 substituted to Claude, Layer 2 alternate Codex unavailable)", calls)
	}
}

// TestApplyThreadTitleIfCurrentSkipsWhenTitleChanged exercises the race
// guard: if the user renamed the thread between the generation call and
// the apply call, UpdateTitleIfCurrent reports 0 rows affected and the
// rename event must NOT be emitted.
func TestApplyThreadTitleIfCurrentSkipsWhenTitleChanged(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-title-cas-lost")
	thread.Title = "User picked this"
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	events := captureThreadTitleEvents(t, app)

	applied, err := app.applyThreadTitleIfCurrent(thread.ID, threadtitle.Default, "Generated fallback")
	if err != nil {
		t.Fatalf("applyThreadTitleIfCurrent() error = %v", err)
	}
	if applied {
		t.Fatal("applied = true when the stored title is not the expected one")
	}
	if updates := events.titleUpdates(); len(updates) != 0 {
		t.Fatalf("thread:updated emitted for a lost CAS: %+v", updates)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "User picked this" {
		t.Fatalf("stored title = %q, want user-picked preserved", stored.Title)
	}
}
