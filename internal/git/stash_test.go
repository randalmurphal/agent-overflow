package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// HasSuffix-not-Contains is a deliberate security choice in
// findStashRefByMessage: an external `git stash push -m
// "ao-carry-deadbeef wip"` from outside the app must NOT preempt the
// app's own stash entry tagged with the same hex token. Reflog format
// is "On <branch>: <message>" so the marker always sits at the suffix.
// Test: push two stashes whose messages differ only in suffix vs.
// substring positioning, then verify the suffix entry resolves.
func TestFindStashRefByMessagePrefersSuffix(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	marker := "ao-carry-deadbeef"

	// Earlier (= deeper in stack) push contains the marker as a substring
	// — emulates an external `git stash push -m "...ao-carry-deadbeef wip"`.
	if err := os.WriteFile(filepath.Join(repo, "noise.txt"), []byte("noise\n"), 0o644); err != nil {
		t.Fatalf("write noise: %v", err)
	}
	if _, err := core.StashPushIncludeUntracked(repo, marker+" wip"); err != nil {
		t.Fatalf("stash push (substring): %v", err)
	}

	// Later (= shallower in stack) push has the marker exactly at suffix.
	if err := os.WriteFile(filepath.Join(repo, "intended.txt"), []byte("intended\n"), 0o644); err != nil {
		t.Fatalf("write intended: %v", err)
	}
	if _, err := core.StashPushIncludeUntracked(repo, marker); err != nil {
		t.Fatalf("stash push (suffix): %v", err)
	}

	ref, err := core.findStashRefByMessage(repo, marker)
	if err != nil {
		t.Fatalf("findStashRefByMessage: %v", err)
	}
	// The intended (suffix-match) ref is the most recent entry, so it's
	// stash@{0}. The substring entry is stash@{1}; if the lookup ever
	// regresses to Contains-style matching, the deeper substring entry
	// would resolve first because git stash list iterates oldest-last.
	// Resolve the ref to its message and confirm exact suffix match.
	stdout, _, err := core.Execute(repo, "stash", "list", "--format=%gd %s")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	matched := false
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.Index(line, " ")
		if idx <= 0 {
			continue
		}
		if line[:idx] != ref {
			continue
		}
		desc := strings.TrimSpace(line[idx+1:])
		if !strings.HasSuffix(desc, marker) {
			t.Fatalf("resolved ref %s has desc %q; want HasSuffix(%q)", ref, desc, marker)
		}
		if strings.HasSuffix(desc, marker+" wip") {
			t.Fatalf("resolved ref %s pulled the substring entry %q instead of the intended suffix entry", ref, desc)
		}
		matched = true
		break
	}
	if !matched {
		t.Fatalf("ref %s not found in stash list:\n%s", ref, stdout)
	}
}

func TestStashPushIncludeUntrackedReportsCleanTreeAsNotCreated(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	created, err := core.StashPushIncludeUntracked(repo, "ao-carry-empty")
	if err != nil {
		t.Fatalf("StashPushIncludeUntracked on clean tree returned err: %v", err)
	}
	if created {
		t.Fatal("created = true on a clean tree; want false")
	}

	stdout, _, err := core.Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty stash after no-op push; got %q", stdout)
	}
}

func TestStashPushIncludeUntrackedRequiresMessage(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if _, err := core.StashPushIncludeUntracked(repo, "   "); err == nil {
		t.Fatal("expected blank message to be rejected")
	}
}

// Round-trip: push a dirty tree, apply the stash, drop it. The
// apply path verifies findStashRefByMessage works for the happy
// path; the drop path leaves the stash stack empty.
func TestStashApplyAndDropByMessageRoundTrip(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	marker := "ao-carry-roundtrip"

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("touched\n"), 0o644); err != nil {
		t.Fatalf("write tracked: %v", err)
	}
	testutil.RunGit(t, repo, "add", "tracked.txt")
	testutil.RunGit(t, repo, "commit", "-m", "seed tracked")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("modify tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}

	created, err := core.StashPushIncludeUntracked(repo, marker)
	if err != nil {
		t.Fatalf("stash push: %v", err)
	}
	if !created {
		t.Fatal("expected created = true with dirty + untracked tree")
	}

	// After push, working tree should be clean of the staged change AND
	// the untracked file (`-u` carries them all into the stash).
	if data, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err == nil && strings.Contains(string(data), "modified") {
		t.Fatal("tracked.txt still shows modified content after stash push")
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt should have been carried to stash; stat err = %v", err)
	}

	if err := core.StashApplyByMessage(repo, marker); err != nil {
		t.Fatalf("StashApplyByMessage: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
	if err != nil {
		t.Fatalf("read tracked after apply: %v", err)
	}
	if !strings.Contains(string(data), "modified") {
		t.Fatalf("tracked.txt = %q after apply; expected modified", string(data))
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); err != nil {
		t.Fatalf("new.txt should be restored after apply: %v", err)
	}

	if err := core.StashDropByMessage(repo, marker); err != nil {
		t.Fatalf("StashDropByMessage: %v", err)
	}
	stdout, _, err := core.Execute(repo, "stash", "list")
	if err != nil {
		t.Fatalf("stash list: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("stash should be empty after drop; got %q", stdout)
	}
}

func TestStashApplyByMessageReportsMissingEntry(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	err := core.StashApplyByMessage(repo, "ao-carry-nonexistent")
	if err == nil {
		t.Fatal("expected missing-entry error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error %v should mention 'not found' for diagnostic clarity", err)
	}
}

func TestRandomStashSuffixUnique(t *testing.T) {
	// The suffix is uniqueness, not unpredictability — but two consecutive
	// calls must never collide on the hex token, otherwise concurrent
	// carry-overs in the same repo lose their stash-list lookup.
	seen := map[string]struct{}{}
	for range 100 {
		s := RandomStashSuffix()
		if len(s) < 8 {
			t.Fatalf("RandomStashSuffix returned %q, want >=8 hex chars", s)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("RandomStashSuffix collision on %q within 100 calls", s)
		}
		seen[s] = struct{}{}
	}
}
