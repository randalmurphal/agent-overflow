package trackerapp

import (
	"testing"
)

// A worklog as the mirror read answers it carries the tracker's revision
// through the store and the view unchanged, so an edit and then a delete
// of an untouched worklog, each made from the latest mirror read, are
// applied on Jira's own worklogs and on Tempo's: the writes compare the
// revision (jiraprovider's sameWorklog), and none is refused falsely.
func TestAWorklogAsMirroredIsTheWorklogAsRead(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"jira", "tempo"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			s := newTimeScenario(t)
			want := "20001"
			if source == "tempo" {
				s.addTempoOn(t)
				want = "7001"
			}
			s.refreshed(t, "the refresh", want)
			mirrored := func() WorklogView {
				t.Helper()
				got := must[WorklogDays](t)(s.svc.Worklogs(s.conn.ID, "acc-user1", s.from, s.to))
				if len(got.Worklogs) != 1 || got.Worklogs[0].ID != want || got.Worklogs[0].Updated == 0 {
					t.Fatalf("the mirror reads %+v; want worklog %s with its revision", got.Worklogs, want)
				}
				return got.Worklogs[0]
			}
			spent := int64(5400)
			v := must[WriteView](t)(s.svc.EditWorklog(t.Context(), s.conn.ID, WorklogEditRequest{From: mirrored(), SpentSeconds: &spent}))
			if len(v.Parts) != 1 || v.Parts[0].State != "applied" {
				t.Fatalf("the edit of the worklog as mirrored: %+v", v.Parts)
			}
			edited := mirrored()
			if edited.SpentSeconds != spent {
				t.Fatalf("after the edit the mirror reads %+v", edited)
			}
			v = must[WriteView](t)(s.svc.DeleteWorklog(t.Context(), s.conn.ID, WorklogDeleteRequest{From: edited}))
			if len(v.Parts) != 1 || v.Parts[0].State != "applied" {
				t.Fatalf("the delete of the worklog as mirrored: %+v", v.Parts)
			}
			if ids, unread := s.mirrored(t); len(ids) != 0 || len(unread) != 0 {
				t.Fatalf("after the delete the mirror reads %v unread %v", ids, unread)
			}
		})
	}
}
