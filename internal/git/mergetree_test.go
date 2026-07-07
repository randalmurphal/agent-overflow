package git

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestParseMergeTreeNameOnly(t *testing.T) {
	tests := []struct {
		name       string
		stdout     string
		conflicted bool
		want       MergeTreeResult
	}{
		{
			name:       "clean",
			stdout:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
			conflicted: false,
			want: MergeTreeResult{
				Conflicted: false,
				TreeOID:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
		{
			name:       "conflicted",
			stdout:     readTestdata(t, "git-merge-tree-nameonly.txt"),
			conflicted: true,
			want: MergeTreeResult{
				Conflicted: true,
				TreeOID:    "d2a25785a8eb8b7e49ce8638778429710b0d277c",
				Paths:      []string{"main.go"},
				// "Auto-merging main.go" and "CONFLICT (content)" from the
				// fixture are both dropped: the viewer already renders the
				// file with visible conflict regions and a badge.
			},
		},
		{
			name: "structural conflicts attribute to their paths",
			stdout: "d2a25785a8eb8b7e49ce8638778429710b0d277c\n" +
				"main.go\n" +
				"lib/util.go\n" +
				"\n" +
				"Auto-merging main.go\n" +
				"CONFLICT (content): Merge conflict in main.go\n" +
				"CONFLICT (modify/delete): lib/util.go deleted in main and modified in feature. Version feature of lib/util.go left in tree.\n" +
				"warning: something forge-wide happened\n",
			conflicted: true,
			want: MergeTreeResult{
				Conflicted: true,
				TreeOID:    "d2a25785a8eb8b7e49ce8638778429710b0d277c",
				Paths:      []string{"main.go", "lib/util.go"},
				Notes: map[string][]string{
					"lib/util.go": {
						"CONFLICT (modify/delete): lib/util.go deleted in main and modified in feature. Version feature of lib/util.go left in tree.",
					},
				},
				Messages: []string{"warning: something forge-wide happened"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMergeTreeNameOnly(tt.stdout, tt.conflicted)
			if err != nil {
				t.Fatalf("parseMergeTreeNameOnly: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("result = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAttributeMergeMessagesLongestPathWins(t *testing.T) {
	// "a/b.go" is a substring-adjacent trap: the message mentions the
	// nested path, so the longer match must win over "b.go".
	notes, rest := attributeMergeMessages(
		[]string{"CONFLICT (rename/delete): a/b.go renamed in main"},
		[]string{"b.go", "a/b.go"},
	)
	if len(rest) != 0 {
		t.Fatalf("rest = %v, want empty", rest)
	}
	if len(notes["a/b.go"]) != 1 || len(notes["b.go"]) != 0 {
		t.Fatalf("notes = %v, want attribution to a/b.go", notes)
	}
}

func TestShowTreeFileValidation(t *testing.T) {
	core := NewCore()
	validOID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name    string
		treeOID string
		path    string
	}{
		{name: "rev expression", treeOID: "HEAD", path: "main.go"},
		{name: "short oid", treeOID: "abc123", path: "main.go"},
		{name: "uppercase oid", treeOID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", path: "main.go"},
		{name: "empty path", treeOID: validOID, path: ""},
		{name: "nul path", treeOID: validOID, path: "main\x00.go"},
		{name: "flag path", treeOID: validOID, path: "-n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := core.ShowTreeFile("", tc.treeOID, tc.path); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestPRHeadRef(t *testing.T) {
	tests := []struct {
		forge string
		n     int
		want  string
	}{
		{forge: "github", n: 12, want: "pull/12/head"},
		{forge: "gitlab", n: 34, want: "merge-requests/34/head"},
	}
	for _, tt := range tests {
		got, err := PRHeadRef(tt.forge, tt.n)
		if err != nil {
			t.Fatalf("PRHeadRef(%q, %d): %v", tt.forge, tt.n, err)
		}
		if got != tt.want {
			t.Fatalf("PRHeadRef(%q, %d) = %q, want %q", tt.forge, tt.n, got, tt.want)
		}
	}
	if _, err := PRHeadRef("bitbucket", 1); err == nil {
		t.Fatal("expected unknown forge error")
	}
	if _, err := PRHeadRef("github", 0); err == nil {
		t.Fatal("expected non-positive number error")
	}
}

func TestDiffMergeBaseIsThreeDot(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// Diverge: base advances on main, head branches off the initial commit
	// and adds a file. A three-dot diff (merge-base..head) must show ONLY
	// the head's change, never the base-only commit made after divergence.
	testutil.RunGit(t, repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature body\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	testutil.RunGit(t, repo, "add", "feature.txt")
	testutil.RunGit(t, repo, "commit", "-m", "add feature file")

	testutil.RunGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "base-only.txt"), []byte("base body\n"), 0o644); err != nil {
		t.Fatalf("write base-only file: %v", err)
	}
	testutil.RunGit(t, repo, "add", "base-only.txt")
	testutil.RunGit(t, repo, "commit", "-m", "base advances")

	diff, err := core.DiffMergeBase(repo, "main", "feature")
	if err != nil {
		t.Fatalf("DiffMergeBase: %v", err)
	}
	if !strings.Contains(diff, "feature.txt") || !strings.Contains(diff, "+feature body") {
		t.Fatalf("diff missing head change:\n%s", diff)
	}
	if strings.Contains(diff, "base-only.txt") {
		t.Fatalf("three-dot diff leaked the base-only commit:\n%s", diff)
	}
}

func TestDiffMergeBaseValidatesRefs(t *testing.T) {
	core := NewCore()
	if _, err := core.DiffMergeBase("", "", "head"); err == nil {
		t.Fatal("expected error for empty base")
	}
	if _, err := core.DiffMergeBase("", "base", ""); err == nil {
		t.Fatal("expected error for empty head")
	}
}

func TestMergeTreeConflictSequenceUsesFetchHeadImmediately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "git.log")
	gitPath := filepath.Join(binDir, "git")
	headOID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	treeOID := "cccccccccccccccccccccccccccccccccccccccc"
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$AO_GIT_LOG"
case "$*" in
  "fetch origin pull/9/head")
    exit 0
    ;;
  "rev-parse FETCH_HEAD")
    echo "$AO_HEAD_OID"
    exit 0
    ;;
  "fetch origin main")
    exit 0
    ;;
  "merge-tree --write-tree --name-only origin/main $AO_HEAD_OID")
    cat <<EOF
$AO_TREE_OID
main.go

CONFLICT (content): Merge conflict in main.go
EOF
    exit 1
    ;;
  *)
    echo "unexpected args: $*" >&2
    exit 9
    ;;
esac
`
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_GIT_LOG", logPath)
	t.Setenv("AO_HEAD_OID", headOID)
	t.Setenv("AO_TREE_OID", treeOID)

	core := NewCore()
	gotHead, err := core.FetchRefOID("", "origin", "pull/9/head")
	if err != nil {
		t.Fatalf("FetchRefOID: %v", err)
	}
	if gotHead != headOID {
		t.Fatalf("head OID = %q, want %q", gotHead, headOID)
	}
	if err := core.FetchBranch("", "origin", "main"); err != nil {
		t.Fatalf("FetchBranch: %v", err)
	}
	result, err := core.MergeTreeConflicts("", "origin/main", gotHead)
	if err != nil {
		t.Fatalf("MergeTreeConflicts: %v", err)
	}
	if !result.Conflicted || result.TreeOID != treeOID || strings.Join(result.Paths, ",") != "main.go" {
		t.Fatalf("result = %+v", result)
	}

	gotLog := strings.TrimSpace(readFile(t, logPath))
	wantLog := strings.Join([]string{
		"fetch origin pull/9/head",
		"rev-parse FETCH_HEAD",
		"fetch origin main",
		"merge-tree --write-tree --name-only origin/main " + headOID,
	}, "\n")
	if gotLog != wantLog {
		t.Fatalf("git argv log:\n%s\nwant:\n%s", gotLog, wantLog)
	}
}
