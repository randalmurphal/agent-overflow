package claude

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// captureSentUserLine runs one Send against a throwaway process that echoes
// stdin to a file and returns the single NDJSON line the session wrote. Same
// shape as TestSessionSend's capture — no provider binary is involved.
func captureSentUserLine(t *testing.T, content string, opts provider.SendOptions) []byte {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	if err := s.Send(context.Background(), content, opts); err != nil {
		t.Fatalf("Send: %v", err)
	}
	lines := waitCapturedLines(t, capturePath, 1)
	return []byte(lines[0])
}

func TestGuardOutboundSlashCommand(t *testing.T) {
	cases := []struct {
		name    string
		content string
		guard   bool
		want    string
	}{
		{
			// The active bug: AO's own /workflow built-in was being swallowed
			// by the CLI's command router ("Unknown command: /workflow",
			// num_turns 0) and the model never saw the appended block.
			name:    "ao workflow command with appended block",
			content: "/workflow run nightly\n\n[appended block]",
			guard:   true,
			want:    "\n/workflow run nightly\n\n[appended block]",
		},
		{
			name:    "native command passes raw",
			content: "/usage",
			want:    "/usage",
		},
		{
			name:    "unknown command keeps native router semantics",
			content: "/zzz-not-a-real-command",
			want:    "/zzz-not-a-real-command",
		},
		{
			name:    "plugin-prefixed name",
			content: "/release-tools:ship-it 1.2.0",
			want:    "/release-tools:ship-it 1.2.0",
		},
		{
			// Interior slash: the CLI cannot resolve this as a command name,
			// so guarding it would rewrite the user's prose for nothing.
			name:    "leading absolute path is prose",
			content: "/etc/hosts on this box has a stale entry",
			want:    "/etc/hosts on this box has a stale entry",
		},
		{
			name:    "mid-message slash untouched",
			content: "run /usage after this",
			want:    "run /usage after this",
		},
		{
			name:    "slash alone is not a command",
			content: "/ is the root directory",
			want:    "/ is the root directory",
		},
		{
			name:    "lone slash",
			content: "/",
			want:    "/",
		},
		{
			// Already prose to the CLI (startsWith('/') is false), so the
			// guard has nothing to fix.
			name:    "leading whitespace already defeats routing",
			content: " /usage",
			want:    " /usage",
		},
		{
			name:    "punctuation right after the name is not a command",
			content: "/usage. what does it show?",
			want:    "/usage. what does it show?",
		},
		{
			name:    "empty content",
			content: "",
			want:    "",
		},
		{
			name:    "guard does not alter prose",
			content: "/etc/hosts is fine",
			guard:   true,
			want:    "/etc/hosts is fine",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := guardOutboundSlashCommand(tc.content, tc.guard); got != tc.want {
				t.Fatalf("guardOutboundSlashCommand(%q, %v) = %q, want %q", tc.content, tc.guard, got, tc.want)
			}
		})
	}
}

// TestGuardOutboundSlashCommandTransitions covers the on->off->on sequence an
// AO composer command introduces. The guard is a pure function of its
// arguments, so no forced-prose send may leave residue behind.
func TestGuardOutboundSlashCommandTransitions(t *testing.T) {
	const content = "/workflow run nightly"
	sequence := []struct {
		guard bool
		want  string
	}{
		{guard: false, want: content},
		{guard: true, want: "\n" + content},
		{guard: false, want: content},
		{guard: true, want: "\n" + content},
	}
	for i, step := range sequence {
		if got := guardOutboundSlashCommand(content, step.guard); got != step.want {
			t.Fatalf("step %d (guard=%v): got %q, want %q", i, step.guard, got, step.want)
		}
	}
}

