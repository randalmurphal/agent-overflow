package store

import "testing"

// PaceFixtureForTest and PaceForTest hand the pace's fixture and the
// Backlog's reading of it to the external test that holds the dashboard's
// pace to it (tracker_cache_dashboard_pace_test.go), which imports
// trackerdash and so cannot be in this package.
func PaceFixtureForTest(t *testing.T) (*Store, int64) { return paceFixture(t) }

func PaceForTest(t *testing.T, s *Store, root int64, board string, regroup MirrorRegroup) []MirrorSprintFinished {
	return paceOf(t, s, root, board, regroup)
}

// ScaleMirrorForTest hands the scale fixture of one board of issues
// issues (benchChanges changes each), its root and its board, to the
// external test that measures a dashboard over it
// (tracker_cache_dashboard_scale_test.go).
func ScaleMirrorForTest(tb testing.TB, issues int) (*Store, int64, string) {
	s, root, _ := openSizedMirror(tb, benchMirrorSize{boards: 1, issues: issues})
	return s, root, benchBoardID(0)
}
