package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A `file` attachment is delivered as one line appended to the PROVIDER
// content, never as an image block and never on the persisted row. These
// tests watch the real wire — the outbound `user` envelope written to the
// CLI's stdin — because that line exists nowhere else: the store keeps the
// attachment in `meta`, not as text.
//
// Every entry point shares one envelope (resolveUserMessageEnvelope), so
// what these prove per-path is that each one ships `providerContent` and
// not `content`. The mixed shape (image, file, image) is the one that can
// go wrong quietly: `[Image #N]` markers are numbered over the IMAGE
// subset, so a file between two images must not consume a number.

// capturedUserEnvelopeBlocks returns the content blocks of every outbound
// `user` envelope, as (type, text) pairs — an image block contributes
// ("image", "") so the caller can assert positional binding.
func capturedUserEnvelopeBlocks(t *testing.T, capturePath string) [][][2]string {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read capture: %v", err)
	}
	var out [][][2]string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var envelope struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil || envelope.Type != "user" {
			continue
		}
		blocks := make([][2]string, 0, len(envelope.Message.Content))
		for _, block := range envelope.Message.Content {
			blocks = append(blocks, [2]string{block.Type, block.Text})
		}
		out = append(out, blocks)
	}
	return out
}

func waitForCapturedUserEnvelopes(t *testing.T, capturePath string, want int) [][][2]string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		envelopes := capturedUserEnvelopeBlocks(t, capturePath)
		if len(envelopes) >= want {
			return envelopes
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d outbound user messages; got %d", want, len(envelopes))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mixedTurnFixture seeds an image, a file, and a second image on one
// thread, and returns the ids in that order plus the prompt line the file
// must produce.
func mixedTurnFixture(t *testing.T, app *App, threadID string) (ids []string, fileLine string) {
	t.Helper()
	first := uploadTestAttachment(t, app, threadID, "one.png", "image/png", tinyPNG())
	file := uploadTestAttachment(t, app, threadID, "report.pdf", "application/pdf", []byte("%PDF-1.7\n"))
	second := uploadTestAttachment(t, app, threadID, "two.png", "image/png", tinyPNG())
	_, path, err := app.attachments.PathForThread(threadID, file.ID)
	if err != nil {
		t.Fatalf("PathForThread: %v", err)
	}
	return []string{first.ID, file.ID, second.ID}, attachment.PromptLine(file, path)
}

// assertMixedTurnOnTheWire pins the whole contract for one outbound
// envelope: exactly two image blocks, positioned where the composer put
// the markers, and the file line trailing the text with no marker of its
// own. The markers themselves are consumed by the split.
func assertMixedTurnOnTheWire(t *testing.T, blocks [][2]string, fileLine string) {
	t.Helper()
	var kinds []string
	var text strings.Builder
	for _, block := range blocks {
		kinds = append(kinds, block[0])
		if block[0] == "text" {
			text.WriteString(block[1])
		}
	}
	// "look at [Image #1] and [Image #2]" splits into text/image/text/image;
	// the trailing file line rides a fifth, text block. Three attachments,
	// two image blocks: the file consumed no marker and no slot.
	if want := "text,image,text,image,text"; strings.Join(kinds, ",") != want {
		t.Fatalf("block kinds = %v, want %v (a file must not become an image block)", kinds, want)
	}
	if want := mixedTurnWireText + "\n\n" + fileLine; text.String() != want {
		t.Fatalf("wire text =\n%q\nwant\n%q", text.String(), want)
	}
	if strings.Contains(fileLine, "[Image #") {
		t.Fatal("the file line must carry no image marker")
	}
}

// newMixedTurnApp is the flush-queue-capable App plus an attachment store,
// which the queue fixture does not wire on its own.
func newMixedTurnApp(t *testing.T) *App {
	t.Helper()
	app, _ := newAppForFlushQueueRPC(t)
	attStore, err := attachment.NewStore(attachment.Config{RootDir: filepath.Join(t.TempDir(), "attachments")}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attStore
	return app
}

func newMixedTurnThread(t *testing.T, app *App, id string) (store.Thread, string) {
	t.Helper()
	thread := newClaudeThreadForProviderCommandTest(t, app, id)
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	installCapturingClaudeSession(t, app, thread, capturePath)
	return thread, capturePath
}

const mixedTurnMessage = "look at [Image #1] and [Image #2]"

// mixedTurnWireText is mixedTurnMessage after the marker split consumes
// both markers — what actually reaches the CLI as text.
const mixedTurnWireText = "look at  and "

func TestSendMessage_MixedAttachmentTurnReachesTheWire(t *testing.T) {
	app := newMixedTurnApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-mixed-send")
	ids, fileLine := mixedTurnFixture(t, app, thread.ID)

	if err := app.SendMessage(thread.ID, mixedTurnMessage, ids); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	envelopes := waitForCapturedUserEnvelopes(t, capturePath, 1)
	assertMixedTurnOnTheWire(t, envelopes[0], fileLine)

	// The persisted row carries the attachment in meta, not as text.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" {
			userItem = item
			break
		}
	}
	if userItem.ID == "" {
		t.Fatal("expected a persisted user item")
	}
	if userItem.Summary != mixedTurnMessage {
		t.Fatalf("persisted summary = %q, want the typed text with no file line", userItem.Summary)
	}
	meta, err := usermessageMetaFromItem(userItem)
	if err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if len(meta.Attachments) != 3 {
		t.Fatalf("meta attachments = %d, want 3", len(meta.Attachments))
	}
	if meta.Attachments[1].Kind != store.AttachmentKindFile {
		t.Fatalf("meta attachment 1 kind = %q, want file", meta.Attachments[1].Kind)
	}
}

