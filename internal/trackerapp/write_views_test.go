package trackerapp

import (
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// A part whose answer was lost says where its change would show, by what
// the write was: a created issue is searched for, since its board's
// filter may leave it out; a sprint is among the board's sprints, a
// worklog in the day's logged time, a rank in the board's order, and any
// other change on its ticket.
func TestALostWriteSaysWhereToLook(t *testing.T) {
	t.Parallel()
	lost := tracker.Unknown(&trackerapi.TransientError{Err: errors.New("reset")})
	for _, c := range []struct {
		step tracker.Step
		want string
	}{
		{tracker.StepCreate, "the issue may or may not have been created; search the tracker for the issue before you try again"},
		{tracker.StepSprint, "this change may or may not have been made; look at the board's sprints before you try again"},
		{tracker.StepWorklog, "this change may or may not have been made; look at the time you logged that day before you try again"},
		{tracker.StepRank, "this change may or may not have been made; look at the issue's place on the board before you try again"},
		{tracker.StepUpload, "this change may or may not have been made; look at the ticket before you try again"},
		{tracker.StepEdit, "this change may or may not have been made; look at the ticket before you try again"},
		{tracker.StepMove, "this change may or may not have been made; look at the ticket before you try again"},
		{tracker.StepEstimate, "this change may or may not have been made; look at the ticket before you try again"},
		{tracker.StepSprintMove, "this change may or may not have been made; look at the ticket before you try again"},
		{tracker.StepComment, "this change may or may not have been made; look at the ticket before you try again"},
		{tracker.StepLink, "this change may or may not have been made; look at the ticket before you try again"},
	} {
		views := partViews(store.TrackerKindJira, tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: c.step, Outcome: lost}}}, 0)
		f := views[0].Failure
		if f == nil || f.Message != "Jira's answer did not arrive in full, so "+c.want {
			t.Errorf("a lost %s: %+v", c.step, f)
		}
		if !strings.HasSuffix(c.want, LostWriteCheck(c.step)) {
			t.Errorf("a lost %s's check %q is not its sentence's", c.step, LostWriteCheck(c.step))
		}
	}
}
