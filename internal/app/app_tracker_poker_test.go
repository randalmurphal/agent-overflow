package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

func TestTrackerPokerMethodsCarryTheirScopeAndRoute(t *testing.T) {
	t.Parallel()
	read, operate := transport.ScopeTrackerRead, transport.ScopeTrackerOperate
	for name, w := range map[string]struct {
		scope  transport.Scope
		budget time.Duration
	}{
		"TrackerPokerSupport":    {read, 0},
		"FollowTrackerPoker":     {read, 0},
		"TrackerPoker":           {read, 0},
		"VoteTrackerPoker":       {operate, trackerWriteBudget},
		"RevealTrackerPoker":     {operate, trackerWriteBudget},
		"ClearTrackerPokerVotes": {operate, trackerWriteBudget},
	} {
		i := slices.IndexFunc(transport.GeneratedMethods, func(m transport.MethodMeta) bool { return m.Name == name })
		if i < 0 {
			t.Errorf("%s is not a generated method", name)
			continue
		}
		m := transport.GeneratedMethods[i]
		if m.Scope != w.scope || m.Route != transport.RouteHome || m.CallerBound || m.Budget != w.budget {
			t.Errorf("%s = %+v, want scope %s, route home, not caller-bound, budget %s", name, m, w.scope, w.budget)
		}
	}
}

// pokerShows waits until the followed ticket's votes, read as a client
// reads them after an event, satisfy ok.
func (f *trackerBoardFixture) pokerShows(t *testing.T, ctx context.Context, sub, what string, ok func(trackerapp.PokerView) bool) trackerapp.PokerView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := f.a.TrackerPoker(ctx, sub)
		if err != nil {
			t.Fatal(err)
		}
		if ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("the votes never showed %s: %+v", what, v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Planning poker is reached end to end from a client through the bound
// methods, the real provider and the fake site's add-on: whether the site
// has it, a follow whose first read arrives as an event with poker set,
// a vote, a reveal and a clear each shown with no refresh, and the
// refusals' sentences.
func TestTrackerPokerReachesItsClient(t *testing.T) {
	t.Parallel()
	site := trackerBoardSite()
	site.Poker = &trackerfake.JiraPoker{}
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: site, cadence: trackerapp.Cadence{Poker: time.Hour}})
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{})

	if got, err := f.a.TrackerPokerSupport(ctx, f.conn.ID); err != nil || !got.Available {
		t.Fatalf("TrackerPokerSupport = %+v, %v", got, err)
	}
	sub, err := f.a.SubscribeTrackerTicket(ctx, f.conn.ID, "10101")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.FollowTrackerPoker(ctx, sub.ID, true); err != nil {
		t.Fatal(err)
	}
	f.issueEvent(t, "for the votes' first read", func(e trackerapp.IssueEvent) bool { return e.Issue == "10101" && e.Poker })
	f.pokerShows(t, ctx, sub.ID, "no votes", func(v trackerapp.PokerView) bool { return v.Loaded && len(v.Votes) == 0 })

	v, err := f.a.VoteTrackerPoker(ctx, f.conn.ID, trackerapp.PokerVoteRequest{Issue: "10101", Vote: new(5.0)})
	if err != nil || !appliedOnly(v) {
		t.Fatalf("VoteTrackerPoker = %+v, %v", v, err)
	}
	f.pokerShows(t, ctx, sub.ID, "my vote", func(v trackerapp.PokerView) bool {
		return len(v.Votes) == 1 && v.Votes[0].Mine && v.Votes[0].Value != nil && *v.Votes[0].Value == 5
	})
	if v, err = f.a.RevealTrackerPoker(ctx, f.conn.ID, "10101"); err != nil || !appliedOnly(v) {
		t.Fatalf("RevealTrackerPoker = %+v, %v", v, err)
	}
	f.pokerShows(t, ctx, sub.ID, "the reveal", func(v trackerapp.PokerView) bool { return v.Revealed })
	if v, err = f.a.ClearTrackerPokerVotes(ctx, f.conn.ID, "10101"); err != nil || !appliedOnly(v) {
		t.Fatalf("ClearTrackerPokerVotes = %+v, %v", v, err)
	}
	f.pokerShows(t, ctx, sub.ID, "the clear", func(v trackerapp.PokerView) bool { return !v.Revealed && len(v.Votes) == 0 })

	board := f.subscribe(t, ctx)
	if _, err := f.a.FollowTrackerPoker(ctx, board.ID, true); !hasCode(err, "tracker_request_invalid") {
		t.Fatalf("a board's follow = %v", err)
	}
	if err := f.a.ReleaseTrackerSubscription(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.TrackerPoker(ctx, sub.ID); !hasCode(err, "tracker_subscription_unknown") {
		t.Fatalf("a released ticket's votes = %v", err)
	}
}
