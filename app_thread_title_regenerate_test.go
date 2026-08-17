package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
)

// seedThreadTitleContextItem inserts one top-level conversation row for
// the regeneration-context tests.
func seedThreadTitleContextItem(t *testing.T, app *App, threadID, id string, turn, index int, kind, role, summary, meta string) {
	t.Helper()
	if err := app.store.InsertItem(store.Item{
		ID: id, ThreadID: threadID, TurnIndex: turn, ItemIndex: index,
		Kind: kind, Role: role, Status: "completed", Summary: summary, Meta: meta,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertItem(%s) error = %v", id, err)
	}
}

func newRegenerateTitleThread(t *testing.T, app *App, id, title string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.Title = title
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return thread
}

// TestRegenerateThreadTitleAppliesAndEmits is the happy path: the RPC
// acknowledges immediately, the context is built from the thread's own
// rows, the CAS lands, the sidebar gets the patch event, and the
// completion frame follows it clean.
func TestRegenerateThreadTitleAppliesAndEmits(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-happy", "Stale title")

	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "make worktree pruning safe",
		`{"attachments":[{"id":"a1","threadId":"thread-regen-happy","filename":"shot.png","mimeType":"image/png","size":3}]}`)
	seedThreadTitleContextItem(t, app, thread.ID, "assistant:0", 0, 1, "assistant_text", "assistant", "the sweep deletes registrations", "")
	seedThreadTitleContextItem(t, app, thread.ID, "tool:0", 1, 0, "tool_call", "assistant", "ran git worktree list", "")

	contexts := make(chan string, 1)
	app.regenerateThreadTitleFn = func(got store.Thread, threadContext string) (string, error) {
		if got.ID != thread.ID {
			t.Errorf("regenerate called with thread %q, want %q", got.ID, thread.ID)
		}
		contexts <- threadContext
		return "  Safe worktree pruning  ", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}

	completion := waitForTitleGenerationEvent(t, events, thread.ID)
	if completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}

	want := "USER:\nmake worktree pruning safe\n[Attachments: shot.png]\n\nASSISTANT:\nthe sweep deletes registrations"
	if gotContext := <-contexts; gotContext != want {
		t.Fatalf("context =\n%q\nwant\n%q", gotContext, want)
	}

	updates := events.titleUpdates()
	if len(updates) != 1 {
		t.Fatalf("thread:updated emitted %d times, want 1", len(updates))
	}
	if updates[0].Action != "patch" || updates[0].ID != thread.ID ||
		updates[0].Title == nil || *updates[0].Title != "Safe worktree pruning" {
		t.Fatalf("unexpected thread:updated payload %+v", updates[0])
	}
	if order := events.order(); len(order) != 2 || order[0] != "thread:updated" || order[1] != "thread:title_generation" {
		t.Fatalf("event order = %v, want the patch before the completion", order)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Safe worktree pruning" {
		t.Fatalf("stored title = %q", stored.Title)
	}
}

// TestRegenerateThreadTitleAcksBeforeTheRunFinishes is the reason the
// method is asynchronous: the RPC must return while the provider leg is
// still going, or the transport's flat client timeout abandons the call
// and the user's button stacks retries on a run that is still alive.
func TestRegenerateThreadTitleAcksBeforeTheRunFinishes(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-async", "Stale title")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "the ask", "")

	started := make(chan struct{})
	release := make(chan struct{})
	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		close(started)
		<-release
		return "Eventually titled", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("regeneration never started")
	}
	// The RPC has returned while the run is provably mid-flight.
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Stale title" {
		t.Fatalf("stored title = %q, want the run still in flight", stored.Title)
	}

	close(release)
	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	waitForThreadTitle(t, app, thread.ID, "Eventually titled")
}

// TestRegenerateThreadTitleJoinsAnInFlightRun: a second click while a
// generation is running must not start a second provider run. The caller
// joins the running one, whose completion event answers both.
func TestRegenerateThreadTitleJoinsAnInFlightRun(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-joins", "Stale title")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "the ask", "")

	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return "Only regeneration", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first regeneration never started")
	}
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle(joined) error = %v", err)
	}
	close(release)

	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	waitForThreadTitle(t, app, thread.ID, "Only regeneration")
	if got := calls.Load(); got != 1 {
		t.Fatalf("regenerateThreadTitleFn called %d times, want 1 (joined the in-flight run)", got)
	}
}

