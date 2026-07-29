package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/usermessage"
)

// Send-time expansion of composer slash commands (D31). Two layers:
// expandComposerCommand as a pure-ish transition table over what the user
// typed, and the real send path proving the split — the block reaches the
// provider's stdin and nothing but the typed text reaches SQLite.

func TestExpandComposerCommandTransitions(t *testing.T) {
	app, _ := newComposerTestApp(t)
	thread := composerSeedThread(t, app, "thr-expand", "")

	// A command then a non-command then the same command again: expansion is
	// resolved per message with no state carried between them, so the
	// sequence matters as much as the individual cases.
	steps := []struct {
		name        string
		content     string
		wantCommand string
		wantBlock   bool
	}{
		{"command with an instruction", "/workflow start the release", "workflow", true},
		{"plain text after a command", "just carry on", "", false},
		{"the same command again", "/workflow start the release", "workflow", true},
		{"bare command", "/workflow", "workflow", true},
		{"unknown command is ordinary text", "/deploy now", "", false},
		{"a longer word is not the command", "/workflows are nice", "", false},
		{"a path is not a command", "/tmp/scratch has the log", "", false},
		// D31 as amended: the word counts wherever it sits.
		{"mid-message command", "look at /workflow later", "workflow", true},
		{"command after a newline", "here is the plan\n/workflow", "workflow", true},
		{"unregistered word does not shadow a later command", "check /tmp then /workflow", "workflow", true},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			expanded, command, err := app.expandComposerCommand(thread.ID, step.content)
			if err != nil {
				t.Fatalf("expandComposerCommand: %v", err)
			}
			if command != step.wantCommand {
				t.Fatalf("command = %q, want %q", command, step.wantCommand)
			}
			if !strings.HasPrefix(expanded, step.content) {
				t.Fatalf("expansion dropped or reordered the typed text: %q", expanded)
			}
			hasBlock := strings.Contains(expanded, "Agent Overflow workflows are available")
			if hasBlock != step.wantBlock {
				t.Fatalf("block present = %v, want %v (payload: %q)", hasBlock, step.wantBlock, expanded)
			}
			if !step.wantBlock && expanded != step.content {
				t.Fatalf("non-command payload changed: %q → %q", step.content, expanded)
			}
			if step.wantBlock {
				// Typed text first (it is the instruction), block second (it
				// is context), separated by a blank line.
				if !strings.HasPrefix(expanded, step.content+"\n\nAgent Overflow workflows") {
					t.Fatalf("block is not appended directly after the typed text: %q", expanded)
				}
			}
		})
	}
}

// TestExpandComposerCommandFiresOnceForRepeats pins the accepted consequence of
// matching at any word position (D31, amended): a message that names the same
// command three times still gets ONE block. The composer colours all three
// words, because every one of them is live; the payload carries the context
// once, because context repeated is only cost.
func TestExpandComposerCommandFiresOnceForRepeats(t *testing.T) {
	app, _ := newComposerTestApp(t)
	thread := composerSeedThread(t, app, "thr-expand-repeat", "")

	const content = "/workflow now, /workflow again, and /workflow once more"
	expanded, command, err := app.expandComposerCommand(thread.ID, content)
	if err != nil {
		t.Fatalf("expandComposerCommand: %v", err)
	}
	if command != "workflow" {
		t.Fatalf("command = %q, want %q", command, "workflow")
	}
	if got := strings.Count(expanded, "Agent Overflow workflows are available"); got != 1 {
		t.Fatalf("block appended %d times, want 1 (payload: %q)", got, expanded)
	}
	if !strings.HasPrefix(expanded, content+"\n\n") {
		t.Fatalf("typed text was rewritten: %q", expanded)
	}
}

func TestExpandComposerCommandFailsLoudly(t *testing.T) {
	app, _ := newComposerTestApp(t)

	// The resolver reads the thread; a thread that does not exist is a
	// resolution failure, and the caller must see it rather than a payload
	// quietly missing the context the user asked for.
	if _, _, err := app.expandComposerCommand("no-such-thread", "/workflow go"); err == nil {
		t.Fatal("expandComposerCommand succeeded for an unknown thread")
	} else if !strings.Contains(err.Error(), "expand /workflow") {
		t.Fatalf("error does not name the command that failed: %v", err)
	}

	// A message that invokes nothing never touches the resolver, so the same
	// unknown thread is not an error on that path.
	expanded, command, err := app.expandComposerCommand("no-such-thread", "just a message")
	if err != nil || command != "" || expanded != "just a message" {
		t.Fatalf("non-command send disturbed by a bad thread: %q, %q, %v", expanded, command, err)
	}
}

