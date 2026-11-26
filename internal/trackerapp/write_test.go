package trackerapp

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

func states(v WriteView) []string {
	out := make([]string, len(v.Parts))
	for i, p := range v.Parts {
		out[i] = p.Issue + " " + p.Step + " " + p.State
	}
	return out
}

// A write's outcomes come back as they are, and only the parts that are
// applied or unknown are read again: a ranked issue's row on the board it
// was ranked on, joined with its card's read through every board the
// mirror has it on, one rows read per board.
func TestWriteOutcomes(t *testing.T) {
	t.Parallel()
	stale := tracker.Refusal{Reason: tracker.ReasonStale, Message: "the neighbors moved", Err: errors.New("moved")}
	cases := []struct {
		name     string
		outcomes []tracker.Outcome
		states   []string
		reads    []string
	}{
		{"all applied", []tracker.Outcome{tracker.Applied(), tracker.Applied(), tracker.Applied()},
			[]string{"10001 rank applied", "10002 rank applied", "10003 rank applied"},
			[]string{"1:[10001 10002 10003]", "2:[10001]"}},
		{"all refused", []tracker.Outcome{tracker.Refused(stale), tracker.Refused(stale), tracker.Refused(stale)},
			[]string{"10001 rank refused", "10002 rank refused", "10003 rank refused"}, nil},
		{"mixed", []tracker.Outcome{tracker.Applied(), tracker.Refused(stale), tracker.NotSent(errors.New("an earlier part failed"))},
			[]string{"10001 rank applied", "10002 rank refused", "10003 rank not_sent"},
			[]string{"1:[10001]", "2:[10001]"}},
		{"unknown", []tracker.Outcome{tracker.Unknown(&trackerapi.TransientError{Err: errors.New("reset")}), tracker.Applied(), tracker.Applied()},
			[]string{"10001 rank unknown", "10002 rank applied", "10003 rank applied"},
			[]string{"1:[10001 10002 10003]", "2:[10001]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newWriteFixture(t)
			reads := f.state.recordRows(f.p)
			var res tracker.Result
			for i, id := range []string{"10001", "10002", "10003"} {
				res.Parts = append(res.Parts, rankPart(testBoard, id, c.outcomes[i]))
				if c.outcomes[i].State() == tracker.StateApplied {
					f.state.set(testBoard, writeRow(id, 1, "z"+id))
				}
			}
			f.answer(res)
			before := len(f.p.callsInOrder())
			v, err := f.svc.Rank(t.Context(), f.conn.ID, RankRequest{Board: "1", Issues: []string{"10001", "10002", "10003"}, Next: "10009"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(states(v), c.states) || len(v.Stale) > 0 || v.Ended {
				t.Fatalf("view %+v", v)
			}
			for i, o := range c.outcomes {
				if f := v.Parts[i].Failure; o.State() == tracker.StateUnknown &&
					(f == nil || !strings.HasSuffix(f.Message, "may or may not have been made; look at the issue's place on the board before you try again")) {
					t.Errorf("an unknown part's failure %+v", f)
				}
			}
			if got := reads(); !slices.Equal(got, c.reads) {
				t.Fatalf("rows read %v, want %v", got, c.reads)
			}
			want := []string{"Rank"}
			for range c.reads {
				want = append(want, "BoardRows")
			}
			if got := f.callsSince(before); !slices.Equal(got, want) {
				t.Fatalf("calls %v, want %v", got, want)
			}
			for i, o := range c.outcomes {
				id := []string{"10001", "10002", "10003"}[i]
				if want := map[bool]string{true: "z" + id, false: string(rune('a' + i))}[o.State() == tracker.StateApplied]; f.orderOf(t, testBoard, id) != want {
					t.Errorf("%s's order is %q, want %q", id, f.orderOf(t, testBoard, id), want)
				}
			}
			for _, p := range v.Parts {
				switch p.State {
				case "refused":
					if r := p.Refusal; r == nil || r.Reason != "stale" || r.Message != "the neighbors moved" {
						t.Errorf("refusal %+v", r)
					}
				case "unknown":
					if p.Failure == nil || p.Failure.Kind != string(FailureTransient) {
						t.Errorf("unknown part's failure %+v", p.Failure)
					}
				}
			}
			if !slices.Contains(f.p.interactiveCalls(), true) || slices.Contains(f.p.interactiveCalls()[before:], false) {
				t.Errorf("a write's calls were not all interactive: %v", f.p.interactiveCalls()[before:])
			}
		})
	}
}

// A create whose answer was lost is reported as that, names no issue and
// is sent once; the board it was made from reads its changed rows at
// once, which brings the issue in.
func TestCreateOfUnknownOutcome(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.answer(tracker.Result{Parts: []tracker.Part{{Step: tracker.StepCreate, Outcome: tracker.Unknown(&trackerapi.TransientError{Err: errors.New("reset")})}}})
	f.state.set(testBoard, writeRow("10004", 1, "d"))
	before := len(f.p.callsInOrder())
	v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Type: "10001", Title: "New task"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.CreateUnknown || v.Created != "" || !slices.Equal(states(v), []string{" create unknown"}) || len(v.Stale) > 0 {
		t.Fatalf("view %+v", v)
	}
	if f := v.Parts[0].Failure; f == nil || f.Kind != string(FailureTransient) || !strings.HasSuffix(f.Message, "may or may not have been created; search the tracker for the issue before you try again") {
		t.Fatalf("the lost create's failure %+v", f)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Create", "ChangedRows"}) {
		t.Fatalf("calls %v", got)
	}
	if f.orderOf(t, testBoard, "10004") != "d" {
		t.Fatal("the board's delta did not bring the new issue in")
	}
}

// With the board's pump live, the tick a create of unknown outcome runs is
// the pump's: a failure it meets is told to the board's subscribers with
// the pump's sync state, and reported beside the outcome.
func TestCreateOfUnknownOutcomeTicksThroughThePump(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.subscribe(t, testBoard)
	f.events.boardEvents(t, 1)
	f.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	f.answer(tracker.Result{Parts: []tracker.Part{{Step: tracker.StepCreate, Outcome: tracker.Unknown(errors.New("lost"))}}})
	before := len(f.p.callsInOrder())
	v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "New task"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Stale) != 1 || v.Stale[0].Resources[0] != (ResourceView{Kind: "delta", Board: "1"}) || v.Stale[0].Failure.Kind != string(FailureTransient) {
		t.Fatalf("view %+v", v)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Create", "ChangedRows"}) {
		t.Fatalf("calls %v", got)
	}
	f.events.waitFor(t, "the pump's failure", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
		last := b[len(b)-1]
		return last.Sync.Failure != nil && last.Sync.Failure.Kind == string(FailureTransient)
	})
}

