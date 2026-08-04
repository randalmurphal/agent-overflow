package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
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

// TestStartCodexReviewSendsTheTargetInline is the happy path: the validated
// target reaches `review/start` with inline delivery, and the answer names the
// AO thread the review's transcript will arrive on.
func TestStartCodexReviewSendsTheTargetInline(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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

// TestStartCodexReviewRefusesADetachedAnswer: we asked for inline, so a
// different review thread id means the transcript lands on a thread this
// session does not own and is quarantined. Returning success would hand the UI
// a billed turn it can never show.
func TestStartCodexReviewRefusesADetachedAnswer(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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
}

// TestCompactCodexThreadDrivesTheRPC — the response body is empty, so the only
// evidence the binding did anything is the request itself.
func TestCompactCodexThreadDrivesTheRPC(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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

// TestCodexReviewBindingsRefuseWithoutALiveCodexSession: both bindings steer an
// EXISTING conversation, so a thread with no process behind it is a clear
// user-facing refusal — never a spawn the user did not ask for.
func TestCodexReviewBindingsRefuseWithoutALiveCodexSession(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	thread := newCodexThreadForReviewTest(t, app, "thread-codex-no-session")

	if _, err := app.StartCodexReview(context.Background(), thread.ID, CodexReviewTarget{Kind: "uncommittedChanges"}); err == nil {
		t.Fatal("StartCodexReview must refuse a thread with no live session")
	} else if !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("error = %v, want a no-active-session refusal", err)
	}
	if err := app.CompactCodexThread(context.Background(), thread.ID); err == nil {
		t.Fatal("CompactCodexThread must refuse a thread with no live session")
	} else if !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("error = %v, want a no-active-session refusal", err)
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
	app.triage = triage.NewRouter(app.store, func(string, any) {})

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