// TestSendMessageExpandsComposerCommandOnTheWireOnly drives the real send
// path against a Claude session whose binary captures stdin, so the assertion
// is on the bytes the provider received — not on a proxy for them.
func TestSendMessageExpandsComposerCommandOnTheWireOnly(t *testing.T) {
	app, _ := newComposerTestApp(t)
	thread := composerSeedThread(t, app, "thr-command-send", "")

	capturePath := filepath.Join(t.TempDir(), "stdin.log")
	if err := os.WriteFile(capturePath, nil, 0o644); err != nil {
		t.Fatalf("touch capture: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeClaudeStdinCapture(t, capturePath), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "tok", claude: sess}

	const commandText = "/workflow start the release run"
	const plainText = "and then tell me what happened"
	// The bound wire method, not the internal helper: expansion is an
	// explicit opt-in that only the composer-facing entry points set.
	if err := app.SendMessage(thread.ID, commandText, nil); err != nil {
		t.Fatalf("send command message: %v", err)
	}
	if err := app.SendMessage(thread.ID, plainText, nil); err != nil {
		t.Fatalf("send plain message: %v", err)
	}
	_ = sess.Close()

	sent := readCapturedUserTexts(t, capturePath, 2)
	if !strings.HasPrefix(sent[0], commandText+"\n\n") {
		t.Fatalf("provider payload does not open with the typed text: %q", sent[0])
	}
	if !strings.Contains(sent[0], "Agent Overflow workflows are available") {
		t.Fatalf("provider payload is missing the expanded block: %q", sent[0])
	}
	if sent[1] != plainText {
		t.Fatalf("a non-command message was rewritten: %q", sent[1])
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItems []string
	var metas []string
	for _, item := range items {
		if item.Kind != "user_text" {
			continue
		}
		userItems = append(userItems, item.Summary)
		meta, err := usermessage.FromItem(item)
		if err != nil {
			t.Fatalf("FromItem: %v", err)
		}
		metas = append(metas, meta.Command)
	}
	if len(userItems) != 2 {
		t.Fatalf("want 2 persisted user rows, got %d (%q)", len(userItems), userItems)
	}
	if userItems[0] != commandText {
		t.Fatalf("stored row kept more than the user typed: %q", userItems[0])
	}
	if userItems[1] != plainText {
		t.Fatalf("second stored row = %q, want %q", userItems[1], plainText)
	}
	if metas[0] != "workflow" {
		t.Fatalf("expanded row meta.command = %q, want %q", metas[0], "workflow")
	}
	if metas[1] != "" {
		t.Fatalf("a message that invoked nothing was marked as a command: %q", metas[1])
	}
}

// TestSendMessageSkipsExpansionForInjectedText proves the app's own injectors
// keep their text byte-for-byte: expansion is an explicit opt-in
// (ExpandComposerCommands) that only the composer-facing wire entry points
// set, so an internal caller — here the workflow wake's PreserveDraft shape —
// never expands a `/…` opener inside a prompt no user typed.
func TestSendMessageSkipsExpansionForInjectedText(t *testing.T) {
	app, _ := newComposerTestApp(t)
	thread := composerSeedThread(t, app, "thr-command-injected", "")

	capturePath := filepath.Join(t.TempDir(), "stdin.log")
	if err := os.WriteFile(capturePath, nil, 0o644); err != nil {
		t.Fatalf("touch capture: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeClaudeStdinCapture(t, capturePath), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "tok", claude: sess}

	const injected = "/workflow was mentioned by a run that woke this thread"
	if _, err := app.sendMessageWithOptions(thread.ID, injected, sendMessageOptions{PreserveDraft: true}); err != nil {
		t.Fatalf("sendMessageWithOptions: %v", err)
	}
	_ = sess.Close()

	sent := readCapturedUserTexts(t, capturePath, 1)
	if sent[0] != injected {
		t.Fatalf("injected text was expanded: %q", sent[0])
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.Kind != "user_text" {
			continue
		}
		meta, err := usermessage.FromItem(item)
		if err != nil {
			t.Fatalf("FromItem: %v", err)
		}
		if meta.Command != "" {
			t.Fatalf("injected row was marked as a command: %q", meta.Command)
		}
	}
}

// readCapturedUserTexts parses the mock binary's stdin capture and returns the
// text of each user envelope in wire order, waiting for `want` of them.
func readCapturedUserTexts(t *testing.T, capturePath string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var texts []string
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(capturePath)
		if err != nil {
			t.Fatalf("read capture: %v", err)
		}
		texts = texts[:0]
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var env struct {
				Type    string `json:"type"`
				Message struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(line), &env) != nil || env.Type != "user" {
				continue
			}
			var parts []string
			for _, block := range env.Message.Content {
				if block.Type == "text" {
					parts = append(parts, block.Text)
				}
			}
			texts = append(texts, strings.Join(parts, ""))
		}
		if len(texts) >= want {
			return texts
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("captured %d user envelopes, want %d", len(texts), want)
	return nil
}
