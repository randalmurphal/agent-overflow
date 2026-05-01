package editor

import (
	"strings"
	"testing"
)

// TestResolvePath exercises ResolvePath directly so each branch has
// coverage independent of Open's spawn pipeline. Open-driven tests
// only verify the spawn-side argv shape; this table pins the resolver
// contract so future Open callers can rely on the documented behavior.
func TestResolvePath(t *testing.T) {
	const ws = "/home/user/repo"

	cases := []struct {
		name      string
		path      string
		workspace string
		want      string
		wantErr   string
	}{
		{
			name:    "absolute canonical passes through",
			path:    "/etc/hosts",
			want:    "/etc/hosts",
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
			want:      ws + "/subdir/file.go",
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
			workspace: "/home/user/repo/",
			wantErr:   "workspacePath must be canonical",
		},
		{
			name:    "empty path rejected",
			path:    "",
			wantErr: "path is required",
		},
		{
			name:      "traversal escape rejected",
			path:      "../../../etc/passwd",
			workspace: ws,
			wantErr:   "escapes workspace",
		},
		{
			name:      "traversal that resolves inside is allowed",
			path:      "subdir/../other.go",
			workspace: ws,
			want:      ws + "/other.go",
		},
		{
			name:      "dot resolves to workspace root",
			path:      ".",
			workspace: ws,
			want:      ws,
		},
		{
			name:      "filename starting with dots is not a parent reference",
			path:      "..foo",
			workspace: ws,
			want:      ws + "/..foo",
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
