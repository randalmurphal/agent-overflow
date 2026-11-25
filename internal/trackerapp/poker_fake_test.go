package trackerapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/secret"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/trackerapi"
)

// Planning poker through the real Jira client and provider against the
// fake site, which holds the add-on's votes.

const (
	pokerSiteName    = "example.atlassian.net"
	pokerMemberToken = "fake-jira-token-2"
	pokerContextPath = "/rest/internal/2/forge/context/token"
)

// pokerSite is the fake Jira site with the add-on, and User 2 as a member
// who votes from another app.
func pokerSite(at time.Time) trackerfake.JiraSite {
	site := jiraSeed(at)
	site.Members = []trackerfake.JiraMember{{Account: "acc-user2", Token: pokerMemberToken}}
	site.Poker = &trackerfake.JiraPoker{}
	return site
}

// pokerScenario is a service with one Jira connection to site.
func pokerScenario(t *testing.T, cad Cadence, site func(time.Time) trackerfake.JiraSite) (*fakeScenario, Connection) {
	t.Helper()
	s := newFakeScenarioWith(t, cad, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site(at)}}
	}, pokerSiteName)
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	return s, conn
}

// memberPoker is User 2's poker on the site, as a teammate in another app
// votes.
func (s *fakeScenario) memberPoker(t *testing.T) tracker.Poker {
	t.Helper()
	base, err := s.origin(store.TrackerKindJira, pokerSiteName)
	if err != nil {
		t.Fatal(err)
	}
	jc, err := jira.New(jira.Options{BaseURL: base, Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user2@example.com", secret.New([]byte(pokerMemberToken))))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jc.Close)
	p, err := jiraprovider.New(jiraprovider.Options{Jira: jc, Site: "https://" + pokerSiteName, SprintsFrom: func() time.Time { return s.clock.Now().Add(-time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	return p.Poker()
}

// gatewayCalls counts the requests to Atlassian's gateway of operation op
// (Ext, the listing; Invoke, the add-on's calls) and to the context token.
func (s *fakeScenario) gatewayCalls(op string) int {
	n := 0
	for _, r := range s.e.Requests(0).Requests {
		switch {
		case op == "token" && r.Route == "POST rest/internal/2/forge/context/token":
			n++
		case r.Route == "POST gateway/api/graphql" && bytes.Contains(r.Body, []byte(`"operationName":"`+op+`"`)):
			n++
		}
	}
	return n
}

// waitPoker polls the votes handle sub was last told until ok accepts
// them. An event carries every change, and the view is read under the
// subscriptions' lock, which the event log's must not wait on.
func waitPoker(t *testing.T, s *Service, sub, what string, ok func(PokerView) bool) PokerView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		v, err := s.Poker(sub)
		if err != nil {
			t.Fatal(err)
		}
		if ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s; the votes %+v", what, v)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitCount(t *testing.T, what string, count func() int, atLeast int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for count() < atLeast {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d %s; %d made", atLeast, what, count())
		}
		time.Sleep(time.Millisecond)
	}
}

// voteOf is account's vote in v, and whether there is one.
func voteOf(v PokerView, account string) (PokerVoteView, bool) {
	for _, vote := range v.Votes {
		if vote.Voter.ID == account {
			return vote, true
		}
	}
	return PokerVoteView{}, false
}

func pokerEvents(s *fakeScenario, issue tracker.IssueID) int {
	_, issues, _ := s.events.snapshot()
	n := 0
	for _, e := range issues {
		if e.Poker && e.Issue == issue {
			n++
		}
	}
	return n
}

func appliedView(t *testing.T, what string, v WriteView, err error) {
	t.Helper()
	if err != nil || len(v.Parts) != 1 || v.Parts[0].State != "applied" || v.Parts[0].Step != "poker" {
		t.Fatalf("%s: %+v, %v", what, v, err)
	}
}

// Opening a ticket asks nothing of poker; following it reads the votes
// at once and every Cadence.Poker after, telling a teammate's vote and
// reveal with an event, and a read that changed nothing with none.
// Another voter's value stays hidden until the reveal.
func TestPokerFollowTellsOtherVoters(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: 20 * time.Millisecond}, pokerSite)
	sub := s.openTicket(t, conn, "10101")
	quiet(t, s.svc)
	if n := s.gatewayCalls("Ext") + s.gatewayCalls("Invoke") + s.gatewayCalls("token"); n != 0 {
		t.Fatalf("opening the ticket made %d poker calls", n)
	}
	if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	waitPoker(t, s.svc, sub.ID, "the first read", func(v PokerView) bool { return v.Loaded && len(v.Votes) == 0 && v.Failure == nil })

	theirs := s.memberPoker(t)
	if r := theirs.Vote(t.Context(), tracker.PokerVoteInput{Issue: "10101", Value: 8}); !r.AllApplied() {
		t.Fatalf("their vote: %+v", r)
	}
	v := waitPoker(t, s.svc, sub.ID, "their vote", func(v PokerView) bool { _, ok := voteOf(v, "acc-user2"); return ok })
	if vote, _ := voteOf(v, "acc-user2"); vote.Value != nil || vote.Mine || vote.Voter.Name != "User 2" || v.Revealed {
		t.Fatalf("their vote before the reveal: %+v in %+v", vote, v)
	}

	told, invokes := pokerEvents(s, "10101"), s.gatewayCalls("Invoke")
	waitCount(t, "reads", func() int { return s.gatewayCalls("Invoke") }, invokes+3)
	if got := pokerEvents(s, "10101"); got != told {
		t.Fatalf("%d poker events from reads that changed nothing", got-told)
	}

	if r := theirs.Reveal(t.Context(), "10101"); !r.AllApplied() {
		t.Fatalf("their reveal: %+v", r)
	}
	v = waitPoker(t, s.svc, sub.ID, "the reveal", func(v PokerView) bool { return v.Revealed })
	if vote, _ := voteOf(v, "acc-user2"); vote.Value == nil || *vote.Value != 8 {
		t.Fatalf("their vote after the reveal: %+v", vote)
	}
	if pokerEvents(s, "10101") <= told {
		t.Fatal("no poker event told the reveal")
	}
}