// TestRegenerateThreadTitleSecondRunSeesTheFirstTitle: the previous
// title the prompt quotes is re-read from the store, so a second
// regeneration is asked to improve on the first one's answer rather than
// on the title it replaced.
func TestRegenerateThreadTitleSecondRunSeesTheFirstTitle(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-second", "Stale title")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "make resume stop flaking", "")

	// Channels rather than a slice: each run happens on its own detached
	// goroutine, so the test reads what they saw through a synchronised
	// handoff instead of an unguarded append.
	previous := make(chan string, 2)
	var runs atomic.Int64
	app.regenerateThreadTitleFn = func(got store.Thread, _ string) (string, error) {
		previous <- got.Title
		if runs.Add(1) == 1 {
			return "First regenerated title", nil
		}
		return "Second regenerated title", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}
	waitForThreadTitle(t, app, thread.ID, "First regenerated title")
	waitForTitleGenerationEvent(t, events, thread.ID)

	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle(second) error = %v", err)
	}
	waitForThreadTitle(t, app, thread.ID, "Second regenerated title")

	if first := <-previous; first != "Stale title" {
		t.Fatalf("first run saw previous title %q, want the stored one", first)
	}
	if second := <-previous; second != "First regenerated title" {
		t.Fatalf("second run saw previous title %q, want the first run's answer", second)
	}
}

// TestRegenerateThreadTitleSupersededKeepsTheRename covers a rename
// landing while the CLI ran: the other writer wins, the completion is
// still clean (this run did nothing wrong), and no patch is emitted.
func TestRegenerateThreadTitleSupersededKeepsTheRename(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-superseded", "Stale title")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "the ask", "")

	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		// The user renames while generation is in flight.
		if err := app.store.UpdateTitle(thread.ID, "User picked this"); err != nil {
			t.Errorf("UpdateTitle() error = %v", err)
		}
		return "Model picked that", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}

	completion := waitForTitleGenerationEvent(t, events, thread.ID)
	if completion.Error != "" {
		t.Fatalf("completion error = %q, want empty (a lost CAS is not a failure)", completion.Error)
	}
	if updates := events.titleUpdates(); len(updates) != 0 {
		t.Fatalf("thread:updated emitted for a lost CAS: %+v", updates)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "User picked this" {
		t.Fatalf("stored title = %q, want the rename preserved", stored.Title)
	}
}

// TestRegenerateThreadTitleReportsTheRedactedFailure: a provider failure
// reaches the frontend as the completion frame's error, redacted because
// the CLI's stderr can carry the prompt.
func TestRegenerateThreadTitleReportsTheRedactedFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-failure", "Stale title")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "the ask", "")

	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		return "", errors.New("codex CLI failed: unauthorized — the prompt said hunter2")
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}

	completion := waitForTitleGenerationEvent(t, events, thread.ID)
	if completion.Error != "provider CLI failed" {
		t.Fatalf("completion error = %q, want the redacted string", completion.Error)
	}
	if updates := events.titleUpdates(); len(updates) != 0 {
		t.Fatalf("thread:updated emitted for a failed regeneration: %+v", updates)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Stale title" {
		t.Fatalf("stored title = %q, want unchanged", stored.Title)
	}
}

// TestRegenerateThreadTitleNoOpResultsSkipTheWrite covers the three
// answers that mean "nothing better to say": unchanged, empty, and the
// default sentinel. All three are clean completions.
func TestRegenerateThreadTitleNoOpResultsSkipTheWrite(t *testing.T) {
	for name, raw := range map[string]string{
		"same as previous": "Existing title",
		"empty":            "   ",
		"default sentinel": threadtitle.Default,
	} {
		t.Run(name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			thread := newRegenerateTitleThread(t, app, "thread-regen-noop-"+strings.ReplaceAll(name, " ", "-"), "Existing title")
			seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "the ask", "")

			app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
				return raw, nil
			}
			events := captureThreadTitleEvents(t, app)

			if err := app.RegenerateThreadTitle(thread.ID); err != nil {
				t.Fatalf("RegenerateThreadTitle() error = %v", err)
			}
			completion := waitForTitleGenerationEvent(t, events, thread.ID)
			if completion.Error != "" {
				t.Fatalf("completion error = %q, want empty", completion.Error)
			}
			if updates := events.titleUpdates(); len(updates) != 0 {
				t.Fatalf("thread:updated emitted for a no-op regeneration: %+v", updates)
			}
			stored, err := app.store.GetThread(thread.ID)
			if err != nil {
				t.Fatalf("GetThread() error = %v", err)
			}
			if stored.Title != "Existing title" {
				t.Fatalf("stored title = %q, want unchanged", stored.Title)
			}
		})
	}
}

// TestRegenerateThreadTitleEmptyThreadDoesNotSpawn: a thread with no
// renderable conversation has no subject to name, so nothing runs — and
// the completion still fires, so the frontend's pending state clears.
func TestRegenerateThreadTitleEmptyThreadDoesNotSpawn(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-empty", "Existing title")
	// Tool traffic only — no user or assistant text.
	seedThreadTitleContextItem(t, app, thread.ID, "tool:0", 0, 0, "tool_call", "assistant", "ran a tool", "")

	var calls atomic.Int64
	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		calls.Add(1)
		return "Should not happen", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}
	completion := waitForTitleGenerationEvent(t, events, thread.ID)
	if completion.Error != "" {
		t.Fatalf("completion error = %q, want empty (nothing to title is not a failure)", completion.Error)
	}
	if calls.Load() != 0 {
		t.Fatal("regeneration ran for a thread with no conversation rows")
	}
}

