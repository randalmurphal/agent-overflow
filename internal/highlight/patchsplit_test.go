package highlight

import (
	"reflect"
	"strings"
	"testing"
)

// SplitPatchFiles must mirror the frontend parsePatchFiles +
// patchTextOf pair byte-for-byte: Path is what ensureFileKey sees on
// file.path, Patch is the '\n'-join of file.lines[].content. The
// frontend side is pinned by patchFiles.test.ts; these vectors mirror
// its behaviors.
func TestSplitPatchFiles(t *testing.T) {
	modified := strings.Join([]string{
		"diff --git a/src/app.py b/src/app.py",
		"index 111..222 100644",
		"--- a/src/app.py",
		"+++ b/src/app.py",
		"@@ -1,2 +1,2 @@",
		"-old",
		"+new",
		" ctx",
	}, "\n")
	added := strings.Join([]string{
		"diff --git a/docs/new.md b/docs/new.md",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/docs/new.md",
		"@@ -0,0 +1,1 @@",
		"+hello",
	}, "\n")
	deleted := strings.Join([]string{
		"diff --git a/gone.txt b/gone.txt",
		"deleted file mode 100644",
		"--- a/gone.txt",
		"+++ /dev/null",
		"@@ -1,1 +0,0 @@",
		"-bye",
	}, "\n")
	renamed := strings.Join([]string{
		"diff --git a/old/name.go b/new/name.go",
		"similarity index 98%",
		"rename from old/name.go",
		"rename to new/name.go",
		"--- a/old/name.go",
		"+++ b/new/name.go",
		"@@ -3,1 +3,1 @@",
		"-x := 1",
		"+x := 2",
	}, "\n")

	cases := []struct {
		name  string
		patch string
		want  []PatchFileSeed
	}{
		{
			name:  "multi-file split preserves each segment verbatim",
			patch: modified + "\n" + added,
			want: []PatchFileSeed{
				{Path: "src/app.py", Patch: modified},
				{Path: "docs/new.md", Patch: added},
			},
		},
		{
			name:  "trailing newline drops only the final empty line",
			patch: modified + "\n",
			want:  []PatchFileSeed{{Path: "src/app.py", Patch: modified}},
		},
		{
			name:  "double trailing newline keeps one empty content line",
			patch: modified + "\n\n",
			want:  []PatchFileSeed{{Path: "src/app.py", Patch: modified + "\n"}},
		},
		{
			name:  "deleted file keeps the b-side opener path past +++ /dev/null",
			patch: deleted,
			want:  []PatchFileSeed{{Path: "gone.txt", Patch: deleted}},
		},
		{
			name:  "rename resolves to the new path",
			patch: renamed,
			want:  []PatchFileSeed{{Path: "new/name.go", Patch: renamed}},
		},
		{
			name:  "content before the first opener is ignored",
			patch: "commit 123abc\nAuthor: someone\n\n" + added,
			want:  []PatchFileSeed{{Path: "docs/new.md", Patch: added}},
		},
		{
			name:  "quoted +++ path is unquoted and unprefixed",
			patch: "diff --git \"a/sp ace.txt\" \"b/sp ace.txt\"\n--- \"a/sp ace.txt\"\n+++ \"b/sp ace.txt\"\n@@ -1 +1 @@\n-a\n+b",
			want: []PatchFileSeed{{
				Path:  "sp ace.txt",
				Patch: "diff --git \"a/sp ace.txt\" \"b/sp ace.txt\"\n--- \"a/sp ace.txt\"\n+++ \"b/sp ace.txt\"\n@@ -1 +1 @@\n-a\n+b",
			}},
		},
		{
			// "diff --git " with a trailing space IS an opener in the
			// frontend parser; its path stays empty (no 4th token, no
			// +++ override), so finish() drops the segment.
			name:  "opener with no path tokens and no later override is dropped",
			patch: "diff --git \n@@ -1 +1 @@\n-a\n+b\n" + added,
			want:  []PatchFileSeed{{Path: "docs/new.md", Patch: added}},
		},
		{
			name:  "empty and blank input",
			patch: "  \n ",
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitPatchFiles(tc.patch)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitPatchFiles mismatch:\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestCleanPatchPath(t *testing.T) {
	cases := map[string]string{
		`b/x.go`:       "x.go",
		`a/x.go`:       "x.go",
		`"b/sp ace"`:   "sp ace",
		`plain.txt`:    "plain.txt",
		`"quoted"`:     "quoted",
		`/dev/null`:    "/dev/null",
		`ab/keep.txt`:  "ab/keep.txt",
		``:             "",
		`"`:            "",
		`b/a/nested.c`: "a/nested.c",
	}
	for in, want := range cases {
		if got := cleanPatchPath(in); got != want {
			t.Errorf("cleanPatchPath(%q) = %q, want %q", in, got, want)
		}
	}
}
