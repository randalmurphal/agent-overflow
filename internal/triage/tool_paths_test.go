package triage

import "testing"

func TestIsFileChangeItemType(t *testing.T) {
	cases := []struct {
		itemType string
		want     bool
	}{
		{"Edit", true},
		{"Write", true},
		{"MultiEdit", true},
		{"NotebookEdit", true},
		{"fileChange", true},
		{"file_change", true},
		// Read carries file_path but is read-only; Bash has no
		// structured path; ExitPlanMode never writes a file.
		{"Read", false},
		{"Bash", false},
		{"ExitPlanMode", false},
		{"WebSearch", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isFileChangeItemType(tc.itemType); got != tc.want {
			t.Errorf("isFileChangeItemType(%q) = %v, want %v", tc.itemType, got, tc.want)
		}
	}
}

func TestNormalizeWorkspaceRelativePath(t *testing.T) {
	workspace := "/work/project"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"absolute path inside workspace", "/work/project/src/foo.go", "src/foo.go"},
		{"already relative", "src/foo.go", "src/foo.go"},
		{"absolute path outside workspace", "/etc/passwd", ""},
		{"relative escape", "../outside.go", ""},
		{"dot-git root", ".git", ""},
		{"inside dot-git", ".git/hooks/pre-commit", ""},
		{"git pathspec magic", ":!important.go", ""},
		{"NUL byte", "foo\x00.go", ""},
		{"control byte", "bar\x01.go", ""},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"dot", ".", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeWorkspaceRelativePath(tc.in, workspace); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	if got := normalizeWorkspaceRelativePath("/work/project/src/foo.go", ""); got != "" {
		t.Fatalf("absolute path without workspace = %q, want empty", got)
	}
}

func TestNormalizeDisplayPath(t *testing.T) {
	workspace := "/work/project"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"absolute path inside workspace", "/work/project/src/foo.go", "src/foo.go"},
		{"already relative", "src/foo.go", "src/foo.go"},
		// Display paths preserve outside-workspace locations —
		// diffs against /tmp scratch files still render.
		{"absolute path outside workspace", "/tmp/scratch.txt", "/tmp/scratch.txt"},
		{"NUL byte", "foo\x00.go", ""},
		{"empty", "", ""},
		{"dot", ".", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeDisplayPath(tc.in, workspace); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
