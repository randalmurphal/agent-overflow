package diffsummary

import "testing"

func TestParseGitNameStatusNumstat(t *testing.T) {
	got := ParseGitNameStatusNumstat(
		"A\x00src/new.go\x00M\x00src/edit.go\x00D\x00src/old.go\x00",
		"12\t0\tsrc/new.go\x005\t3\tsrc/edit.go\x000\t7\tsrc/old.go\x00-\t-\tassets/logo.png\x00",
	)

	want := []File{
		{Path: "src/new.go", Kind: "added", Additions: 12},
		{Path: "src/edit.go", Kind: "modified", Additions: 5, Deletions: 3},
		{Path: "src/old.go", Kind: "deleted", Deletions: 7},
		{Path: "assets/logo.png", Kind: "modified"},
	}

	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("file[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