// A create that made an issue and then failed a later step reports the
// issue as created, and the issue's row is read through the board the
// create was made from. Its card and detail, which the mirror neither
// keeps nor holds yet, are not read.
func TestCreateThenAFailedStep(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	id := tracker.IssueID("10004")
	f.answer(tracker.Result{Parts: []tracker.Part{
		appliedPart("10004", tracker.StepCreate, tracker.CardOf(id), tracker.DetailOf(id)),
		{Issue: id, Step: tracker.StepUpload, Outcome: tracker.Refused(tracker.Refusal{Reason: tracker.ReasonTracker, Message: "too large"}),
			Changed: []tracker.Resource{tracker.DetailOf(id)}},
	}})
	f.state.set(testBoard, writeRow("10004", 1, "d"))
	reads := f.state.recordRows(f.p)
	before := len(f.p.callsInOrder())
	v, err := f.svc.Create(t.Context(), f.conn.ID, CreateRequest{Board: "1", Project: "10000", Title: "New task"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Created != "10004" || v.CreateUnknown || !slices.Equal(states(v), []string{"10004 create applied", "10004 upload refused"}) {
		t.Fatalf("view %+v", v)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Create", "NewRows"}) || len(reads()) != 0 {
		t.Fatalf("calls %v", got)
	}
	if f.orderOf(t, testBoard, "10004") != "d" {
		t.Fatal("the new issue's row was not stored")
	}
	if v.CreatedKey != "WEB-4" {
		t.Fatalf("the created issue's key %q, want the key of the row stored", v.CreatedKey)
	}
}

// A result that fails its check is an error, never a partial success, and
// nothing is read again.
func TestAResultThatFailsItsCheckIsAnError(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.answer(tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: tracker.StepEdit, Outcome: tracker.Applied(),
		Changed: []tracker.Resource{tracker.CardOf("10001")}}}})
	before := len(f.p.callsInOrder())
	_, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
	if !errors.Is(err, ErrBadResult) {
		t.Fatalf("an edit whose result names no detail: %v", err)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Edit"}) {
		t.Fatalf("calls %v", got)
	}
}

