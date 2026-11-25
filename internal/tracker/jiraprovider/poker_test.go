package jiraprovider

import (
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// memberPoker is User 2's poker on the scenario site, as a teammate in
// another app votes.
func (s *scenario) memberPoker(t *testing.T) tracker.Poker {
	t.Helper()
	jc, err := jira.New(jira.Options{BaseURL: s.url, Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user2@example.com", secret.New([]byte(scenarioMemberToken))))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jc.Close)
	p, err := New(Options{Jira: jc, Site: "https://" + scenarioSite, SprintsFrom: func() time.Time { return s.clock.Now().Add(-scenarioWindow) }})
	if err != nil {
		t.Fatal(err)
	}
	return p.Poker()
}

func pokerValues(r tracker.PokerRound) map[tracker.UserID]any {
	out := map[tracker.UserID]any{}
	for _, v := range r.Votes {
		if v.Value == nil {
			out[v.Voter] = nil
			continue
		}
		out[v.Voter] = *v.Value
	}
	return out
}

// TestPokerRoundThroughTheFake: two people vote; before the reveal each
// sees only their own value, after it both; clear empties the round. A
// vote writes no estimate.
func TestPokerRoundThroughTheFake(t *testing.T) {
	s := newScenario(t, false)
	mine, theirs := s.p.Poker(), s.memberPoker(t)
	if ok, err := mine.Support(s.ctx); err != nil || !ok {
		t.Fatalf("support %v %v", ok, err)
	}
	allApplied(t, "my vote", mine.Vote(s.ctx, tracker.PokerVoteInput{Issue: "10101", Value: 3}))
	allApplied(t, "their vote", theirs.Vote(s.ctx, tracker.PokerVoteInput{Issue: "10101", Value: 8}))

	r, err := mine.Read(s.ctx, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if got := pokerValues(r); r.Revealed || len(got) != 2 || got["acc-user1"] != 3.0 || got["acc-user2"] != nil {
		t.Fatalf("before the reveal I read %v", got)
	}
	for _, v := range r.Votes {
		if v.Mine != (v.Voter == "acc-user1") {
			t.Fatalf("vote %+v marks mine wrongly", v)
		}
	}
	names := map[tracker.UserID]string{}
	for _, u := range r.Users {
		names[u.ID] = u.Name
	}
	if names["acc-user1"] != "User 1" || names["acc-user2"] != "User 2" {
		t.Fatalf("voters named %v", names)
	}
	if got := mustRead(t, s, theirs); got["acc-user1"] != nil || got["acc-user2"] != 8.0 {
		t.Fatalf("before the reveal they read %v", got)
	}

	allApplied(t, "reveal", theirs.Reveal(s.ctx, "10101"))
	if got := mustRead(t, s, mine); got["acc-user1"] != 3.0 || got["acc-user2"] != 8.0 {
		t.Fatalf("after the reveal I read %v", got)
	}
	allApplied(t, "clear", mine.Clear(s.ctx, "10101"))
	if got := mustRead(t, s, mine); len(got) != 0 {
		t.Fatalf("after clear I read %v", got)
	}
	for _, is := range s.e.Snapshot().Jira[0].Issues {
		if is.ID == "10101" && string(is.Values["customfield_10016"]) != "5" {
			t.Fatalf("poker changed the estimate to %s", is.Values["customfield_10016"])
		}
	}
}

func mustRead(t *testing.T, s *scenario, p tracker.Poker) map[tracker.UserID]any {
	t.Helper()
	r, err := p.Read(s.ctx, "10101")
	if err != nil {
		t.Fatal(err)
	}
	return pokerValues(r)
}

// TestPokerWithoutTheAddOn: a site without it says so to Support, fails a
// read with tracker.ErrPokerAbsent and refuses a write as unsupported,
// sending no token request or invoke.
func TestPokerWithoutTheAddOn(t *testing.T) {
	s := newScenario(t, false)
	site := scenarioFixture(s.clock.Now())
	site.Poker, site.Tempo = nil, nil
	if err := s.e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
	poker := s.p.Poker()
	if ok, err := poker.Support(s.ctx); err != nil || ok {
		t.Fatalf("support %v %v", ok, err)
	}
	if _, err := poker.Read(s.ctx, "10101"); !errors.Is(err, tracker.ErrPokerAbsent) {
		t.Fatalf("read: %v", err)
	}
	for name, res := range map[string]tracker.Result{
		"vote":   poker.Vote(s.ctx, tracker.PokerVoteInput{Issue: "10101", Value: 3}),
		"reveal": poker.Reveal(s.ctx, "10101"),
		"clear":  poker.Clear(s.ctx, "10101"),
	} {
		if r, ok := res.Parts[0].Outcome.Refusal(); !ok || r.Reason != tracker.ReasonUnsupported || r.Message != PokerAbsent {
			t.Errorf("%s: %+v", name, res.Parts[0].Outcome)
		}
	}
	for _, r := range s.e.Requests(0).Requests {
		if r.Route == "POST rest/internal/2/forge/context/token" {
			t.Fatal("a token was asked for on a site without the add-on")
		}
	}
}

// TestPokerWriteOutcomes: a vote out of range is refused before anything
// is sent; the gateway's 4xx refuses a write and its 5xx leaves it
// unknown.
func TestPokerWriteOutcomes(t *testing.T) {
	s := newScenario(t, false)
	poker := s.p.Poker()
	res := poker.Vote(s.ctx, tracker.PokerVoteInput{Issue: "10101", Value: 1001})
	if r, ok := res.Parts[0].Outcome.Refusal(); !ok || r.Reason != tracker.ReasonInvalid || len(s.e.Requests(0).Requests) != 0 {
		t.Fatalf("a vote of 1001: %+v after %d requests", res.Parts[0].Outcome, len(s.e.Requests(0).Requests))
	}
	if _, err := poker.Read(s.ctx, "10101"); err != nil {
		t.Fatal(err)
	}
	for status, want := range map[int]tracker.State{400: tracker.StateRefused, 503: tracker.StateUnknown} {
		if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: scenarioSite},
			Route: "POST gateway/api/graphql", Kind: trackerfake.FaultRefuse, Status: status, Times: 1,
			Body: []byte(`{"errors":[{"message":"no"}]}`)}); err != nil {
			t.Fatal(err)
		}
		res := poker.Vote(s.ctx, tracker.PokerVoteInput{Issue: "10101", Value: 3})
		if err := res.Check(); err != nil || res.Parts[0].Step != tracker.StepPoker || res.Parts[0].Outcome.State() != want {
			t.Errorf("HTTP %d: %+v (%v)", status, res.Parts[0].Outcome, err)
		}
	}
}
