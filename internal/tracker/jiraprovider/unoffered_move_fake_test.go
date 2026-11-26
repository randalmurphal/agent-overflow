package jiraprovider

import (
	"slices"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A move the issue's workflow stopped offering after its moves were read
// is refused as stale, in one sentence, whether the provider reads the
// moves before sending (a person's move) or takes them from the read the
// caller kept (Detail.Read) and Jira refuses the send: the fake answers
// the measured 400 and its sentence. Nothing is applied.
func TestFakeAMoveNoLongerOfferedIsStale(t *testing.T) {
	s := newScenario(t, false)
	d := s.detail(t, "10102")
	i := slices.IndexFunc(d.Moves, func(m tracker.Move) bool { return m.To == "10002" })
	if i < 0 || d.Read == nil {
		t.Fatalf("moves %+v, read %v", d.Moves, d.Read)
	}
	move := d.Moves[i].ID

	// The workflow drops the move, which moves no update time.
	f := s.e.Snapshot()
	f.Jira[0].Token, f.Jira[0].Members[0].Token = "scenario-jira-1", scenarioMemberToken
	p := &f.Jira[0].Projects[0]
	p.Transitions = slices.DeleteFunc(p.Transitions, func(tr trackerfake.JiraTransition) bool { return tr.ID == string(move) })
	if err := s.e.Seed(f); err != nil {
		t.Fatal(err)
	}

	var said []string
	for _, c := range []struct {
		name string
		read tracker.IssueRead
	}{{"moves read before the send", nil}, {"moves the caller kept", d.Read}} {
		res := s.p.Move(s.ctx, tracker.MoveInput{Issue: "10102", Move: move, Read: c.read})
		r, ok := res.Parts[0].Outcome.Refusal()
		if !ok || r.Reason != tracker.ReasonStale {
			t.Fatalf("%s: %s %+v; want refused as stale", c.name, res.Parts[0].Outcome.State(), r)
		}
		said = append(said, r.Message)
	}
	if said[0] != said[1] {
		t.Errorf("two sentences for one refusal:\n%s\n%s", said[0], said[1])
	}
	if got := s.detail(t, "10102").Card.Status; got == "10002" {
		t.Errorf("a refused move moved the issue")
	}
	if n := countRoute(s.e, "POST rest/api/3/issue/{issue}/transitions"); n != 1 {
		t.Errorf("%d moves sent; want 1, the kept read's", n)
	}
}
