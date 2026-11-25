package trackerapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"sync"
	"testing"
	"time"
)

// The poker read's owner and what admits it: a read in flight never holds
// the ticket's own work, and only a handle that both follows and shows the
// ticket makes poker reads.

// pokerHold holds the add-on reads (GET invokes) the proxy sees while it
// is armed: each arrives on arrived, then waits for release or for its
// request's end, which it tells on ended.
type pokerHold struct {
	mu      sync.Mutex
	armed   bool
	arrived chan struct{}
	ended   chan struct{}
	release chan struct{}
}

func (s *fakeScenario) holdPokerReads(t *testing.T) *pokerHold {
	t.Helper()
	h := &pokerHold{armed: true, arrived: make(chan struct{}, 16), ended: make(chan struct{}, 16), release: make(chan struct{})}
	s.proxied(t, func(rp *httputil.ReverseProxy) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			h.mu.Lock()
			hold := h.armed && bytes.Contains(body, []byte(`"operationName":"Invoke"`)) && bytes.Contains(body, []byte(`"method":"GET"`))
			h.mu.Unlock()
			if hold {
				h.arrived <- struct{}{}
				select {
				case <-h.release:
				case <-r.Context().Done():
					h.ended <- struct{}{}
					return
				}
			}
			rp.ServeHTTP(w, r)
		})
	})
	return h
}

// open lets every held read and later read through.
func (h *pokerHold) open() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.armed {
		h.armed = false
		close(h.release)
	}
}

func receive(t *testing.T, what string, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(20 * time.Second):
		t.Fatalf("waited for %s", what)
	}
}

// F1: a poker read held at the add-on leaves the ticket's own refresh
// free: the refresh runs and returns while the read waits.
func TestPokerHeldReadLeavesTheTicketFree(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour}, pokerSite)
	hold := s.holdPokerReads(t)
	t.Cleanup(hold.open)
	sub := s.openTicket(t, conn, "10101")
	if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
		t.Fatal(err)
	}
	receive(t, "the held poker read", hold.arrived)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.svc.RefreshSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("the ticket's refresh while a poker read was held: %v", err)
	}
	hold.open()
	waitPoker(t, s.svc, sub.ID, "the released read", func(v PokerView) bool { return v.Loaded })
}

// F1: hiding the ticket, unfollowing it and releasing its handle each end
// the poker read in flight, and its late answer is not applied.
func TestPokerHeldReadEndsWithItsAdmission(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(s *Service, sub string) error{
		"hide":     func(s *Service, sub string) error { return s.SetSubscriptionVisible(sub, false) },
		"unfollow": func(s *Service, sub string) error { _, err := s.FollowPoker(sub, false); return err },
		"release":  func(s *Service, sub string) error { s.ReleaseSubscription(sub); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour}, pokerSite)
			hold := s.holdPokerReads(t)
			t.Cleanup(hold.open)
			sub := s.openTicket(t, conn, "10101")
			if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
				t.Fatal(err)
			}
			receive(t, "the held poker read", hold.arrived)
			if err := end(s.svc, sub.ID); err != nil {
				t.Fatal(err)
			}
			receive(t, "the held read's end", hold.ended)
			if name == "hide" {
				if v := must[PokerView](t)(s.svc.Poker(sub.ID)); v.Loaded {
					t.Fatalf("the ended read was applied: %+v", v)
				}
			}
		})
	}
}