// TestRegenerateThreadTitleReadsCorruptUserMeta: attachment names are
// garnish, so a user row whose meta no longer decodes still contributes
// its text rather than failing the whole regeneration.
//
// The meta is well-formed JSON of the wrong SHAPE rather than a torn
// blob, because a torn one cannot exist: the items table's partial
// indexes evaluate json_extract over meta, so SQLite refuses the insert
// outright ("malformed JSON").
func TestRegenerateThreadTitleReadsCorruptUserMeta(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-corrupt-meta", "Stale title")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "the ask that survives", `{"attachments":"not-an-array"}`)

	contexts := make(chan string, 1)
	app.regenerateThreadTitleFn = func(_ store.Thread, threadContext string) (string, error) {
		contexts <- threadContext
		return "Survived the bad meta", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}
	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	if got := <-contexts; got != "USER:\nthe ask that survives" {
		t.Fatalf("context = %q, want the row's text despite the corrupt meta", got)
	}
	waitForThreadTitle(t, app, thread.ID, "Survived the bad meta")
}

// TestRegenerateThreadTitleCASesAgainstStoredBytes guards the one place
// trimming could silently break the write: UpdateTitleIfCurrent matches
// the stored string exactly, so a padded title must still swap.
func TestRegenerateThreadTitleCASesAgainstStoredBytes(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := newRegenerateTitleThread(t, app, "thread-regen-padded", "  Padded title  ")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "the ask", "")

	seen := make(chan string, 1)
	app.regenerateThreadTitleFn = func(got store.Thread, _ string) (string, error) {
		seen <- got.Title
		return "Sharper title", nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}
	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	if gotPrevious := <-seen; gotPrevious != "  Padded title  " {
		t.Fatalf("seam saw thread title %q, want the stored bytes", gotPrevious)
	}
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Sharper title" {
		t.Fatalf("stored title = %q, want the regenerated title", stored.Title)
	}
}

func TestRegenerateThreadTitleUnknownThreadErrors(t *testing.T) {
	app := newTestAppWithStore(t)
	app.regenerateThreadTitleFn = func(store.Thread, string) (string, error) {
		t.Error("regeneration ran for an unknown thread")
		return "", nil
	}
	if err := app.RegenerateThreadTitle("nope"); err == nil {
		t.Fatal("RegenerateThreadTitle(unknown) error = nil, want not-found")
	}
}

// TestRegenerateThreadTitleDrivesRegeneratePromptWithoutImages walks the
// real CLI path (through the executor fake): the regeneration prompt is
// what reaches stdin, it names the previous title, and no image flags
// ride along — the regen path passes attachment NAMES, not bytes.
func TestRegenerateThreadTitleDrivesRegeneratePromptWithoutImages(t *testing.T) {
	app := newTestAppWithStore(t)
	// Update the fixture's own settings service rather than replacing it:
	// a fresh one would discard the poisoned provider binary paths the
	// spawn isolation installed.
	if _, err := app.settings.Update(map[string]any{"textGenerationProvider": "claude"}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	thread := newRegenerateTitleThread(t, app, "thread-regen-cli", "Old title")
	seedThreadTitleContextItem(t, app, thread.ID, "user:0", 0, 0, "user_text", "user", "make resume stop flaking", "")

	specs := make(chan textgen.CLISpec, 1)
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		specs <- spec
		return textgen.CLIResult{
			Stdout:   `{"structured_output":{"title":"Fix flaky session resume"}}`,
			ExitCode: 0,
		}, nil
	}

	events := captureThreadTitleEvents(t, app)
	if err := app.RegenerateThreadTitle(thread.ID); err != nil {
		t.Fatalf("RegenerateThreadTitle() error = %v", err)
	}
	if completion := waitForTitleGenerationEvent(t, events, thread.ID); completion.Error != "" {
		t.Fatalf("completion error = %q, want empty", completion.Error)
	}
	waitForThreadTitle(t, app, thread.ID, "Fix flaky session resume")

	gotSpec := <-specs
	if !strings.HasPrefix(gotSpec.Stdin, "Regenerate the title for an existing thread") {
		t.Fatalf("stdin is not the regeneration prompt: %q", gotSpec.Stdin)
	}
	if !strings.Contains(gotSpec.Stdin, `The previous title was "Old title".`) {
		t.Fatalf("prompt missing the quoted previous title: %q", gotSpec.Stdin)
	}
	if !strings.HasSuffix(gotSpec.Stdin, "Thread contents:\nUSER:\nmake resume stop flaking") {
		t.Fatalf("prompt missing the thread contents section: %q", gotSpec.Stdin)
	}
	if argsContain(gotSpec.Args, "--image") {
		t.Fatalf("regeneration must not pass images: %v", gotSpec.Args)
	}
}