// settledCalls waits for a read in flight to end, and returns the invokes
// made, which no healthy run moves past for wait.
func settledCalls(t *testing.T, s *fakeScenario, wait time.Duration) int {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	n := s.gatewayCalls("Invoke")
	time.Sleep(wait)
	if got := s.gatewayCalls("Invoke"); got != n {
		t.Fatalf("%d poker reads while nothing should read", got-n)
	}
	return n
}

// A followed ticket that no handle shows, one no handle follows, and a
// closed one make no poker call; showing it again reads at once. The
// last follower's end drops the votes held.
func TestPokerCostsNothingHiddenOrUnfollowed(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: 20 * time.Millisecond}, pokerSite)
	sub := s.openTicket(t, conn, "10101")
	if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	waitPoker(t, s.svc, sub.ID, "the first read", func(v PokerView) bool { return v.Loaded })

	if err := s.svc.SetSubscriptionVisible(sub.ID, false); err != nil {
		t.Fatal(err)
	}
	n := settledCalls(t, s, 300*time.Millisecond)
	if err := s.svc.SetSubscriptionVisible(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	waitCount(t, "reads once shown", func() int { return s.gatewayCalls("Invoke") }, n+1)

	if v, err := s.svc.FollowPoker(sub.ID, false); err != nil || v.Loaded {
		t.Fatalf("unfollow answered %+v, %v", v, err)
	}
	if v := must[PokerView](t)(s.svc.Poker(sub.ID)); v.Loaded || len(v.Votes) > 0 {
		t.Fatalf("the votes after the last follower left: %+v", v)
	}
	settledCalls(t, s, 300*time.Millisecond)

	if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	waitPoker(t, s.svc, sub.ID, "the read of a new follow", func(v PokerView) bool { return v.Loaded })
	s.closeTicket(t, conn, sub, "10101")
	settledCalls(t, s, 300*time.Millisecond)
}

// Two handles follow one ticket: the votes are read once per interval for
// both, and stay followed until the second unfollows.
func TestPokerTwoFollowersShareOneRead(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour}, pokerSite)
	a := s.openTicket(t, conn, "10101")
	b := s.openTicket(t, conn, "10101")
	for _, sub := range []TicketSubscription{a, b} {
		if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	waitPoker(t, s.svc, b.ID, "the first read", func(v PokerView) bool { return v.Loaded })
	settledCalls(t, s, 100*time.Millisecond)
	if n := s.gatewayCalls("Invoke"); n != 1 {
		t.Fatalf("%d reads for two followers' first look; want 1", n)
	}
	s.svc.ReleaseSubscription(a.ID)
	if v := must[PokerView](t)(s.svc.Poker(b.ID)); !v.Loaded {
		t.Fatalf("one follower's release dropped the other's votes: %+v", v)
	}
}

