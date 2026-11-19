package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
)

// promptReuseFamily is one kind of prompt a person answers: how the mock
// CLI asks it, how the pane lists it, and an answer to it that the CLI's
// stdin shows by its mark.
type promptReuseFamily struct {
	name   string
	ask    func(command string) string
	open   func(pending provider.PendingInteractiveRequests) []provider.PromptOccurrence
	answer func(a *App, threadID string, occurrence provider.PromptOccurrence, mark string) error
}

var promptReuseFamilies = []promptReuseFamily{
	{
		name: "approval",
		ask: func(command string) string {
			return `{"type":"control_request","request_id":"req-7","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"` + command + `"}}}`
		},
		open: func(pending provider.PendingInteractiveRequests) (out []provider.PromptOccurrence) {
			for _, request := range pending.Approvals {
				out = append(out, request.Occurrence)
			}
			return out
		},
		answer: func(a *App, threadID string, occurrence provider.PromptOccurrence, mark string) error {
			return a.RespondToApproval(context.Background(), threadID, provider.ApprovalResponse{
				RequestID: "req-7", Occurrence: occurrence, Decision: "accept",
				UpdatedInput: json.RawMessage(`{"command":"` + mark + `"}`),
			})
		},
	},
	{
		name: "question",
		ask: func(command string) string {
			return `{"type":"control_request","request_id":"req-7","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Run ` + command + `?","header":"Target","options":[{"label":"yes"},{"label":"no"}],"multiSelect":false}]}}}`
		},
		open: func(pending provider.PendingInteractiveRequests) (out []provider.PromptOccurrence) {
			for _, request := range pending.UserInputs {
				out = append(out, request.Occurrence)
			}
			return out
		},
		answer: func(a *App, threadID string, occurrence provider.PromptOccurrence, mark string) error {
			return a.RespondToUserInput(context.Background(), threadID, provider.UserInputResponse{
				RequestID: "req-7", Occurrence: occurrence, Decision: "accept",
				Answers: map[string]provider.UserInputAnswer{"Target": {mark}},
			})
		},
	},
}

// A person's answer names the prompt their screen showed. The CLI asks again
// under the same request id, in the same session once the first prompt is
// answered and in a replacement session after the first one ends; a late
// answer from a screen still showing an earlier prompt is refused and never
// reaches the CLI, and the open prompt stays open for its own answer.
func TestAUsersAnswerIsWrittenOnlyToTheSessionThatAsked(t *testing.T) {
	t.Parallel()
	for _, family := range promptReuseFamilies {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.UpdateThreadRuntimeMode(context.Background(), thread.ID, string(provider.RuntimeApprovalRequired)); err != nil {
				t.Fatal(err)
			}
			// Each process asks on the user's message, and asks again under
			// the same id once its first prompt is answered. stdin keeps
			// every answer that reached a CLI.
			dir := t.TempDir()
			stdin := filepath.Join(dir, "answers.ndjson")
			init := `{"type":"system","subtype":"init","session_id":"sess-reuse","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`
			script := "#!/bin/bash\nanswers=0\nwhile IFS= read -r line; do\n  case \"$line\" in\n" +
				"    *'\"type\":\"control_response\"'*)\n      printf '%s\\n' \"$line\" >> '" + stdin + "'\n      answers=$((answers+1))\n" +
				"      if [ $answers -eq 1 ]; then printf '%s\\n' '" + family.ask("make destroy") + "'; fi\n      ;;\n" +
				"    *'\"type\":\"user\"'*)\n      printf '%s\\n' '" + init + "'\n      printf '%s\\n' '" + family.ask("make deploy") + "'\n      ;;\n" +
				"  esac\ndone\nexit 0\n"
			binary := mockexec.WriteIn(t, dir, "mock-claude-reuse.sh", script)
			if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
				t.Fatal(err)
			}
			start := func(message string) {
				t.Helper()
				if err := app.StartSession(thread.ID); err != nil {
					t.Fatalf("StartSession: %v", err)
				}
				if err := app.SendMessage(context.Background(), thread.ID, message, nil); err != nil {
					t.Fatalf("SendMessage: %v", err)
				}
			}
			t.Cleanup(func() { _ = app.StopSession(thread.ID) })
			openOther := func(not provider.PromptOccurrence) provider.PromptOccurrence {
				t.Helper()
				var open provider.PromptOccurrence
				waitUntil(t, 10*time.Second, func() bool {
					occurrences := family.open(app.triage.PendingInteractiveRequests(thread.ID))
					if len(occurrences) == 1 && occurrences[0] != not {
						open = occurrences[0]
					}
					return open != 0
				})
				return open
			}
			stillOpen := func(want provider.PromptOccurrence, after string) {
				t.Helper()
				if open := family.open(app.triage.PendingInteractiveRequests(thread.ID)); len(open) != 1 || open[0] != want {
					t.Fatalf("open prompts %v after %s, want only %v", open, after, want)
				}
			}
			late := func(occurrence provider.PromptOccurrence, mark string) {
				t.Helper()
				if err := family.answer(app, thread.ID, occurrence, mark); !errors.Is(err, transport.ErrAlreadyHandled) {
					t.Fatalf("a late answer to prompt %v: %v, want already handled", occurrence, err)
				}
			}

			start("go")
			first := openOther(0)
			// Another screen answers the first prompt; the CLI asks again.
			if err := family.answer(app, thread.ID, first, "answer-first"); err != nil {
				t.Fatalf("the first prompt's answer: %v", err)
			}
			second := openOther(first)
			late(first, "late-first")
			stillOpen(second, "a late answer to the first prompt")

			// The session ends with the second prompt open; a replacement asks
			// under the same id.
			if err := app.StopSession(thread.ID); err != nil {
				t.Fatalf("StopSession: %v", err)
			}
			waitUntil(t, 10*time.Second, func() bool {
				return len(family.open(app.triage.PendingInteractiveRequests(thread.ID))) == 0
			})
			start("go again")
			third := openOther(second)
			late(second, "late-second")
			late(first, "late-first-again")
			stillOpen(third, "late answers to the ended session's prompts")
			if err := family.answer(app, thread.ID, third, "answer-third"); err != nil {
				t.Fatalf("the replacement's prompt's answer: %v", err)
			}

			var written string
			waitUntil(t, 10*time.Second, func() bool {
				raw, err := os.ReadFile(stdin)
				written = string(raw)
				return err == nil && strings.Contains(written, "answer-third")
			})
			if !strings.Contains(written, "answer-first") || strings.Contains(written, "late-") {
				t.Fatalf("answers that reached a CLI:\n%s\nwant the first and third prompts' own answers only", written)
			}
		})
	}
}
