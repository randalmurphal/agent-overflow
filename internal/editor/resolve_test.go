package editor

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePath exercises ResolvePath directly so each branch has
// coverage independent of Open's spawn pipeline. Open-driven tests
// only verify the spawn-side argv shape; this table pins the resolver
// contract so future Open callers can rely on the documented behavior.
//
// The workspace is a real (existing) directory: the openability rule
// stats the resolved target, so a fabricated workspace path would
// silently route every case through the missing-target branch and pin
// nothing about the existing-target rules.
func TestResolvePath(t *testing.T) {
	ws := t.TempDir()

	cases := []struct {
		name      string
		path      string
		workspace string
		want      string
		wantErr   string
	}{
		{
			name: "absolute canonical passes through",
			path: "/etc/hosts",
			want: "/etc/hosts",
		},
		{
			name:    "absolute non-canonical rejected",
			path:    "/foo/../etc/passwd",
			wantErr: "canonical",
		},
		{
			name:      "relative joined against workspace",
			path:      "subdir/file.go",
			workspace: ws,
			want:      filepath.Join(ws, "subdir", "file.go"),
		},
		{
			name:    "relative without workspace rejected",
			path:    "subdir/file.go",
			wantErr: "workspacePath",
		},
		{
			name:      "non-absolute workspace rejected",
			path:      "file.go",
			workspace: "relative/ws",
			wantErr:   "workspacePath must be absolute",
		},
		{
			name:      "trailing-slash workspace rejected as non-canonical",
			path:      "file.go",
			workspace: ws + string(filepath.Separator),
			wantErr:   "workspacePath must be canonical",
		},
		{
			name:      "UNC workspace rejected before any stat",
			path:      "file.go",
			workspace: `\\evil-host\share`,
			wantErr:   "network share",
		},
		{
			name:    "empty path rejected",
			path:    "",
			wantErr: "path is required",
		},
		{
			// Escaping the workspace is allowed only onto an existing
			// regular file (the 2026-08-18 file-only carve-out); an
			// escape to a nonexistent target stays refused.
			name:      "traversal escape to missing target rejected",
			path:      "../../../no-such-dir-xq/passwd",
			workspace: ws,
			wantErr:   "outside the workspace",
		},
		{
			name:      "traversal that resolves inside is allowed",
			path:      "subdir/../other.go",
			workspace: ws,
			want:      filepath.Join(ws, "other.go"),
		},
		{
			// The workspace root itself is a directory, and directory
			// opens are refused even inside the workspace — a folder
			// open can execute workspace config the model authored.
			name:      "dot refused as a directory open",
			path:      ".",
			workspace: ws,
			wantErr:   "not a regular file",
		},
		{
			name:      "filename starting with dots is not a parent reference",
			path:      "..foo",
			workspace: ws,
			want:      filepath.Join(ws, "..foo"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePath(tc.path, tc.workspace)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none (got=%q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error to contain %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ResolvePath(%q, %q) = %q, want %q", tc.path, tc.workspace, got, tc.want)
			}
		})
	}
}