type heldInvoke struct{}

// The follow's first read is answered before the vote lands and arrives
// after it: the read the vote asked for while that one was in flight
// still runs, and shows the vote, well before the next interval.
func TestPokerWriteDuringAReadIsReadAgain(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour}, pokerSite)
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.proxied(t, func(rp *httputil.ReverseProxy) http.Handler {
		rp.ModifyResponse = func(r *http.Response) error {
			if r.Request.Context().Value(heldInvoke{}) != nil {
				close(arrived)
				<-release
			}
			return nil
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"operationName":"Invoke"`)) && bytes.Contains(body, []byte(`"method":"GET"`)) {
				once.Do(func() { r = r.WithContext(context.WithValue(r.Context(), heldInvoke{}, true)) })
			}
			rp.ServeHTTP(w, r)
		})
	})
	sub := s.openTicket(t, conn, "10101")
	if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, "the first read's answer", arrived)
	v, err := s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: "10101", Vote: 3})
	appliedView(t, "vote", v, err)
	close(release)
	waitPoker(t, s.svc, sub.ID, "my vote", func(v PokerView) bool { return len(v.Votes) == 1 })
}

// A vote, a reveal and a clear answer as every write does, and the
// following ticket reads the votes at once after each, well before the
// next interval. No vote writes the issue's estimate.
func TestPokerWritesReadAtOnce(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour}, pokerSite)
	sub := s.openTicket(t, conn, "10101")
	if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	waitPoker(t, s.svc, sub.ID, "the first read", func(v PokerView) bool { return v.Loaded })

	v, err := s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: "10101", Vote: 3})
	appliedView(t, "vote", v, err)
	got := waitPoker(t, s.svc, sub.ID, "my vote", func(v PokerView) bool { _, ok := voteOf(v, "acc-user1"); return ok })
	if mine, _ := voteOf(got, "acc-user1"); !mine.Mine || mine.Value == nil || *mine.Value != 3 || mine.Voter.Name != "User 1" {
		t.Fatalf("my vote: %+v", mine)
	}
	v, err = s.svc.RevealPoker(t.Context(), conn.ID, "10101")
	appliedView(t, "reveal", v, err)
	waitPoker(t, s.svc, sub.ID, "the reveal", func(v PokerView) bool { return v.Revealed })
	v, err = s.svc.ClearPoker(t.Context(), conn.ID, "10101")
	appliedView(t, "clear", v, err)
	waitPoker(t, s.svc, sub.ID, "the clear", func(v PokerView) bool { return !v.Revealed && len(v.Votes) == 0 })

	for _, is := range s.e.Snapshot().Jira[0].Issues {
		if is.ID == "10101" && string(is.Values["customfield_10016"]) != "5" {
			t.Fatalf("poker changed the estimate to %s", is.Values["customfield_10016"])
		}
	}
}

// Whether a connection's site has the add-on is read from one listing,
// kept by the client, with no invoke and no token; a site without it, a
// Linear and a Tempo connection answer false with no failure, the last
// two with no request.
func TestPokerSupportByConnection(t *testing.T) {
	t.Parallel()
	t.Run("with the add-on", func(t *testing.T) {
		t.Parallel()
		s, conn := pokerScenario(t, Cadence{}, pokerSite)
		for range 2 {
			if got, err := s.svc.PokerSupport(t.Context(), conn.ID); err != nil || got != (PokerSupport{Available: true}) {
				t.Fatalf("support %+v, %v", got, err)
			}
		}
		if ext, other := s.gatewayCalls("Ext"), s.gatewayCalls("Invoke")+s.gatewayCalls("token"); ext != 1 || other != 0 {
			t.Fatalf("%d listings and %d other poker calls for two asks", ext, other)
		}
	})
	t.Run("without it", func(t *testing.T) {
		t.Parallel()
		s, conn := pokerScenario(t, Cadence{}, jiraSeed)
		if got, err := s.svc.PokerSupport(t.Context(), conn.ID); err != nil || got != (PokerSupport{}) {
			t.Fatalf("support %+v, %v", got, err)
		}
		before := len(s.e.Requests(0).Requests)
		for _, other := range []Connection{s.addLinear(), s.addTempo(conn.ID)} {
			if got, err := s.svc.PokerSupport(t.Context(), other.ID); err != nil || got != (PokerSupport{}) {
				t.Fatalf("%s support %+v, %v", other.Kind, got, err)
			}
		}
		if n := len(s.e.Requests(0).Requests) - before; n != 0 {
			t.Fatalf("Linear and Tempo support made %d requests", n)
		}
	})
	t.Run("a failed listing", func(t *testing.T) {
		t.Parallel()
		s, conn := pokerScenario(t, Cadence{}, pokerSite)
		if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: pokerSiteName},
			Route: "POST gateway/api/graphql", Kind: trackerfake.FaultRefuse, Status: 400, Times: 1,
			Body: []byte(`{"errors":[{"message":"Cannot query field"}]}`)}); err != nil {
			t.Fatal(err)
		}
		got, err := s.svc.PokerSupport(t.Context(), conn.ID)
		if err != nil || got.Available || got.Failure == nil || !strings.HasPrefix(got.Failure.Message, "planning poker is unavailable on this site right now") {
			t.Fatalf("support %+v, %v", got, err)
		}
		if c := must[[]Connection](t)(s.svc.Connections()); c[0].Problem != nil {
			t.Fatalf("poker's failure became the connection's: %+v", c[0].Problem)
		}
	})
}

// On a site without the add-on a poker write is refused with a sentence
// before the add-on is called; a vote out of range is refused before
// anything is sent; the gateway's 4xx refuses a write and its 5xx leaves
// it unknown, saying to look before trying again.
func TestPokerWriteOutcomes(t *testing.T) {
	t.Parallel()
	t.Run("without the add-on", func(t *testing.T) {
		t.Parallel()
		s, conn := pokerScenario(t, Cadence{}, jiraSeed)
		v, err := s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: "10101", Vote: 3})
		if err != nil || v.Parts[0].State != "refused" || v.Parts[0].Refusal.Message != jiraprovider.PokerAbsent {
			t.Fatalf("vote %+v, %v", v, err)
		}
		if n := s.gatewayCalls("Invoke") + s.gatewayCalls("token"); n != 0 {
			t.Fatalf("%d calls toward an add-on the site lacks", n)
		}
	})
	t.Run("with it", func(t *testing.T) {
		t.Parallel()
		s, conn := pokerScenario(t, Cadence{}, pokerSite)
		v, err := s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: "10101", Vote: 1001})
		if err != nil || v.Parts[0].State != "refused" || v.Parts[0].Refusal.Reason != "invalid" || len(s.e.Requests(0).Requests) != 0 {
			t.Fatalf("a vote of 1001: %+v, %v", v, err)
		}
		v, err = s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: "10101", Vote: 3})
		appliedView(t, "vote", v, err)
		for status, want := range map[int]string{400: "refused", 503: "unknown"} {
			if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: pokerSiteName},
				Route: "POST gateway/api/graphql", Kind: trackerfake.FaultRefuse, Status: status, Times: 1,
				Body: []byte(`{"errors":[{"message":"no"}]}`)}); err != nil {
				t.Fatal(err)
			}
			v, err := s.svc.RevealPoker(t.Context(), conn.ID, "10101")
			if err != nil || v.Parts[0].State != want {
				t.Fatalf("HTTP %d: %+v, %v", status, v, err)
			}
			if want == "unknown" && !strings.HasSuffix(v.Parts[0].Failure.Message, LostWriteCheck(tracker.StepPoker)) {
				t.Fatalf("HTTP %d says %q", status, v.Parts[0].Failure.Message)
			}
		}
	})
}

// A Linear ticket refuses a follow, its poker writes are refused, and it
// has no support.
func TestPokerOnLinear(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
	})
	conn := s.addLinear()
	s.writeToken(conn, "lin_api_fake00001")
	sub := s.openTicket(t, conn, tracker.IssueID(lid(60)))
	if _, err := s.svc.FollowPoker(sub.ID, true); !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("follow: %v", err)
	}
	v, err := s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: lid(60), Vote: 3})
	if err != nil || v.Parts[0].State != "refused" || v.Parts[0].Refusal.Reason != "unsupported" {
		t.Fatalf("vote %+v, %v", v, err)
	}
}

// A poker read that fails keeps the votes last read and tells its
// failure with them, never as the ticket's or the connection's; the next
// read on the retry schedule clears it.
func TestPokerReadFailureIsPokersOwn(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour, RetryBase: 50 * time.Millisecond}, pokerSite)
	sub := s.openTicket(t, conn, "10101")
	if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	v, err := s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: "10101", Vote: 3})
	appliedView(t, "vote", v, err)
	waitPoker(t, s.svc, sub.ID, "my vote", func(v PokerView) bool { return len(v.Votes) == 1 })

	if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: pokerSiteName},
		Route: "POST gateway/api/graphql", Kind: trackerfake.FaultRefuse, Status: 400, Times: 1,
		Body: []byte(`{"errors":[{"message":"Cannot query field"}]}`)}); err != nil {
		t.Fatal(err)
	}
	s.svc.readPokerNow(conn.ID, "10101")
	failed := waitPoker(t, s.svc, sub.ID, "the failure", func(v PokerView) bool { return v.Failure != nil })
	if len(failed.Votes) != 1 || !strings.HasPrefix(failed.Failure.Message, "planning poker is unavailable on this site right now: ") ||
		!strings.HasSuffix(failed.Failure.Message, "; the rest of the ticket is unaffected") {
		t.Fatalf("the failed read: %+v", failed)
	}
	waitPoker(t, s.svc, sub.ID, "the retry", func(v PokerView) bool { return v.Failure == nil })
	_, issues, _ := s.events.snapshot()
	for _, e := range issues {
		if e.Failure != nil {
			t.Fatalf("the ticket was told a failure: %+v", e)
		}
	}
	if c := must[[]Connection](t)(s.svc.Connections()); c[0].Problem != nil {
		t.Fatalf("poker's failure became the connection's: %+v", c[0].Problem)
	}
}

// The add-on's context token stays in the client's memory: no event,
// answer, log line, file under the test's directories, or request the
// fake logged carries it, including a refusal whose message repeats it.
func TestPokerTokenStaysInMemory(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		tokens []string
		lines  []string
	)
	s := newFakeScenarioWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour}, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{pokerSite(at)}}
	}, pokerSiteName)
	s.svc.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	s.proxied(t, func(rp *httputil.ReverseProxy) http.Handler {
		rp.ModifyResponse = func(r *http.Response) error {
			if r.Request.URL.Path != pokerContextPath {
				return nil
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return err
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var answer struct{ Token string }
			if err := json.Unmarshal(body, &answer); err != nil {
				return err
			}
			mu.Lock()
			tokens = append(tokens, answer.Token)
			mu.Unlock()
			return nil
		}
		return rp
	})
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")

	var answers []any
	sub := s.openTicket(t, conn, "10101")
	answers = append(answers, sub, must[PokerSupport](t)(s.svc.PokerSupport(t.Context(), conn.ID)), must[PokerView](t)(s.svc.FollowPoker(sub.ID, true)))
	v, err := s.svc.PokerVote(t.Context(), conn.ID, PokerVoteRequest{Issue: "10101", Vote: 3})
	appliedView(t, "vote", v, err)
	answers = append(answers, v, waitPoker(t, s.svc, sub.ID, "my vote", func(v PokerView) bool { return len(v.Votes) == 1 }))

	mu.Lock()
	token := ""
	if len(tokens) > 0 {
		token = tokens[0]
	}
	mu.Unlock()
	if len(token) < 20 {
		t.Fatalf("the proxy saw no context token: %q", token)
	}
	echo := func() {
		t.Helper()
		if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: pokerSiteName},
			Route: "POST gateway/api/graphql", Kind: trackerfake.FaultRefuse, Status: 400, Times: 1,
			Body: []byte(`{"errors":[{"message":"token ` + token + ` refused"}]}`)}); err != nil {
			t.Fatal(err)
		}
	}
	echo()
	s.svc.readPokerNow(conn.ID, "10101")
	answers = append(answers, waitPoker(t, s.svc, sub.ID, "the refused read", func(v PokerView) bool { return v.Failure != nil }))
	echo()
	v, err = s.svc.RevealPoker(t.Context(), conn.ID, "10101")
	if err != nil || v.Parts[0].State != "refused" {
		t.Fatalf("the refused reveal: %+v, %v", v, err)
	}
	answers = append(answers, v, must[PokerView](t)(s.svc.Poker(sub.ID)))
	s.closeTicket(t, conn, sub, "10101")

	_, issues, loopErrs := s.events.snapshot()
	mu.Lock()
	logged := strings.Join(lines, "\n")
	mu.Unlock()
	for what, data := range map[string]any{
		"an answer": answers, "an event": issues, "a loop error": fmt.Sprint(loopErrs),
		"a log line": logged, "the fake's log": s.e.Requests(0), "an unhandled request": s.e.Unhandled(),
	} {
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("%s carries the context token: %s", what, b)
		}
	}
	// Every directory the test made: the store's and the key files'.
	root := filepath.Dir(filepath.Dir(s.creds))
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("%s holds the context token", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