// A write the tracker could not be reached for settles that as the
// connection's problem, and one the tracker refused on its merits shows
// the connection works.
func TestAWriteSettlesTheProblem(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	edit := func(res tracker.Result) {
		t.Helper()
		f.answer(res)
		if _, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	edit(tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: tracker.StepEdit, Outcome: tracker.NotSent(&trackerapi.TransientError{Err: errors.New("reset")})}}})
	if f.problem() != FailureTransient {
		t.Fatalf("after an edit that could not reach the tracker, problem %q", f.problem())
	}
	// A file that stopped arriving says nothing of the connection.
	f.answer(tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: tracker.StepUpload,
		Outcome: tracker.NotSent(&trackerapi.SourceError{Err: errors.New("upload part 0 ended 5 bytes short of its declared size")})}}})
	if _, err := f.svc.Attach(t.Context(), f.conn.ID, "10001", []tracker.File{{Name: "a.txt", Content: strings.NewReader("12345"), Size: 10}}); err != nil {
		t.Fatal(err)
	}
	if f.problem() != FailureTransient {
		t.Fatalf("after an attach whose file stopped arriving, problem %q", f.problem())
	}
	edit(tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: tracker.StepEdit, Outcome: tracker.Refused(tracker.Refusal{Reason: tracker.ReasonStale, Message: "changed"})}}})
	if f.problem() != "" {
		t.Fatalf("after an edit the tracker refused, problem %q", f.problem())
	}
}

// A write refused for its credential says what the connection's problem
// says: what to do about the key file.
func TestAWriteRefusedForItsCredentialSaysWhatToDo(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.answer(tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: tracker.StepEdit, Outcome: tracker.Refused(tracker.Refusal{
		Reason: tracker.ReasonTracker, Message: "the tracker refused the credential (HTTP 401)", Err: &trackerapi.AuthError{Status: http.StatusUnauthorized}})}}})
	v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := v.Parts[0].Refusal; r == nil || r.Message != "Jira refused the token in the key file; put a valid token in it. A token made with Create API token with scopes cannot reach the Jira site: make one with Create API token" {
		t.Fatalf("a credential refused: %+v", r)
	}
	// A Jira connection's time goes to its linked Tempo connection: Jira
	// answered who logs it, then Tempo refused its own token
	// (jiraprovider's TestATempoRefusalAfterJiraNamesTempo).
	f.answer(tracker.Result{Parts: []tracker.Part{{Issue: "10001", Step: tracker.StepWorklog, Outcome: tracker.Refused(tracker.Refusal{
		Reason: tracker.ReasonTracker, Message: "the tracker refused the credential (HTTP 401)", Err: &trackerapi.AuthError{Tracker: "tempo", Status: http.StatusUnauthorized}})}}})
	v, err = f.svc.AddWorklog(t.Context(), f.conn.ID, WorklogRequest{Issue: "10001", Day: "2026-11-02", SpentSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	const tempo = "Tempo refused the token in the key file of the linked Tempo connection; put a valid token in it, and check that the region is the one the site's Tempo data is in"
	if r := v.Parts[0].Refusal; r == nil || r.Message != tempo {
		t.Fatalf("a Tempo credential refused: %+v", r)
	}
}

// A read that fails after an applied write is reported beside the
// outcomes, each resource once, as stale, and settled as the connection's
// problem; the write stands, and the other resources are still read
// again.
func TestARefreshThatFailsAfterAnAppliedWrite(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.openDetail(t, "10001")
	f.p.set(func(p *scripted) {
		p.boardRows = func(tracker.BoardID, []tracker.IssueID) (tracker.Rows, error) {
			return tracker.Rows{}, &trackerapi.TransientError{Err: errors.New("reset")}
		}
	})
	f.answer(tracker.Result{Parts: []tracker.Part{
		appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001")),
	}})
	before := len(f.p.callsInOrder())
	v, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(states(v), []string{"10001 edit applied"}) || len(v.Stale) != 1 {
		t.Fatalf("view %+v", v)
	}
	if s := v.Stale[0]; len(s.Resources) != 1 || s.Resources[0] != (ResourceView{Kind: "card", Issue: "10001"}) ||
		s.Failure.Kind != string(FailureTransient) {
		t.Errorf("stale entry %+v", s)
	}
	if got := f.callsSince(before); !slices.Equal(got, []string{"Edit", "BoardRows", "BoardRows", "Details"}) {
		t.Fatalf("calls %v", got)
	}
	if f.problem() != FailureTransient {
		t.Fatalf("problem %q", f.problem())
	}
}
