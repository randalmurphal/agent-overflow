package store

import (
	"strconv"
	"testing"
)

// Proof of the replace's cost (A, B), against the API of eb733523b: a
// replace that removes half of n stored acts and adds n/2 new ones.
func TestProofActsReplaceCost(t *testing.T) {
	steps := map[int]int{}
	for _, n := range []int{1000, 2000, 4000} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a", "21")), []byte("c1"))
		acts := func(from, count int) MirrorIssueActs {
			in := MirrorIssueActs{IssueID: "10001", Source: MirrorActComment, ReadFor: 5}
			for i := range count {
				in.Acts = append(in.Acts, MirrorAct{ID: strconv.Itoa(from + i), Person: "acc-user1", At: 2})
			}
			return in
		}
		if _, _, err := s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 10), []MirrorIssueActs{acts(0, n)}, MirrorCatalog{}); err != nil {
			t.Fatal(err)
		}
		var err error
		_, steps[n] = countStatementRuns(t, mirrorActsRemoveSQL, func() {
			_, _, err = s.ReplaceMirrorActs(root, 0, mirrorReadOf(root, 11), []MirrorIssueActs{acts(n/2, n)}, MirrorCatalog{})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("remove steps: %v", steps)
	if steps[4000] > 5*steps[1000] {
		t.Fatalf("the remove's steps grow faster than its acts: %v", steps)
	}
}
