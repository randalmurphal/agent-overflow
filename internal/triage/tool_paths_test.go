package triage

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestExtractToolPaths_ClaudeFilePathTools(t *testing.T) {
	cases := []struct {
		name string
		tool string
		want []string
	}{
		{"Edit", "Edit", []string{"src/foo.go"}},
		{"Write", "Write", []string{"src/foo.go"}},
		{"MultiEdit", "MultiEdit", []string{"src/foo.go"}},
		{"NotebookEdit", "NotebookEdit", []string{"src/foo.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := mustJSON(t, map[string]any{
				"toolName": tc.tool,
				"input":    map[string]any{"file_path": "src/foo.go"},
			})
			evt := provider.ProviderEvent{
				ItemType: tc.tool,
				Meta:     meta,
			}
			got := extractToolPaths(evt)
			if len(got) != 1 || got[0] != tc.want[0] {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractToolPaths_ClaudeBashIsIgnored(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "rm -rf /"},
	})
	evt := provider.ProviderEvent{ItemType: "Bash", Meta: meta}
	if got := extractToolPaths(evt); got != nil {
		t.Errorf("Bash should be untracked, got %v", got)
	}
}

func TestExtractToolPaths_ClaudeReadIsIgnored(t *testing.T) {
	// Read carries file_path but is read-only — must not be tracked.
	meta := mustJSON(t, map[string]any{
		"toolName": "Read",
		"input":    map[string]any{"file_path": "src/foo.go"},
	})
	evt := provider.ProviderEvent{ItemType: "Read", Meta: meta}
	if got := extractToolPaths(evt); got != nil {
		t.Errorf("Read should be untracked, got %v", got)
	}
}

func TestExtractToolPaths_ClaudeMissingInput(t *testing.T) {
	meta := mustJSON(t, map[string]any{"toolName": "Edit"})
	evt := provider.ProviderEvent{ItemType: "Edit", Meta: meta}
	if got := extractToolPaths(evt); got != nil {
		t.Errorf("missing input should yield nil, got %v", got)
	}
}

func TestExtractToolPaths_CodexFileChangeAdd(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"item": map[string]any{
			"type": "fileChange",
			"changes": []map[string]any{
				{"path": "new.go", "kind": map[string]any{"type": "add"}, "diff": ""},
			},
		},
	})
	evt := provider.ProviderEvent{ItemType: "fileChange", Meta: meta}
	got := extractToolPaths(evt)
	if len(got) != 1 || got[0] != "new.go" {
		t.Errorf("got %v, want [new.go]", got)
	}
}

func TestExtractToolPaths_CodexFileChangeDelete(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"item": map[string]any{
			"type": "fileChange",
			"changes": []map[string]any{
				{"path": "old.go", "kind": map[string]any{"type": "delete"}, "diff": ""},
			},
		},
	})
	evt := provider.ProviderEvent{ItemType: "fileChange", Meta: meta}
	got := extractToolPaths(evt)
	if len(got) != 1 || got[0] != "old.go" {
		t.Errorf("got %v, want [old.go]", got)
	}
}

func TestExtractToolPaths_CodexFileChangeUpdateWithRename(t *testing.T) {
	// kind.move_path tracks BOTH paths so the restore can remove the old
	// file (when it didn't exist at the checkpoint) and restore the new
	// one (or vice versa, depending on the direction of the revert).
	meta := mustJSON(t, map[string]any{
		"item": map[string]any{
			"type": "fileChange",
			"changes": []map[string]any{
				{
					"path": "old.go",
					"kind": map[string]any{"type": "update", "move_path": "new.go"},
					"diff": "",
				},
			},
		},
	})
	evt := provider.ProviderEvent{ItemType: "fileChange", Meta: meta}
	got := extractToolPaths(evt)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 entries", got)
	}
	want := map[string]bool{"old.go": true, "new.go": true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q in result", p)
		}
	}
}

func TestExtractToolPaths_CodexLegacyFileChangeKind(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"item": map[string]any{
			"type": "file_change",
			"changes": []map[string]any{
				{"path": "legacy.go", "kind": map[string]any{"type": "add"}},
			},
		},
	})
	evt := provider.ProviderEvent{ItemType: "file_change", Meta: meta}
	got := extractToolPaths(evt)
	if len(got) != 1 || got[0] != "legacy.go" {
		t.Errorf("got %v, want [legacy.go]", got)
	}
}

func TestExtractToolPaths_UnknownItemTypeIgnored(t *testing.T) {
	evt := provider.ProviderEvent{ItemType: "WebSearch", Meta: mustJSON(t, map[string]any{"query": "go"})}
	if got := extractToolPaths(evt); got != nil {
		t.Errorf("unknown tool should yield nil, got %v", got)
	}
}

func TestNormalizeWorkspaceRelativePaths(t *testing.T) {
	workspace := "/work/project"
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "absolute path inside workspace",
			in:   []string{"/work/project/src/foo.go"},
			want: []string{"src/foo.go"},
		},
		{
			name: "already relative",
			in:   []string{"src/foo.go"},
			want: []string{"src/foo.go"},
		},
		{
			name: "dedups across raw shapes",
			in:   []string{"/work/project/src/foo.go", "src/foo.go"},
			want: []string{"src/foo.go"},
		},
		{
			name: "drops paths escaping workspace",
			in:   []string{"/etc/passwd", "../outside.go"},
			want: nil,
		},
		{
			name: "drops .git and anything inside it",
			in:   []string{".git", ".git/config", ".git/hooks/pre-commit"},
			want: nil,
		},
		{
			name: "drops git pathspec magic prefix",
			in:   []string{":!important.go", ":(literal)foo.go", ":^excluded.go"},
			want: nil,
		},
		{
			name: "drops paths with NUL or control bytes",
			in:   []string{"foo\x00.go", "bar\x01.go", "baz\x1f.go"},
			want: nil,
		},
		{
			name: "drops empties",
			in:   []string{"", "   "},
			want: nil,
		},
		{
			name: "sorted ascending",
			in:   []string{"z.go", "a.go", "m.go"},
			want: []string{"a.go", "m.go", "z.go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeWorkspaceRelativePaths(tc.in, workspace)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}

	if got := normalizeWorkspaceRelativePaths([]string{"/work/project/src/foo.go"}, ""); got != nil {
		t.Fatalf("absolute path without workspace = %v, want nil", got)
	}
}

func TestToolCallSucceeded(t *testing.T) {
	cases := []struct {
		name string
		meta json.RawMessage
		want bool
	}{
		{"empty meta defaults to success", nil, true},
		{"explicit is_error false", mustJSON(t, map[string]any{"is_error": false}), true},
		{"is_error true", mustJSON(t, map[string]any{"is_error": true}), false},
		{"codex item_status completed", mustJSON(t, map[string]any{"item_status": "completed"}), true},
		{"codex item_status failed", mustJSON(t, map[string]any{"item_status": "failed"}), false},
		{"codex item_status cancelled", mustJSON(t, map[string]any{"item_status": "cancelled"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt := provider.ProviderEvent{Meta: tc.meta}
			if got := toolCallSucceeded(evt); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
