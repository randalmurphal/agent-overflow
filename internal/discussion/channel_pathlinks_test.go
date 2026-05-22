package discussion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// TestChannelPostMessageEnrichesPathRefs verifies that PostMessage
// validates path-shaped tokens in the message content against the
// thread's workspace filesystem and stores the resulting allowlist on
// the persisted channel_messages.meta column. ChannelView's
// ChatMarkdown linkifier consumes that allowlist; without this hook
// it would have nothing to gate on and discussion messages would lose
// path linkification compared to assistant_text.
func TestChannelPostMessageEnrichesPathRefs(t *testing.T) {
	// Seed a workspace with one real file. A bogus path-shaped token
	// in the message body should be rejected by ExtractAndValidate.
	wsRoot := t.TempDir()
	srcDir := filepath.Join(wsRoot, "internal", "agent")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "handler.go"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)

	project := testutil.EnsureProject(t, st, "/tmp/project")
	thread := store.Thread{
		ID:            "thread-pathlinks",
		ProjectID:     project.ID,
		Title:         "Discussion Thread",
		Provider:      "codex",
		WorkspacePath: wsRoot,
		Model:         "gpt-5.4",
		Mode:          "discussion",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "deliberation")
	if err != nil {
		t.Fatalf("Create channel: %v", err)
	}

	msg, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "agent",
		FromID:    "thread-a",
		FromRole:  "proposer",
		Content:   "Look at internal/agent/handler.go and bogus.nope/file.bad",
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if msg.Meta == "" {
		t.Fatalf("expected meta to carry pathRefs, got empty")
	}

	var decoded struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}
	if err := json.Unmarshal([]byte(msg.Meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", msg.Meta, err)
	}
	if len(decoded.PathRefs) != 1 {
		t.Fatalf("expected exactly one validated path ref, got %#v (meta=%q)", decoded.PathRefs, msg.Meta)
	}
	if decoded.PathRefs[0].Path != "internal/agent/handler.go" {
		t.Fatalf("expected internal/agent/handler.go, got %#v", decoded.PathRefs[0])
	}

	// And the persisted row round-trips through ListChannelMessages.
	listed, err := channelSvc.GetMessages(channel.ID, -1, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 message, got %d", len(listed))
	}
	if listed[0].Meta != msg.Meta {
		t.Fatalf("round-trip meta mismatch: insert=%q list=%q", msg.Meta, listed[0].Meta)
	}
}

// TestChannelPostMessageNoPathsLeavesMetaEmpty pins the negative case:
// a message with no path-shaped tokens (or no matches against the
// workspace) persists with empty meta so old rows + new rows look the
// same in the JSON wire shape.
func TestChannelPostMessageNoPathsLeavesMetaEmpty(t *testing.T) {
	wsRoot := t.TempDir()
	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)

	project := testutil.EnsureProject(t, st, "/tmp/project")
	thread := store.Thread{
		ID:            "thread-no-paths",
		ProjectID:     project.ID,
		Title:         "Discussion Thread",
		Provider:      "codex",
		WorkspacePath: wsRoot,
		Model:         "gpt-5.4",
		Mode:          "discussion",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "deliberation")
	if err != nil {
		t.Fatalf("Create channel: %v", err)
	}

	msg, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "agent",
		FromID:    "thread-a",
		Content:   "Plain prose with no file paths or shapes that look like them.",
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if msg.Meta != "" {
		t.Fatalf("expected empty meta for message with no paths, got %q", msg.Meta)
	}
}

// TestChannelPostMessageMissingWorkspaceLeavesMetaEmpty ensures the
// degraded path is safe: a thread with an empty WorkspacePath cannot
// safely run os.Stat, so we skip enrichment entirely rather than fail
// the post or expose an existence oracle.
func TestChannelPostMessageMissingWorkspaceLeavesMetaEmpty(t *testing.T) {
	st := newDiscussionTestStore(t)
	channelSvc := NewChannelService(st)

	project := testutil.EnsureProject(t, st, "/tmp/project")
	thread := store.Thread{
		ID:            "thread-no-ws",
		ProjectID:     project.ID,
		Title:         "Discussion Thread",
		Provider:      "codex",
		WorkspacePath: "",
		Model:         "gpt-5.4",
		Mode:          "discussion",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	channel, err := channelSvc.Create(thread.ID, "deliberation")
	if err != nil {
		t.Fatalf("Create channel: %v", err)
	}

	msg, err := channelSvc.PostMessage(PostMessageInput{
		ChannelID: channel.ID,
		FromType:  "agent",
		FromID:    "thread-a",
		Content:   "Mentions src/foo.ts but workspace is unset",
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if msg.Meta != "" {
		t.Fatalf("expected empty meta when workspace is missing, got %q", msg.Meta)
	}
}