func TestBuildUserMessageBlocksGuardsSlashCommand(t *testing.T) {
	blocks, err := buildUserMessageBlocks("/workflow run nightly\n\n[appended block]", nil, true)
	if err != nil {
		t.Fatalf("buildUserMessageBlocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	text, _ := blocks[0]["text"].(string)
	if !strings.HasPrefix(text, "\n/workflow") {
		t.Fatalf("text = %q, want a leading newline before the command", text)
	}
}

func TestBuildUserMessageBlocksPreservesNativeSlashCommand(t *testing.T) {
	blocks, err := buildUserMessageBlocks("/usage", nil, false)
	if err != nil {
		t.Fatalf("buildUserMessageBlocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if text, _ := blocks[0]["text"].(string); text != "/usage" {
		t.Fatalf("text = %q, want the raw command", text)
	}
}

// TestBuildUserMessageBlocksGuardComposesWithImageMarkers pins the composition
// rule: the guard prefixes the WHOLE content before the image split, so marker
// offsets are computed against the string that actually goes out and inline
// placement is unchanged.
func TestBuildUserMessageBlocksGuardComposesWithImageMarkers(t *testing.T) {
	attachments := []provider.ImageAttachment{
		{ID: "a", MimeType: "image/png", Data: []byte("first")},
		{ID: "b", MimeType: "image/png", Data: []byte("second")},
	}
	blocks, err := buildUserMessageBlocks("/workflow run [Image #1] then [Image #2] please", attachments, true)
	if err != nil {
		t.Fatalf("buildUserMessageBlocks: %v", err)
	}
	var kinds []string
	for _, block := range blocks {
		kind, _ := block["type"].(string)
		kinds = append(kinds, kind)
	}
	want := []string{"text", "image", "text", "image", "text"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("block kinds = %v, want %v", kinds, want)
	}
	if text, _ := blocks[0]["text"].(string); text != "\n/workflow run " {
		t.Fatalf("first text block = %q, want the guarded prefix", text)
	}
	if text, _ := blocks[2]["text"].(string); text != " then " {
		t.Fatalf("middle text block = %q", text)
	}
}

// TestBuildUserMessageBlocksGuardsWhenMessageOpensWithAnImage proves the guard
// must run before the split rather than on the first text PART: with an image
// marker first, the first text part does not start the message and prefixing it
// would leave the CLI's router looking at a `/` it still claims.
func TestBuildUserMessageBlocksGuardsWhenMessageOpensWithAnImage(t *testing.T) {
	attachments := []provider.ImageAttachment{{ID: "a", MimeType: "image/png", Data: []byte("x")}}
	blocks, err := buildUserMessageBlocks("[Image #1]/usage", attachments, true)
	if err != nil {
		t.Fatalf("buildUserMessageBlocks: %v", err)
	}
	// The assembled message starts with "[", not "/", so the CLI never routes
	// it and the guard correctly leaves it alone.
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if kind, _ := blocks[0]["type"].(string); kind != "image" {
		t.Fatalf("first block = %q, want image", kind)
	}
	if text, _ := blocks[1]["text"].(string); text != "/usage" {
		t.Fatalf("trailing text = %q, want the unmodified remainder", text)
	}
}

// TestSendGuardsSlashCommandOnTheWire walks the full Send path so the guard is
// pinned where it actually matters: the JSON line written to the CLI's stdin.
func TestSendGuardsSlashCommandOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guard      bool
		wantPrefix string
	}{
		{name: "native", guard: false, wantPrefix: "/workflow run nightly"},
		{name: "ao expanded", guard: true, wantPrefix: "\n/workflow run nightly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := captureSentUserLine(t, "/workflow run nightly", provider.SendOptions{
				GuardClaudeSlashCommand: tc.guard,
			})
			var envelope struct {
				Message struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &envelope); err != nil {
				t.Fatalf("unmarshal outbound line: %v", err)
			}
			if len(envelope.Message.Content) != 1 {
				t.Fatalf("content blocks = %d, want 1", len(envelope.Message.Content))
			}
			if got := envelope.Message.Content[0].Text; got != tc.wantPrefix {
				t.Fatalf("outbound text = %q, want %q", got, tc.wantPrefix)
			}
		})
	}
}
