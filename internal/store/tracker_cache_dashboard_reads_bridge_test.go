package store

import "testing"

// DashboardReadsFixtureForTest is sprint board 10 with a row in every
// relation of the export, its card blobs of CardShapeForTest's shape:
// WEB-10001 a done Bug, High, labeled frontend, in active sprint 21;
// WEB-10002 an in-progress Task, Low, in sprint 21, with one status change
// by acc-author, who is assigned nothing; WEB-10003 a to-do ticket whose
// blob is of no version the shape reads. Every ticket is acc-user1's.
func DashboardReadsFixtureForTest(t *testing.T) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	st := webStructure()
	st.Catalog.Users = append(st.Catalog.Users, MirrorUser{ID: "acc-author", Name: "Author", Data: []byte(`{"active":true}`)})
	mustReplaceStructure(t, s, root, st, true)
	bug := sectionRow("10001", "a", "10001", 3, "21")
	bug.Card.Data = []byte(`{"v":2,"type_name":"Bug","priority_name":"High","labels":[{"id":"frontend","name":"frontend"}]}`)
	task := sectionRow("10002", "b", "3", 2, "21")
	task.Card.Data = []byte(`{"v":2,"type_name":"Task","priority_name":"Low"}`)
	mustWriteRowPage(t, s, root, webPage("10", bug, task, sectionRow("10003", "c", "1", 1)), nil)
	histories := []MirrorHistory{
		{IssueID: "10002", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 3, AuthorID: "acc-author", From: "1", To: "3"}}},
	}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, histories), MirrorCatalog{},
		mirrorReadOf(root, 10)); err != nil {
		t.Fatal(err)
	}
	return s, root
}
