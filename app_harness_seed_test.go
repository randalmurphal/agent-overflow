package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness"
)

// TestHarnessSeedRefusesTraversalProjectNames: a generated repo's name
// becomes a directory under <dataRoot>/workspaces — a traversal name
// would create (and later reset-wipe) a repository outside the
// harness-owned tree.
func TestHarnessSeedRefusesTraversalProjectNames(t *testing.T) {
	h, _ := newHarnessTestApp(t)
	for _, name := range []string{"../outside", "a/b", `a\b`, ".", ".."} {
		_, err := h.HarnessSeed(HarnessSeedSpec{Projects: []HarnessSeedProject{{
			Name: name,
			Repo: &harness.RepoSpec{},
		}}})
		if err == nil || !strings.Contains(err.Error(), "plain directory name") {
			t.Fatalf("HarnessSeed(name=%q): err = %v, want plain-directory-name refusal", name, err)
		}
	}
	// "../outside" resolves to <dataRoot>/outside — nothing may exist there.
	if _, err := os.Stat(filepath.Join(h.paths.DataRoot, "outside")); !os.IsNotExist(err) {
		t.Fatalf("traversal seed escaped the workspaces root (stat err %v)", err)
	}
}