// The queued-flush dispatch re-runs the envelope from the queue payload,
// so it is its own chance to ship `content` instead of `providerContent`.
func TestQueuedFlush_MixedAttachmentTurnReachesTheWire(t *testing.T) {
	app := newMixedTurnApp(t)
	thread, capturePath := newMixedTurnThread(t, app, "thread-mixed-flush")
	ids, fileLine := mixedTurnFixture(t, app, thread.ID)

	// An open turn is what makes the message queue rather than send.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("EventTurnStart: %v", err)
	}
	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, mixedTurnMessage, SendMessageOptions{AttachmentIDs: ids}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	envelopes := waitForCapturedUserEnvelopes(t, capturePath, 1)
	assertMixedTurnOnTheWire(t, envelopes[0], fileLine)
}

// Edit-and-resend rebuilds the turn from the edited text plus the same
// attachment ids and dispatches it through sendMessageLocked — the third
// entry point, and the one that reaches the provider from inside the
// thread-action lock. It runs on the saga's own fixture (a sliceable
// session file and an idle two-turn thread) with the capturing CLI
// installed as the binary the resend spawns.
func TestRevertAndResend_MixedAttachmentTurnReachesTheWire(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-mixed")

	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeStdinCapturingClaudeBinary(t, capturePath),
	}); err != nil {
		t.Fatalf("install capturing claude binary: %v", err)
	}

	ids, fileLine := mixedTurnFixture(t, app, thread.ID)
	if err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{
		Content:       mixedTurnMessage,
		AttachmentIDs: ids,
	}); err != nil {
		t.Fatalf("RevertConversationAndResendMessage: %v", err)
	}
	envelopes := waitForCapturedUserEnvelopes(t, capturePath, 1)
	assertMixedTurnOnTheWire(t, envelopes[0], fileLine)
}

// The provider slice is the image subset by construction, so neither
// provider's turn builder can ever be handed a file. This is the tripwire
// for that: it asserts on the resolve, which is the one place a file could
// be let into the slice.
func TestResolveSendMessageAttachments_NeverPutsAFileInTheProviderSlice(t *testing.T) {
	app := newMixedTurnApp(t)
	thread := newClaudeThreadForProviderCommandTest(t, app, "thread-no-file-in-slice")
	ids, _ := mixedTurnFixture(t, app, thread.ID)

	resolved, err := app.resolveSendMessageAttachments(thread.ID, ids)
	if err != nil {
		t.Fatalf("resolveSendMessageAttachments: %v", err)
	}
	if len(resolved.images) != 2 {
		t.Fatalf("images = %d, want 2", len(resolved.images))
	}
	for _, image := range resolved.images {
		if !strings.HasPrefix(image.MimeType, "image/") {
			t.Fatalf("provider slice carries a non-image: %+v", image)
		}
	}
	if len(resolved.fileLines) != 1 {
		t.Fatalf("fileLines = %d, want 1", len(resolved.fileLines))
	}
	if len(resolved.records) != 3 {
		t.Fatalf("records = %d, want 3 (the meta keeps both kinds)", len(resolved.records))
	}
}

// usermessageMetaFromItem is the decode the timeline does.
func usermessageMetaFromItem(item store.Item) (userMessageMeta, error) {
	var meta userMessageMeta
	err := json.Unmarshal([]byte(item.Meta), &meta)
	return meta, err
}

// Claude gets `--add-dir <attachmentsRoot>` on every spawn so a Read of an
// attached file never raises a permission prompt for a path outside the
// workspace. This asserts the whole chain — attachment store root → App
// stamp → claude.Config → buildArgs → argv — by recording the real argv
// the spawned binary was given.
func TestStartSession_ClaudeGetsAddDirForTheAttachmentsRoot(t *testing.T) {
	app, _ := setupE2EApp(t)

	root := filepath.Join(t.TempDir(), "attachments")
	attStore, err := attachment.NewStore(attachment.Config{RootDir: root}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attStore

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	binary := filepath.Join(t.TempDir(), "claude-argv.sh")
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\" >> " + argvPath + "; done\ncat >/dev/null\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write argv-recording binary: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(argvPath)
		if err == nil {
			argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			for i, arg := range argv {
				if arg == "--add-dir" && i+1 < len(argv) && argv[i+1] == root {
					return
				}
			}
			if len(argv) > 0 && time.Now().After(deadline) {
				t.Fatalf("argv has no `--add-dir %s`: %v", root, argv)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the spawned argv (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
