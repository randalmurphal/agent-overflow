package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptSnapshotReadsBranchStatusAndCommits(t *testing.T) {
	dir := seedRepo(t)
	core := NewCore()

	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "UNTRACKED"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot, err := core.PromptSnapshot(dir)
	if err != nil {
		t.Fatalf("PromptSnapshot() error = %v", err)
	}
	if !snapshot.IsRepo {
		t.Fatal("IsRepo = false for a seeded repository")
	}
	if snapshot.Branch != "main" {
		t.Fatalf("Branch = %q, want main", snapshot.Branch)
	}
	// The `## ` header is metadata, not an entry — it must not leak into
	// the status the model reads.
	if strings.Contains(snapshot.ShortStatus, "##") {
		t.Fatalf("ShortStatus carries the branch header: %q", snapshot.ShortStatus)
	}
	if !strings.Contains(snapshot.ShortStatus, "README") ||
		!strings.Contains(snapshot.ShortStatus, "UNTRACKED") {
		t.Fatalf("ShortStatus = %q, want the modified and untracked paths", snapshot.ShortStatus)
	}
	if len(snapshot.RecentCommits) != 1 || !strings.HasSuffix(snapshot.RecentCommits[0], " init") {
		t.Fatalf("RecentCommits = %v, want one `<sha> init` line", snapshot.RecentCommits)
	}
}

func TestPromptSnapshotOnACleanRepoHasNoStatusSection(t *testing.T) {
	snapshot, err := NewCore().PromptSnapshot(seedRepo(t))
	if err != nil {
		t.Fatalf("PromptSnapshot() error = %v", err)
	}
	if snapshot.ShortStatus != "" {
		t.Fatalf("ShortStatus = %q, want empty on a clean tree", snapshot.ShortStatus)
	}
}

// Not being a repository is an ANSWER, not a failure: the caller renders
// an empty git block and the session starts either way.
func TestPromptSnapshotOutsideARepositoryIsNotAnError(t *testing.T) {
	snapshot, err := NewCore().PromptSnapshot(t.TempDir())
	if err != nil {
		t.Fatalf("PromptSnapshot() error = %v, want nil outside a repository", err)
	}
	if snapshot.IsRepo {
		t.Fatal("IsRepo = true outside a repository")
	}
}

func TestPromptSnapshotCapsTheStatusSection(t *testing.T) {
	dir := seedRepo(t)
	for i := 0; i < 400; i++ {
		name := filepath.Join(dir, strings.Repeat("f", 40)+string(rune('a'+i%26))+".txt")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := NewCore().PromptSnapshot(dir)
	if err != nil {
		t.Fatalf("PromptSnapshot() error = %v", err)
	}
	if len(snapshot.ShortStatus) > PromptStatusLimit+len("\n\n[truncated]") {
		t.Fatalf("ShortStatus is %d bytes, want it capped at %d", len(snapshot.ShortStatus), PromptStatusLimit)
	}
}

// splitShortStatus stops accumulating past PromptStatusLimit, so the
// truncated answer must be byte-identical to what an unbounded walk would
// have produced — and the branch header must survive the stop.
func TestSplitShortStatusBoundsAccumulationWithoutChangingTheOutput(t *testing.T) {
	var b strings.Builder
	b.WriteString("## main...origin/main [ahead 1]\n")
	for i := 0; b.Len() < PromptStatusLimit*3; i++ {
		fmt.Fprintf(&b, " M %s-%d.txt\n", strings.Repeat("path", 20), i)
	}
	stdout := b.String()

	branch, status := splitShortStatus(stdout)
	if branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if len(status) <= PromptStatusLimit {
		t.Fatalf("status is %d bytes, want more than the %d-byte cap so limitSection truncates identically", len(status), PromptStatusLimit)
	}
	// The prefix the caller keeps is what must be unchanged.
	want := unboundedSplitShortStatus(stdout)
	if limitSection(status, PromptStatusLimit) != limitSection(want, PromptStatusLimit) {
		t.Fatal("bounded accumulation changed the truncated status output")
	}
	// A header that lands after the cap is still read: git puts it first,
	// but the parser must not depend on that.
	trailing := strings.TrimPrefix(stdout, "## main...origin/main [ahead 1]\n") + "## feature/late\n"
	if branch, _ := splitShortStatus(trailing); branch != "feature/late" {
		t.Fatalf("branch = %q, want the header found past the cap", branch)
	}
}

// unboundedSplitShortStatus is the pre-bound implementation, kept in the
// test as the oracle the bounded one is compared against.
func unboundedSplitShortStatus(stdout string) string {
	var entries []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "## ") || strings.TrimSpace(line) == "" {
			continue
		}
		entries = append(entries, line)
	}
	return strings.Join(entries, "\n")
}

// PromptBlock is what the {{GIT_BLOCK}} placeholder renders to; a section
// with nothing to say is omitted rather than left as an empty heading.
func TestPromptSnapshotPromptBlock(t *testing.T) {
	cases := []struct {
		name     string
		snapshot PromptSnapshot
		want     string
	}{
		{
			name:     "empty snapshot",
			snapshot: PromptSnapshot{},
			want:     "",
		},
		{
			name:     "branch only",
			snapshot: PromptSnapshot{IsRepo: true, Branch: "main"},
			want:     "Current branch: main",
		},
		{
			name:     "branch and status",
			snapshot: PromptSnapshot{IsRepo: true, Branch: "main", ShortStatus: " M a.go\n?? b.go"},
			want:     "Current branch: main\nStatus:\n M a.go\n?? b.go",
		},
		{
			name: "all three sections",
			snapshot: PromptSnapshot{
				IsRepo:        true,
				Branch:        "feature/x",
				ShortStatus:   " M a.go",
				RecentCommits: []string{"abc1234 first", "def5678 second"},
			},
			want: "Current branch: feature/x\nStatus:\n M a.go\nRecent commits:\nabc1234 first\ndef5678 second",
		},
		{
			name:     "detached HEAD has no branch line",
			snapshot: PromptSnapshot{IsRepo: true, RecentCommits: []string{"abc1234 first"}},
			want:     "Recent commits:\nabc1234 first",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snapshot.PromptBlock(); got != tc.want {
				t.Errorf("PromptBlock() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseShortStatusBranchHandlesEveryHeaderShape(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{header: "main", want: "main"},
		{header: "main...origin/main", want: "main"},
		{header: "main...origin/main [ahead 1, behind 2]", want: "main"},
		{header: "No commits yet on main", want: "main"},
		{header: "feature/x...origin/feature/x", want: "feature/x"},
		{header: "HEAD (no branch)", want: ""},
	}
	for _, tc := range cases {
		if got := parseShortStatusBranch(tc.header); got != tc.want {
			t.Errorf("parseShortStatusBranch(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
