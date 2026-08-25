package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// writeCodexReviewBinary mints a fake codex app-server that answers the two
// RPCs these bindings drive and appends every request line to capturePath, so
// a test can assert what actually went on the wire rather than only that no
// error came back.
//
// reviewThreadID is what `review/start` reports back. Passing the session's own
// thread id models the inline answer; passing anything else models the
// detached one the binding has to refuse.
func writeCodexReviewBinary(t *testing.T, threadID, reviewThreadID, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %s
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"review/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"reviewThreadId":"%s","turn":{"id":"review-turn","status":"inProgress"}}}\n' "$id"
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, capturePath, threadID, reviewThreadID)

	path := filepath.Join(t.TempDir(), "codex-review.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex-review binary: %v", err)
	}
	return path
}

func writeProjectedCodexReviewBinary(t *testing.T, threadID, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %s
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"config/read"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"config":{"review_model":"gpt-review"},"origins":{}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"review/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"reviewThreadId":"%s","turn":{"id":"outer-review","status":"inProgress","items":[]}}}\n' "$id"
        printf '{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"enteredReviewMode","id":"enter-review","review":"Review uncommitted changes"}}}\n'
        printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"%s","turn":{"id":"private-review","status":"inProgress","items":[]}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"userMessage","id":"review-prompt","content":[{"type":"text","text":"Review the working tree"}]}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"commandExecution","id":"cmd-review","command":"git diff --stat","status":"inProgress"}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"commandExecution","id":"cmd-review","command":"git diff --stat","status":"completed","aggregatedOutput":"1 file changed","exitCode":0}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"reasoning","id":"think-review","summary":"Checked the affected parser."}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"agentMessage","id":"raw-review","text":"{\\"findings\\":[]}"}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"exitedReviewMode","id":"exit-review","review":"No issues found."}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"agentMessage","id":"answer-review","text":"No issues found."}}}\n'
        printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","turnId":"outer-review","item":{"type":"agentMessage","id":"answer-review","text":"No issues found."}}}\n'
        printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"id":"outer-review","status":"completed","items":[]}}}\n'
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
	`, capturePath, threadID, threadID, threadID, threadID, threadID, threadID, threadID, threadID, threadID, threadID, threadID, threadID, threadID)

	path := filepath.Join(t.TempDir(), "codex-projected-review.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write projected codex review binary: %v", err)
	}
	return path
}

func installCodexReviewSession(t *testing.T, app *App, thread store.Thread, reviewThreadID, capturePath string) {
	t.Helper()
	codexThreadID := thread.ID + "-codex"
	if reviewThreadID == "" {
		reviewThreadID = codexThreadID
	}
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  writeCodexReviewBinary(t, codexThreadID, reviewThreadID, capturePath),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.mu.Lock()
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "codex-review-token",
		codex:    sess,
	}
	app.mu.Unlock()
}

func newCodexThreadForReviewTest(t *testing.T, app *App, id string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

func capturedRequestLines(t *testing.T, capturePath string) string {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read capture: %v", err)
	}
	return string(raw)
}

func waitForCapturedRequest(t *testing.T, capturePath, needle string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		captured := capturedRequestLines(t, capturePath)
		if strings.Contains(captured, needle) {
			return captured
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q on the wire; captured:\n%s", needle, captured)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCodexReviewTargetFromWireRoutesThroughTheValidatingConstructors pins
// that the flat wire shape cannot smuggle an invalid variant past the union's
// constructors: a commit with no sha, a base branch with no branch, and a
// custom review with no instructions are all refused HERE, before a request
// that would make Codex review the wrong thing is written.
func TestCodexReviewTargetFromWireRoutesThroughTheValidatingConstructors(t *testing.T) {
	valid := []struct {
		name string
		in   CodexReviewTarget
		want codex.ReviewTargetKind
	}{
		{"uncommitted", CodexReviewTarget{Kind: "uncommittedChanges"}, codex.ReviewTargetUncommittedChanges},
		{"base branch", CodexReviewTarget{Kind: "baseBranch", Branch: "main"}, codex.ReviewTargetBaseBranch},
		{"commit", CodexReviewTarget{Kind: "commit", SHA: "abc123", Title: "fix"}, codex.ReviewTargetCommit},
		{"commit without a title", CodexReviewTarget{Kind: "commit", SHA: "abc123"}, codex.ReviewTargetCommit},
		{"custom", CodexReviewTarget{Kind: "custom", Instructions: "look at the locking"}, codex.ReviewTargetCustom},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			target, err := codexReviewTargetFromWire(tc.in)
			if err != nil {
				t.Fatalf("codexReviewTargetFromWire: %v", err)
			}
			if target.Kind() != tc.want {
				t.Fatalf("kind = %q, want %q", target.Kind(), tc.want)
			}
		})
	}

	invalid := []struct {
		name string
		in   CodexReviewTarget
	}{
		{"unset kind", CodexReviewTarget{}},
		{"unknown kind", CodexReviewTarget{Kind: "wholeRepo"}},
		{"base branch without a branch", CodexReviewTarget{Kind: "baseBranch"}},
		{"base branch with a blank branch", CodexReviewTarget{Kind: "baseBranch", Branch: "   "}},
		{"commit without a sha", CodexReviewTarget{Kind: "commit", Title: "fix"}},
		{"custom without instructions", CodexReviewTarget{Kind: "custom"}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := codexReviewTargetFromWire(tc.in); err == nil {
				t.Fatalf("codexReviewTargetFromWire(%+v) must fail", tc.in)
			}
		})
	}
}

func TestCodexReviewCommandTargetUsesTheComposerGrammar(t *testing.T) {
	for _, command := range []string{
		"/review",
		"/review uncommitted",
		"/review branch\tmain",
		"/review commit abc123 Fix the parser",
		"/review custom inspect every lock transition",
	} {
		if _, matched, err := codexReviewCommandTarget(command); !matched || err != nil {
			t.Fatalf("codexReviewCommandTarget(%q) = matched %v, err %v", command, matched, err)
		}
	}
	for _, prose := range []string{" /review", "/reviewish", "please /review"} {
		if _, matched, err := codexReviewCommandTarget(prose); matched || err != nil {
			t.Fatalf("codexReviewCommandTarget(%q) = matched %v, err %v", prose, matched, err)
		}
	}
	if _, matched, err := codexReviewCommandTarget("/review branch"); !matched || err == nil {
		t.Fatalf("missing branch = matched %v, err %v", matched, err)
	}
}

func TestCodexReviewCannotBeQueuedOrSteeredIntoAnActiveTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := newCodexThreadForReviewTest(t, app, "thread-codex-review-active")

	if _, err := app.RegisterQueueItem(thread.ID, "/review", SendMessageOptions{}); err == nil ||
		!strings.Contains(err.Error(), "idle thread") {
		t.Fatalf("queue /review error = %v", err)
	}
	if _, err := app.SteerMessageWithOptions(thread.ID, "/review", SendMessageOptions{}); err == nil ||
		!strings.Contains(err.Error(), "idle thread") {
		t.Fatalf("steer /review error = %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("rejected review command persisted rows: %+v", items)
	}
}

// TestStartCodexReviewSendsTheTargetInline is the happy path: the validated
// target reaches `review/start` with inline delivery, and the answer names the
// AO thread the review's transcript will arrive on.
func TestStartCodexReviewSendsTheTargetInline(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	thread := newCodexThreadForReviewTest(t, app, "thread-codex-review")
	capturePath := filepath.Join(t.TempDir(), "rpc.ndjson")
	installCodexReviewSession(t, app, thread, "", capturePath)

	started, err := app.StartCodexReview(context.Background(), thread.ID, CodexReviewTarget{
		Kind:   "baseBranch",
		Branch: "main",
	})
	if err != nil {
		t.Fatalf("StartCodexReview: %v", err)
	}
	if started.ThreadID != thread.ID {
		t.Fatalf("ThreadID = %q, want the requesting thread %q", started.ThreadID, thread.ID)
	}
	if started.TurnID != "review-turn" || started.TurnStatus != "inProgress" {
		t.Fatalf("started = %+v, want the wire's turn id and status", started)
	}

	captured := waitForCapturedRequest(t, capturePath, `"method":"review/start"`)
	if !strings.Contains(captured, `"delivery":"inline"`) {
		t.Fatalf("review/start must name inline delivery; captured:\n%s", captured)
	}
	if !strings.Contains(captured, `"type":"baseBranch"`) || !strings.Contains(captured, `"branch":"main"`) {
		t.Fatalf("review/start did not carry the requested target; captured:\n%s", captured)
	}
}

func TestComposerReviewUsesOneTurnWithAgentActivityAndSourcedResult(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = app.newTriageRouter(app.store)

	thread := newCodexThreadForReviewTest(t, app, "thread-codex-projected-review")
	capturePath := filepath.Join(t.TempDir(), "rpc.ndjson")
	var callbackErrors = make(chan error, 16)
	sess, err := codex.NewSession(
		context.Background(),
		thread.ID,
		codex.Config{
			Binary:  writeProjectedCodexReviewBinary(t, thread.ID+"-codex", capturePath),
			WorkDir: thread.WorkspacePath,
			Model:   "gpt-parent",
		},
		func(event provider.ProviderEvent) {
			if handleErr := app.triage.Handle(event); handleErr != nil {
				select {
				case callbackErrors <- handleErr:
				default:
				}
			}
		},
	)
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.mu.Lock()
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex), token: "codex-projected-review", codex: sess,
	}
	app.mu.Unlock()

	if _, err := app.SendMessageWithOptions(thread.ID, "/review", SendMessageOptions{}); err != nil {
		t.Fatalf("SendMessageWithOptions /review: %v", err)
	}
	waitForCapturedRequest(t, capturePath, `"method":"review/start"`)

	var items []store.Item
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, err = app.store.ListItems(thread.ID)
		if err != nil {
			t.Fatalf("ListItems: %v", err)
		}
		hasResult := false
		for _, item := range items {
			if item.Kind == "command_result" {
				hasResult = true
				break
			}
		}
		if hasResult {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for review result; items=%+v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case callbackErr := <-callbackErrors:
		t.Fatalf("triage callback: %v", callbackErr)
	default:
	}

	var user, launch, result *store.Item
	for i := range items {
		item := &items[i]
		switch {
		case item.ID == "user:0":
			user = item
		case item.ToolName == "codex_review":
			launch = item
		case item.Kind == "command_result":
			result = item
		case item.Kind == "notification" && strings.Contains(item.Meta, "review_status"):
			t.Fatalf("review status notification should not be persisted: %+v", item)
		case item.Kind == "assistant_text" && item.ParentID == "":
			t.Fatalf("review final leaked as parent-agent prose: %+v", item)
		}
	}
	if user == nil || user.Summary != "/review" || !strings.Contains(user.Meta, `"command":"review"`) {
		t.Fatalf("user command row = %+v", user)
	}
	if launch == nil || launch.Status != "completed" || !strings.Contains(launch.Meta, `"model":"gpt-review"`) {
		t.Fatalf("review launch = %+v", launch)
	}
	if result == nil || result.Summary != "No issues found." || result.ParentID != "" {
		t.Fatalf("review result = %+v", result)
	}
	if !strings.Contains(result.Meta, `"sourceKind":"review"`) || !strings.Contains(result.Meta, launch.ID) {
		t.Fatalf("review result source meta = %s", result.Meta)
	}
	children, err := app.store.ListSubagentDescendants(thread.ID, launch.ID)
	if err != nil {
		t.Fatalf("ListSubagentDescendants: %v", err)
	}
	if len(children) < 4 {
		t.Fatalf("review agent transcript has %d rows, want prompt, tool, thinking, and final: %+v", len(children), children)
	}
}

// TestStartCodexReviewRefusesADetachedAnswer: we asked for inline, so a
// different review thread id means the transcript lands on a thread this
// session does not own and is quarantined. Returning success would hand the UI
// a billed turn it can never show.
func TestStartCodexReviewRefusesADetachedAnswer(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	thread := newCodexThreadForReviewTest(t, app, "thread-codex-review-detached")
	capturePath := filepath.Join(t.TempDir(), "rpc.ndjson")
	installCodexReviewSession(t, app, thread, "some-other-thread", capturePath)

	_, err := app.StartCodexReview(context.Background(), thread.ID, CodexReviewTarget{Kind: "uncommittedChanges"})
	if err == nil {
		t.Fatal("a detached answer must be surfaced as an error, not reported as a started review")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Fatalf("error = %v, want it to name the detached thread", err)
	}
	turn, found, turnErr := app.store.GetTurnByThreadIndex(thread.ID, 0)
	if turnErr != nil || !found || turn.CompletedAt == nil {
		t.Fatalf("failed review turn = %+v, found=%v, err=%v", turn, found, turnErr)
	}
}

// TestCompactCodexThreadDrivesTheRPC — the response body is empty, so the only
// evidence the binding did anything is the request itself.
func TestCompactCodexThreadDrivesTheRPC(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	thread := newCodexThreadForReviewTest(t, app, "thread-codex-compact")
	capturePath := filepath.Join(t.TempDir(), "rpc.ndjson")
	installCodexReviewSession(t, app, thread, "", capturePath)

	if err := app.CompactCodexThread(context.Background(), thread.ID); err != nil {
		t.Fatalf("CompactCodexThread: %v", err)
	}
	captured := waitForCapturedRequest(t, capturePath, `"method":"thread/compact/start"`)
	if !strings.Contains(captured, thread.ID+"-codex") {
		t.Fatalf("compact must name the session's codex thread id; captured:\n%s", captured)
	}
}

// A review is a turn-starting composer command, so a cold thread gets the same
// lazy session materialisation as an ordinary first message. Compaction still
// requires an existing provider context.
func TestCodexReviewLazyStartsAColdThread(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := newCodexThreadForReviewTest(t, app, "thread-codex-no-session")
	binary := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:           `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`:         `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"cold-codex-thread"}}}`,
		`"method":"config/read"`:          `{"jsonrpc":"2.0","id":%s,"result":{"config":{},"origins":{}}}`,
		`"method":"review/start"`:         `{"jsonrpc":"2.0","id":%s,"result":{"reviewThreadId":"cold-codex-thread","turn":{"id":"cold-review","status":"inProgress","items":[]}}}`,
		`"method":"thread/compact/start"`: `{"jsonrpc":"2.0","id":%s,"result":{}}`,
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set mock Codex binary: %v", err)
	}

	started, err := app.StartCodexReview(context.Background(), thread.ID, CodexReviewTarget{Kind: "uncommittedChanges"})
	if err != nil {
		t.Fatalf("StartCodexReview: %v", err)
	}
	if started.TurnID != "cold-review" {
		t.Fatalf("started = %+v", started)
	}
	if _, ok := app.sessionManager().get(thread.ID); !ok {
		t.Fatal("cold review did not retain the lazy-started session")
	}
	if err := app.CompactCodexThread(context.Background(), thread.ID); err == nil {
		t.Fatal("CompactCodexThread must refuse while the review turn is active")
	}

	if _, err := app.StartCodexReview(context.Background(), "  ", CodexReviewTarget{Kind: "uncommittedChanges"}); err == nil {
		t.Fatal("StartCodexReview must refuse an empty thread id")
	}
	if err := app.CompactCodexThread(context.Background(), ""); err == nil {
		t.Fatal("CompactCodexThread must refuse an empty thread id")
	}
}

// TestCodexReviewBindingsRefuseANonCodexThread — Codex-only RPCs. A Claude
// thread has no `review/start`, so the frontend must branch on provider and
// the backend must say so rather than nil-dereferencing its way there.
func TestCodexReviewBindingsRefuseANonCodexThread(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	thread := testThread("thread-claude-not-codex")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeClaudeControlPassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.mu.Lock()
	app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "t", claude: sess}
	app.mu.Unlock()

	if _, err := app.StartCodexReview(context.Background(), thread.ID, CodexReviewTarget{Kind: "uncommittedChanges"}); err == nil ||
		!strings.Contains(err.Error(), "not a Codex thread") {
		t.Fatalf("StartCodexReview on a Claude thread = %v, want a provider-mismatch refusal", err)
	}
	if err := app.CompactCodexThread(context.Background(), thread.ID); err == nil ||
		!strings.Contains(err.Error(), "not a Codex thread") {
		t.Fatalf("CompactCodexThread on a Claude thread = %v, want a provider-mismatch refusal", err)
	}
}
