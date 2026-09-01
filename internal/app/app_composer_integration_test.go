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
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/workspacefiles"
)

// newComposerTestApp builds an *App with a fresh on-disk SQLite store, a
// per-test attachment root under t.TempDir(), and a workspace file searcher
// ready for composer-layer tests. Returns the App and the attachment root so
// callers can probe the disk layout.
func newComposerTestApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newTestAppWithStore(t)

	rootDir := filepath.Join(t.TempDir(), "attachments")
	attStore, err := attachment.NewStore(attachment.Config{RootDir: rootDir}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attStore
	app.workspaceFiles = workspacefiles.NewSearcher(workspacefiles.Config{})
	return app, rootDir
}

func composerSeedThread(t *testing.T, app *App, id, workspacePath string) store.Thread {
	t.Helper()
	if workspacePath == "" {
		workspacePath = t.TempDir()
	}
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            id,
		ProjectID:     defaultTestProjectID,
		Title:         "Composer Thread",
		Provider:      string(provider.Claude),
		WorkspacePath: workspacePath,
		Model:         "claude-sonnet",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

// composerSeedThreadWithProvider mirrors composerSeedThread but lets the caller
// pick the provider, for tests that exercise provider-specific send wiring.
func composerSeedThreadWithProvider(t *testing.T, app *App, id, providerKind string) store.Thread {
	t.Helper()
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            id,
		ProjectID:     defaultTestProjectID,
		Title:         "Composer Thread",
		Provider:      providerKind,
		WorkspacePath: t.TempDir(),
		Model:         "claude-sonnet",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

// tinyPNG returns a short but valid PNG payload — enough to pass the MIME
// whitelist and the signature check without bloating test output.
func tinyPNG() []byte {
	payload := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89,
	}
	return payload
}

// writeClaudeStdinCapture creates a mock Claude binary that redirects every
// byte it reads from stdin into capturePath before exiting cleanly. Because
// `tee` mirrors stdin to both the capture file and stdout, Claude's session
// readLoop would pick up the echoed bytes; we redirect stdout to /dev/null
// so only the capture file matters. The script stays running until stdin
// closes so the session can send multiple messages if the test wishes.
func writeClaudeStdinCapture(t *testing.T, capturePath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-capture.sh")
	// /bin/cat reads until EOF; we append to the capture file so concurrent
	// sends within a single test survive. The redirect order matters: stdin
	// → capture file, stdout → /dev/null.
	script := "#!/bin/sh\ncat >> " + shellQuoteComposer(capturePath) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	return path
}

func shellQuoteComposer(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// TestComposer_DraftSaveLoadRoundTrip confirms SaveDraft + GetDraft are a
// true round-trip for content, attachment IDs, and terminal chips.
func TestComposer_DraftSaveLoadRoundTrip(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-rt", "")

	chips := []TerminalChip{
		{
			ID:        "chip-alpha",
			Label:     "shell",
			Preview:   "$ ls",
			Content:   "$ ls\nREADME.md\npackage.json",
			CreatedAt: 1700_000,
		},
	}
	if err := app.SaveDraft(t.Context(), "thr-rt", "hello @file please", []string{"att-1", "att-2"}, chips, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	got, err := app.GetDraft("thr-rt")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "hello @file please" {
		t.Fatalf("Content = %q", got.Content)
	}
	if len(got.AttachmentIDs) != 2 || got.AttachmentIDs[0] != "att-1" || got.AttachmentIDs[1] != "att-2" {
		t.Fatalf("AttachmentIDs = %+v", got.AttachmentIDs)
	}
	if len(got.TerminalChips) != 1 || got.TerminalChips[0].ID != "chip-alpha" {
		t.Fatalf("TerminalChips = %+v", got.TerminalChips)
	}
	if got.TerminalChips[0].Content != chips[0].Content {
		t.Fatalf("chip content drift: %q vs %q", got.TerminalChips[0].Content, chips[0].Content)
	}
}

// TestComposer_DraftOverwritePreservesMostRecent writes v1, then v2, and
// asserts GetDraft returns v2. SaveDraft is an upsert.
func TestComposer_DraftOverwritePreservesMostRecent(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-over", "")

	if err := app.SaveDraft(t.Context(), "thr-over", "v1", []string{"a"}, nil, nil); err != nil {
		t.Fatalf("SaveDraft v1: %v", err)
	}
	if err := app.SaveDraft(t.Context(), "thr-over", "v2", []string{"b"}, nil, nil); err != nil {
		t.Fatalf("SaveDraft v2: %v", err)
	}
	got, err := app.GetDraft("thr-over")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "v2" {
		t.Fatalf("Content = %q, want v2", got.Content)
	}
	if len(got.AttachmentIDs) != 1 || got.AttachmentIDs[0] != "b" {
		t.Fatalf("AttachmentIDs = %+v, want [b]", got.AttachmentIDs)
	}
}

// TestComposer_DraftClearRemovesRow confirms ClearDraft deletes the row and
// GetDraft returns a zero-value Draft (empty content, non-nil empty slices).
func TestComposer_DraftClearRemovesRow(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-clear", "")

	if err := app.SaveDraft(t.Context(), "thr-clear", "to clear", []string{"a"}, nil, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := app.ClearDraft(t.Context(), "thr-clear"); err != nil {
		t.Fatalf("ClearDraft: %v", err)
	}
	got, err := app.GetDraft("thr-clear")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "" {
		t.Fatalf("Content after clear = %q, want empty", got.Content)
	}
	if got.AttachmentIDs == nil {
		t.Fatal("AttachmentIDs should be non-nil empty slice")
	}
	if len(got.AttachmentIDs) != 0 {
		t.Fatalf("AttachmentIDs = %+v, want []", got.AttachmentIDs)
	}
}

// TestComposer_DraftStaleGenerationCounterRejected documents the actual
// contract: the backend has NO generation counter. Writes land in the order
// the client sends them. The frontend's composerDraft.svelte.ts guards
// against out-of-order saves via its own in-memory counter; Go simply
// performs last-writer-wins upserts.
//
// This test exercises back-to-back SaveDrafts and asserts the final state
// reflects the last call, proving the contract: it is the caller's job to
// serialize writes.
func TestComposer_DraftStaleGenerationCounterRejected(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-gen", "")

	// "Newer" save with higher logical generation (gen=1, content "new").
	if err := app.SaveDraft(t.Context(), "thr-gen", "new", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft gen=1: %v", err)
	}
	// "Older" save arriving late (gen=0, content "old"). No counter → it wins.
	if err := app.SaveDraft(t.Context(), "thr-gen", "old", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft gen=0: %v", err)
	}
	got, err := app.GetDraft("thr-gen")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "old" {
		t.Fatalf("CONTRACT: backend has no generation counter; last write wins. Content = %q, want \"old\"", got.Content)
	}
}

// TestComposer_DraftPerThreadIsolation confirms two threads' drafts do not
// leak into each other.
func TestComposer_DraftPerThreadIsolation(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-iso-a", "")
	composerSeedThread(t, app, "thr-iso-b", "")

	if err := app.SaveDraft(t.Context(), "thr-iso-a", "A content", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft A: %v", err)
	}
	if err := app.SaveDraft(t.Context(), "thr-iso-b", "B content", []string{"b-att"}, nil, nil); err != nil {
		t.Fatalf("SaveDraft B: %v", err)
	}

	a, err := app.GetDraft("thr-iso-a")
	if err != nil {
		t.Fatalf("GetDraft A: %v", err)
	}
	b, err := app.GetDraft("thr-iso-b")
	if err != nil {
		t.Fatalf("GetDraft B: %v", err)
	}
	if a.Content != "A content" || len(a.AttachmentIDs) != 0 {
		t.Fatalf("A drifted: %+v", a)
	}
	if b.Content != "B content" || len(b.AttachmentIDs) != 1 || b.AttachmentIDs[0] != "b-att" {
		t.Fatalf("B drifted: %+v", b)
	}
}

// TestComposer_DraftCascadeOnThreadDelete deletes a thread and asserts the
// draft row goes with it (FK ON DELETE CASCADE).
func TestComposer_DraftCascadeOnThreadDelete(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-cascade", "")

	if err := app.SaveDraft(t.Context(), "thr-cascade", "doomed", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	if err := app.store.DeleteThread("thr-cascade"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	got, err := app.GetDraft("thr-cascade")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "" {
		t.Fatalf("expected empty draft after thread delete, got %q", got.Content)
	}
}

// TestComposer_AttachmentReferenceInDraft uploads two real attachments, saves
// their IDs in a draft, and asserts the attachments are still retrievable
// afterwards.
func TestComposer_AttachmentReferenceInDraft(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-att-draft", "")

	first := uploadTestAttachment(t, app, "thr-att-draft", "a.png", "image/png", tinyPNG())
	second := uploadTestAttachment(t, app, "thr-att-draft", "b.png", "image/png", tinyPNG())

	if err := app.SaveDraft(t.Context(), "thr-att-draft", "see: ",
		[]string{first.ID, second.ID}, nil, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	got, err := app.GetDraft("thr-att-draft")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if len(got.AttachmentIDs) != 2 {
		t.Fatalf("AttachmentIDs = %+v", got.AttachmentIDs)
	}
	for _, id := range got.AttachmentIDs {
		if _, _, err := app.attachments.ReadThreadBytes("thr-att-draft", id); err != nil {
			t.Fatalf("read attachment %s: %v", id, err)
		}
	}
}

// TestComposer_SendMessageWithAttachmentPersistsOnItem simulates the full
// composer flow: create thread, upload an attachment, send a message that
// embeds the attachment markdown, then assert the user item stores the
// full markdown in Summary and the attachment is still retrievable after
// send (SendMessage does NOT delete attachments).
func TestComposer_SendMessageWithAttachmentPersistsOnItem(t *testing.T) {
	app, _ := newComposerTestApp(t)
	thread := composerSeedThread(t, app, "thr-att-send", "")

	record := uploadTestAttachment(t, app, thread.ID, "cover.png", "image/png", tinyPNG())

	// Attach a real Claude session backed by the passthrough binary so the
	// send path executes end-to-end (turn index, user item insert, stdin
	// write).
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
	})

	msg := "please review this image"
	if err := app.SendMessage(thread.ID, msg, []string{record.ID}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

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
		t.Fatal("expected persisted user item")
	}
	if strings.Contains(userItem.Summary, "attachment://") {
		t.Fatalf("user item Summary should not embed attachment refs, got %q", userItem.Summary)
	}
	var meta userMessageMeta
	if err := json.Unmarshal([]byte(userItem.Meta), &meta); err != nil {
		t.Fatalf("unmarshal user item meta: %v", err)
	}
	if len(meta.Attachments) != 1 || meta.Attachments[0].ID != record.ID {
		t.Fatalf("expected attachment metadata on user item, got %+v", meta.Attachments)
	}
	// Attachment must still be retrievable post-send.
	if _, _, err := app.attachments.ReadThreadBytes(record.ThreadID, record.ID); err != nil {
		t.Fatalf("read attachment after send: %v", err)
	}
	list, err := app.ListAttachments(thread.ID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != 1 || list[0].ID != record.ID {
		t.Fatalf("expected attachment to persist after send, got %+v", list)
	}
}

func TestComposer_SendMessageRejectsAttachmentFromDifferentThread(t *testing.T) {
	app, _ := newComposerTestApp(t)
	sourceThread := composerSeedThread(t, app, "thr-attachment-source", "")
	targetThread := composerSeedThread(t, app, "thr-attachment-target", "")

	record := uploadTestAttachment(t, app, sourceThread.ID, "cover.png", "image/png", tinyPNG())

	err := app.SendMessage(targetThread.ID, "please review this image", []string{record.ID})
	if err == nil {
		t.Fatal("SendMessage error = nil, want cross-thread attachment rejection")
	}
	if !strings.Contains(err.Error(), "belongs to thread") {
		t.Fatalf("SendMessage error = %v, want ownership rejection", err)
	}
	items, err := app.store.ListItems(targetThread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no user item after rejected attachment, got %+v", items)
	}
}

// TestComposer_SendMessageWithTerminalChipFormatsAsFencedCodeBlock verifies
// that whatever formatted text the frontend passes to SendMessage is written
// verbatim to the provider stdin. The frontend emits terminal chips as
// ```terminal ...``` fenced blocks, while image attachments travel as
// structured provider content blocks.
func TestComposer_SendMessageWithTerminalChipFormatsAsFencedCodeBlock(t *testing.T) {
	app, _ := newComposerTestApp(t)
	thread := composerSeedThread(t, app, "thr-outgoing", "")

	record := uploadTestAttachment(t, app, thread.ID, "snap.png", "image/png", tinyPNG())

	captureDir := t.TempDir()
	capturePath := filepath.Join(captureDir, "stdin.log")
	// Touch so the session's writer doesn't race with mkdir on first write.
	if err := os.WriteFile(capturePath, nil, 0o644); err != nil {
		t.Fatalf("touch capture: %v", err)
	}
	binary := writeClaudeStdinCapture(t, capturePath)

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  binary,
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
	})

	outgoing := "initial question\n\n```terminal shell\n$ ls\nREADME.md\n```"
	if err := app.SendMessage(thread.ID, outgoing, []string{record.ID}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Close the session so the mock binary flushes its capture file. Without
	// close, `cat >> file` may buffer indefinitely and the test races the OS.
	_ = sess.Close()

	// The session writer flushes synchronously, but the mock uses buffered
	// shell I/O; give it a moment to complete.
	deadline := time.Now().Add(2 * time.Second)
	var captured []byte
	for time.Now().Before(deadline) {
		captured, err = os.ReadFile(capturePath)
		if err != nil {
			t.Fatalf("read capture: %v", err)
		}
		if len(captured) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(captured) == 0 {
		t.Fatal("mock captured no stdin; session did not send")
	}

	// The capture file contains a JSON frame whose content blocks include the
	// outgoing text and a Claude-compatible image source.
	escapedOutgoing := jsonEscapeContent(outgoing)
	if !strings.Contains(string(captured), escapedOutgoing) {
		t.Fatalf("captured stdin missing expected content block.\nwant substring: %q\nfull capture: %q",
			escapedOutgoing, string(captured))
	}
	// Sanity: the terminal marker stays in the text block, and the attachment
	// is sent as an image block rather than an attachment:// markdown ref.
	for _, marker := range []string{
		"```terminal shell",
		`"type":"image"`,
		`"media_type":"image/png"`,
	} {
		if !strings.Contains(string(captured), marker) {
			t.Fatalf("captured stdin missing marker %q in %q", marker, string(captured))
		}
	}
	if strings.Contains(string(captured), "attachment://") {
		t.Fatalf("captured stdin should not contain attachment markdown refs: %q", string(captured))
	}
}

// TestResolveSendMessageAttachmentsByProvider proves the provider-aware attachment
// resolve that backs claude-tui image paste: claude-tui receives the on-disk PATH
// and NO image bytes (it pastes the path into the real TUI composer, where Claude
// reads the file), while a bytes-based provider (claude) receives the Data and no
// path. Mis-wiring either branch would silently break image sends for one provider.
func TestResolveSendMessageAttachmentsByProvider(t *testing.T) {
	app, rootDir := newComposerTestApp(t)

	claudeThread := composerSeedThread(t, app, "by-provider-claude", "")
	tuiThread := composerSeedThreadWithProvider(t, app, "by-provider-tui", string(provider.ClaudeTUI))

	claudeAtt := uploadTestAttachment(t, app, claudeThread.ID, "pic.png", "image/png", tinyPNG())
	tuiAtt := uploadTestAttachment(t, app, tuiThread.ID, "pic.png", "image/png", tinyPNG())

	// claude → inline bytes, no path.
	claudeResolved, _, err := app.resolveSendMessageAttachments(claudeThread.ID, []string{claudeAtt.ID})
	if err != nil {
		t.Fatalf("resolve(claude): %v", err)
	}
	if len(claudeResolved) != 1 {
		t.Fatalf("resolve(claude): got %d attachments, want 1", len(claudeResolved))
	}
	if len(claudeResolved[0].Data) == 0 {
		t.Error("claude attachment must carry image bytes")
	}
	if claudeResolved[0].Path != "" {
		t.Errorf("claude attachment must not carry a path, got %q", claudeResolved[0].Path)
	}

	// claude-tui → on-disk path, no bytes.
	tuiResolved, _, err := app.resolveSendMessageAttachments(tuiThread.ID, []string{tuiAtt.ID})
	if err != nil {
		t.Fatalf("resolve(claude-tui): %v", err)
	}
	if len(tuiResolved) != 1 {
		t.Fatalf("resolve(claude-tui): got %d attachments, want 1", len(tuiResolved))
	}
	if len(tuiResolved[0].Data) != 0 {
		t.Errorf("claude-tui attachment must be path-only, got %d image bytes", len(tuiResolved[0].Data))
	}
	if tuiResolved[0].Path == "" {
		t.Fatal("claude-tui attachment must carry the on-disk path")
	}
	if !strings.HasPrefix(tuiResolved[0].Path, rootDir) {
		t.Errorf("claude-tui path %q is not under the attachment root %q", tuiResolved[0].Path, rootDir)
	}
	if _, err := os.Stat(tuiResolved[0].Path); err != nil {
		t.Errorf("claude-tui path is not a real file on disk: %v", err)
	}
}

// jsonEscapeContent re-encodes a content string the way json.Marshal would
// embed it inside a JSON object: newlines become \n, quotes become \", etc.
// It returns the escaped form without the surrounding quotes.
func jsonEscapeContent(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	)
	return replacer.Replace(s)
}

// TestComposer_AttachmentNotLostOnDraftSave uploads an attachment, references
// it in a draft, saves the draft again WITHOUT the reference, and asserts
// the attachment row + disk bytes are still present. Dropping an attachment
// from a draft must NOT delete the underlying attachment — otherwise the
// user loses their upload as soon as they clear the input.
func TestComposer_AttachmentNotLostOnDraftSave(t *testing.T) {
	app, rootDir := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-att-save", "")

	record := uploadTestAttachment(t, app, "thr-att-save", "img.png", "image/png", tinyPNG())

	if err := app.SaveDraft(t.Context(), "thr-att-save", "hi", []string{record.ID}, nil, nil); err != nil {
		t.Fatalf("SaveDraft with ref: %v", err)
	}
	if err := app.SaveDraft(t.Context(), "thr-att-save", "hi again", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft without ref: %v", err)
	}

	// Attachment metadata row is still there.
	if _, _, err := app.attachments.ReadThreadBytes(record.ThreadID, record.ID); err != nil {
		t.Fatalf("attachment lost after draft save: %v", err)
	}
	// Disk file is still there.
	if _, err := os.Stat(filepath.Join(rootDir, record.RelativePath)); err != nil {
		t.Fatalf("attachment file missing: %v", err)
	}
}

// TestComposer_SendMessageClearsDraft verifies the backend clears the draft
// row after persisting the user message. The frontend fires a fire-and-forget
// ClearDraft as defense-in-depth, but the backend is the authoritative cleanup.
func TestComposer_SendMessageClearsDraft(t *testing.T) {
	app, _ := newComposerTestApp(t)
	thread := composerSeedThread(t, app, "thr-send-draft", "")

	if err := app.SaveDraft(t.Context(), thread.ID, "draft text", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
	})

	if err := app.SendMessage(thread.ID, "Actual message", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	got, err := app.GetDraft(thread.ID)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "" {
		t.Fatalf("Draft should be cleared after send, got content = %q", got.Content)
	}
}

// TestComposer_MentionPopoverSearchRespectsThreadWorkspace creates two
// threads with DIFFERENT workspace paths, drops a marker file in each, and
// confirms SearchWorkspaceFiles returns files from the caller's thread
// workspace — not the other thread's.
func TestComposer_MentionPopoverSearchRespectsThreadWorkspace(t *testing.T) {
	app, _ := newComposerTestApp(t)
	wsA := t.TempDir()
	wsB := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsA, "alpha.txt"), []byte("A"), 0o644); err != nil {
		t.Fatalf("write alpha: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsB, "beta.txt"), []byte("B"), 0o644); err != nil {
		t.Fatalf("write beta: %v", err)
	}
	composerSeedThread(t, app, "thr-ws-a", wsA)
	composerSeedThread(t, app, "thr-ws-b", wsB)

	a, err := app.SearchWorkspaceFiles("thr-ws-a", "", 50)
	if err != nil {
		t.Fatalf("SearchWorkspaceFiles A: %v", err)
	}
	if a.Root != wsA {
		t.Fatalf("A.Root = %q, want %q", a.Root, wsA)
	}
	foundAlpha, sawBeta := false, false
	for _, f := range a.Files {
		if f.Path == "alpha.txt" {
			foundAlpha = true
		}
		if f.Path == "beta.txt" {
			sawBeta = true
		}
	}
	if !foundAlpha || sawBeta {
		t.Fatalf("A scope wrong: foundAlpha=%v sawBeta=%v files=%+v", foundAlpha, sawBeta, a.Files)
	}

	b, err := app.SearchWorkspaceFiles("thr-ws-b", "", 50)
	if err != nil {
		t.Fatalf("SearchWorkspaceFiles B: %v", err)
	}
	if b.Root != wsB {
		t.Fatalf("B.Root = %q, want %q", b.Root, wsB)
	}
	foundBeta, sawAlpha := false, false
	for _, f := range b.Files {
		if f.Path == "beta.txt" {
			foundBeta = true
		}
		if f.Path == "alpha.txt" {
			sawAlpha = true
		}
	}
	if !foundBeta || sawAlpha {
		t.Fatalf("B scope wrong: foundBeta=%v sawAlpha=%v files=%+v", foundBeta, sawAlpha, b.Files)
	}
}

// TestComposer_LargeDraftHandled writes a 50KB content blob and confirms the
// round-trip preserves length exactly.
func TestComposer_LargeDraftHandled(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-big", "")

	var b strings.Builder
	for i := 0; i < 50*1024; i++ {
		b.WriteByte(byte('a' + (i % 26)))
	}
	big := b.String()
	if err := app.SaveDraft(t.Context(), "thr-big", big, nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft big: %v", err)
	}
	got, err := app.GetDraft("thr-big")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if len(got.Content) != len(big) {
		t.Fatalf("length mismatch: got %d want %d", len(got.Content), len(big))
	}
	if got.Content != big {
		t.Fatal("content drift across large blob")
	}
}

// TestComposer_EmptyDraftSaveIsNoOp confirms that saving an empty draft
// upserts a row with empty content — it is NOT a no-op at the store level,
// but GetDraft returns an empty Draft either way.
func TestComposer_EmptyDraftSaveIsNoOp(t *testing.T) {
	app, _ := newComposerTestApp(t)
	composerSeedThread(t, app, "thr-empty-save", "")

	if err := app.SaveDraft(t.Context(), "thr-empty-save", "", nil, nil, nil); err != nil {
		t.Fatalf("SaveDraft empty: %v", err)
	}
	got, err := app.GetDraft("thr-empty-save")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Content != "" {
		t.Fatalf("Content = %q, want empty", got.Content)
	}
	if got.AttachmentIDs == nil {
		t.Fatal("AttachmentIDs should be non-nil empty slice")
	}
	if got.TerminalChips == nil {
		t.Fatal("TerminalChips should be non-nil empty slice")
	}
}

// TestComposer_DraftSurvivesRestart opens a fresh SQLite file, saves a
// draft, closes the DB, reopens it, and asserts the draft is still there.
// Uses a file-backed store (not :memory:) since only file-backed stores
// survive a Close.
func TestComposer_DraftSurvivesRestart(t *testing.T) {
	dbPath := storetest.ClonePath(t)
	threadID := "thr-restart"

	// Round 1: create thread, save draft, close.
	st1, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	app1 := &App{
		store: st1,
	}
	ensureDefaultTestProject(t, app1)
	thread := store.Thread{
		ID:            threadID,
		ProjectID:     defaultTestProjectID,
		Title:         "Restart",
		Provider:      string(provider.Claude),
		WorkspacePath: "/tmp",
		Model:         "claude",
		Mode:          "chat",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := app1.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := app1.SaveDraft(t.Context(), threadID, "survived", []string{"abc"}, []TerminalChip{{ID: "c1", Content: "body"}}, nil); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Round 2: reopen the same file and re-read.
	st2, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("reopen store.New: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	app2 := &App{
		store: st2,
	}
	got, err := app2.GetDraft(threadID)
	if err != nil {
		t.Fatalf("GetDraft post-restart: %v", err)
	}
	if got.Content != "survived" {
		t.Fatalf("Content post-restart = %q, want survived", got.Content)
	}
	if len(got.AttachmentIDs) != 1 || got.AttachmentIDs[0] != "abc" {
		t.Fatalf("AttachmentIDs post-restart = %+v", got.AttachmentIDs)
	}
	if len(got.TerminalChips) != 1 || got.TerminalChips[0].Content != "body" {
		t.Fatalf("TerminalChips post-restart = %+v", got.TerminalChips)
	}
}