// F2: a handle that follows but is hidden makes no read because another
// handle shows the ticket; reads follow the handles that both follow and
// show it, through show, hide, unfollow and release.
func TestPokerReadsOnlyForAVisibleFollower(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: 20 * time.Millisecond}, pokerSite)
	a := s.openTicket(t, conn, "10101")
	b := s.openTicket(t, conn, "10101")
	if err := s.svc.SetSubscriptionVisible(a.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.FollowPoker(a.ID, true); err != nil {
		t.Fatal(err)
	}
	if n := settledCalls(t, s, 300*time.Millisecond); n != 0 {
		t.Fatalf("%d poker reads for a follower nobody sees", n)
	}

	if err := s.svc.SetSubscriptionVisible(a.ID, true); err != nil {
		t.Fatal(err)
	}
	waitCount(t, "reads once the follower shows", func() int { return s.gatewayCalls("Invoke") }, 2)
	if err := s.svc.SetSubscriptionVisible(a.ID, false); err != nil {
		t.Fatal(err)
	}
	settledCalls(t, s, 300*time.Millisecond)

	if err := s.svc.SetSubscriptionVisible(a.ID, true); err != nil {
		t.Fatal(err)
	}
	n := s.gatewayCalls("Invoke")
	waitCount(t, "reads once the follower shows again", func() int { return s.gatewayCalls("Invoke") }, n+2)
	if _, err := s.svc.FollowPoker(a.ID, false); err != nil {
		t.Fatal(err)
	}
	settledCalls(t, s, 300*time.Millisecond)

	if _, err := s.svc.FollowPoker(a.ID, true); err != nil {
		t.Fatal(err)
	}
	s.svc.ReleaseSubscription(b.ID)
	n = s.gatewayCalls("Invoke")
	waitCount(t, "reads after the other handle's release", func() int { return s.gatewayCalls("Invoke") }, n+2)
	s.closeTicket(t, conn, a, "10101")
	settledCalls(t, s, 300*time.Millisecond)
}

// F3: a vote request with no value, or a null one, is refused before
// anything is sent, and the vote already cast stays; a vote of zero is a
// vote.
func TestPokerVoteNeedsItsValue(t *testing.T) {
	t.Parallel()
	s, conn := pokerScenario(t, Cadence{}, pokerSite)
	request := func(raw string) PokerVoteRequest {
		var in PokerVoteRequest
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	v, err := s.svc.PokerVote(t.Context(), conn.ID, request(`{"issue":"10101","vote":8}`))
	appliedView(t, "vote 8", v, err)
	for _, raw := range []string{`{"issue":"10101"}`, `{"issue":"10101","vote":null}`} {
		before := len(s.e.Requests(0).Requests)
		if v, err := s.svc.PokerVote(t.Context(), conn.ID, request(raw)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s answered %+v, %v", raw, v, err)
		}
		if n := len(s.e.Requests(0).Requests) - before; n != 0 {
			t.Fatalf("%s sent %d requests", raw, n)
		}
	}
	if got := fakeVote(t, s, "acc-user1"); got != 8 {
		t.Fatalf("the vote is %v after the requests with no value", got)
	}
	v, err = s.svc.PokerVote(t.Context(), conn.ID, request(`{"issue":"10101","vote":0}`))
	appliedView(t, "vote 0", v, err)
	if got := fakeVote(t, s, "acc-user1"); got != 0 {
		t.Fatalf("a vote of zero left %v", got)
	}
}

// fakeVote is account's vote on 10101 as the fake holds it.
func fakeVote(t *testing.T, s *fakeScenario, account string) float64 {
	t.Helper()
	for _, r := range s.e.Snapshot().Jira[0].Poker.Rounds {
		for _, v := range r.Votes {
			if r.Issue == "10101" && v.Account == account {
				return v.Vote
			}
		}
	}
	t.Fatalf("no vote of %s", account)
	return 0
}

// Stop and a connection turned off return with no poker run left, a run
// held in its request included.
func TestPokerRunEndsWithTheService(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(t *testing.T, s *fakeScenario, conn Connection){
		"turn off": func(t *testing.T, s *fakeScenario, conn Connection) {
			if _, err := s.svc.SetEnabled(conn.ID, false); err != nil {
				t.Fatal(err)
			}
		},
		"Stop": func(_ *testing.T, s *fakeScenario, _ Connection) { s.svc.Stop() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, conn := pokerScenario(t, Cadence{Delta: time.Hour, Ticket: time.Hour, Poker: time.Hour}, pokerSite)
			hold := s.holdPokerReads(t)
			t.Cleanup(hold.open)
			sub := s.openTicket(t, conn, "10101")
			tp := s.svc.ticketPumpOf(conn.ID, "10101")
			if _, err := s.svc.FollowPoker(sub.ID, true); err != nil {
				t.Fatal(err)
			}
			receive(t, "the held poker read", hold.arrived)
			end(t, s, conn)
			s.svc.subMu.Lock()
			runs := len(tp.pokerRuns)
			s.svc.subMu.Unlock()
			if runs != 0 {
				t.Fatalf("%d poker runs left after %s returned", runs, name)
			}
			select {
			case <-hold.ended:
			default:
				t.Fatalf("%s returned with the poker read still in its request", name)
			}
		})
	}
}
