package trackerapp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The reads by ID of a reconcile against fake Jira, a proxy in front of
// it answering or watching the requests a test names. done is a completed
// issue whose row the first sync stores and that the window then passes,
// so a reconcile's listing leaves it out and reads it by ID under a rule
// naming the board's sprints.
const done = "10104"

// The requests of a reconcile, as the proxy tells them apart.
const (
	reqListing = "listing"   // the board issue list of a listing
	reqRowRead = "rows read" // the board issue list by ID
	reqMatch   = "match"
	reqBulk    = "bulk"
	reqSprints = "sprints" // any sprint read: a board's list or one sprint
)

// ruleScenario is a fake Jira scenario whose board pump (pump) synced the
// board once and stored done's row, the window then moved past done; answer,
// while it returns true, answers a request itself, and counts are the
// requests by kind since the last reset.
type ruleScenario struct {
	*fakeScenario
	scope  Scope
	conn   Connection
	pump   *boardPump
	shift  atomic.Int64
	mu     sync.Mutex
	answer func(kind string, w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy) bool
	counts map[string]int
}

func newRuleScenario(t *testing.T) *ruleScenario {
	t.Helper()
	s := &ruleScenario{fakeScenario: newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		old := at.Add(-HistoryWindow + 2*time.Hour)
		site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: done, Key: "WEB-4", Project: "WEB", Type: "10002",
			Summary: "Done long ago", Status: "10002", Created: old, Updated: old})
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
	}, "example.atlassian.net"), counts: map[string]int{}}
	s.proxied(t, func(rp *httputil.ReverseProxy) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			kind := requestKind(r)
			s.mu.Lock()
			s.counts[kind]++
			answer := s.answer
			s.mu.Unlock()
			if answer == nil || !answer(kind, w, r, rp) {
				rp.ServeHTTP(w, r)
			}
		})
	})
	s.conn = s.addJira()
	s.writeToken(s.conn, "fake-jira-token-1")
	s.scope = Scope{Connection: s.conn.ID, Board: "1"}
	s.listBoards(s.conn.ID, s.scope.Board)
	s.svc.now = func() time.Time { return time.Now().Add(time.Duration(s.shift.Load())) }
	s.pump = &boardPump{scope: s.scope}
	s.syncBoard(t, s.scope)
	if !s.rowStored(t, s.conn.ID, done) {
		t.Fatalf("the first sync stored no row of %s", done)
	}
	s.shift.Store(int64(2 * time.Hour))
	return s
}

// requestKind is what r is to a reconcile.
func requestKind(r *http.Request) string {
	switch p := r.URL.Path; {
	case p == "/rest/software/1.0/board/1/issue" && strings.Contains(r.URL.Query().Get("jql"), "id in ("):
		return reqRowRead
	case p == "/rest/software/1.0/board/1/issue" && r.URL.Query().Get("fields") == "id":
		return reqListing
	case p == "/rest/api/3/jql/match":
		return reqMatch
	case p == "/rest/api/3/issue/bulkfetch":
		return reqBulk
	case strings.Contains(p, "/sprint"):
		return reqSprints
	}
	return r.Method + " " + r.URL.Path
}

// answering makes f answer requests from now on, the counts reset.
func (s *ruleScenario) answering(f func(kind string, w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer, s.counts = f, map[string]int{}
}

// count is the requests of kind since the counts were last reset.
func (s *ruleScenario) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[kind]
}

// reconcileRequests are the requests since the counts were last reset
// that a reconcile makes.
func (s *ruleScenario) reconcileRequests() int {
	return s.count(reqListing) + s.count(reqRowRead) + s.count(reqMatch) + s.count(reqBulk) + s.count(reqSprints)
}

// stored is the board's stored sync state.
func (s *ruleScenario) stored(t *testing.T) store.MirrorSync {
	t.Helper()
	root, ok, err := s.st.GetMirrorRoot(s.conn.ID)
	if err != nil || !ok {
		t.Fatalf("the mirror root of %s: %v, %v", s.conn.ID, ok, err)
	}
	st, _, err := s.st.GetMirrorSync(root, string(s.scope.Board), s.svc.steady().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// failed reports whether the board's stored sync state holds a failure,
// in either slot.
func (s *ruleScenario) failed(t *testing.T) bool {
	t.Helper()
	st := s.stored(t)
	return st.Failure.Kind != "" || st.ReconcileFailure.Kind != ""
}

// refused reports whether the board's stored reconcile failure is a
// refusal of the listing's rule.
func (s *ruleScenario) refused(t *testing.T) bool {
	t.Helper()
	return s.stored(t).ReconcileFailure.Refused
}

// pumpTick runs a tick of the board as its pump does, every unit its due
// rule (boardDue) picks.
func (s *ruleScenario) pumpTick(t *testing.T) Outcome {
	t.Helper()
	out, _, err := s.svc.syncBoard(t.Context(), s.pump, Background, s.svc.boardDue)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// tick runs a tick of the board's reconcile when its pump's due rule
// (boardDue) has it due, the other units left out so that the counts are
// the reconcile's.
func (s *ruleScenario) tick(t *testing.T) Outcome {
	t.Helper()
	due := func(r *run) BoardDue { return BoardDue{Reconcile: s.svc.boardDue(r).Reconcile} }
	out, _, err := s.svc.syncBoard(t.Context(), s.pump, Background, due)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// refresh runs the reconcile of a person's refresh, which lists the board
// whatever its due rule says.
func (s *ruleScenario) refresh(t *testing.T) Outcome {
	t.Helper()
	out, _, err := s.svc.syncBoard(t.Context(), s.pump, Interactive, func(*run) BoardDue { return BoardDue{Reconcile: true} })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// reply answers w with status and body as Jira's JSON.
func reply(w http.ResponseWriter, status int, body string) bool {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	return true
}

const badRequest = `{"errorMessages":["Bad request"],"errors":{}}`

// A sprint the listing's rule names is deleted between the listing and
// the reads by ID, which Jira refuses: the pass ends with the refusal
// stored, reading no sprint to judge why and listing nothing more, the
// completed issue's row kept. A refresh lists the board again and the
// pass completes, removing that row and clearing the refusal. Counted in
// wire requests.
func TestADeletedSprintEndsThePassUntilARefresh(t *testing.T) {
	t.Parallel()
	s := newRuleScenario(t)
	var deleted atomic.Bool
	s.answering(func(kind string, w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy) bool {
		if kind == reqRowRead && !deleted.Swap(true) {
			del := httptest.NewRequestWithContext(r.Context(), http.MethodDelete, "/rest/agile/1.0/sprint/1", nil)
			del.Header = r.Header.Clone()
			rec := httptest.NewRecorder()
			rp.ServeHTTP(rec, del)
			if rec.Code != http.StatusNoContent {
				t.Errorf("the sprint's delete answered %d: %s", rec.Code, rec.Body)
			}
		}
		return false
	})
	out := s.tick(t)
	if out.Failure == nil || len(out.Changes().Removed) != 0 || !s.refused(t) || !s.rowStored(t, s.conn.ID, done) {
		t.Fatalf("the tick %+v, refused %v; want the refusal stored and the row kept", out, s.refused(t))
	}
	if n, rows, sprints := s.count(reqListing), s.count(reqRowRead), s.count(reqSprints); n != 1 || rows != 1 || sprints != 0 || s.pump.pass.openIn(s.lifetime(t)) {
		t.Fatalf("%d listings, %d rows reads, %d sprint reads, the pass %+v; want 1, 1, 0 and the pass ended", n, rows, sprints, s.pump.pass)
	}
	out = s.refresh(t)
	if out.Failure != nil || out.More() || fmt.Sprint(out.Changes().Removed) != "["+done+"]" {
		t.Fatalf("the refresh %+v; want the completed issue removed and the pass done", out)
	}
	// The refresh's listing, which Jira refuses for the sprint the client
	// still holds, so the provider reads the sprints and lists once more
	// (one BoardIssues call), and the rows read under that listing's rule.
	if n, rows := s.count(reqListing), s.count(reqRowRead); n != 3 || rows != 2 {
		t.Fatalf("%d listings and %d rows reads in all; want 3 and 2", n, rows)
	}
	if s.pump.pass.openIn(s.lifetime(t)) || s.failed(t) {
		t.Fatalf("the pass %+v, the board failed %v; want all clear", s.pump.pass, s.failed(t))
	}
}

// A sprint the listing's rule names is deleted as the match judges an
// issue the list left out, and Jira answers the match with an error for
// it (not judged, not a refusal of the rule): the rows read reads the
// board's configuration again and the rows once more, whose list Jira
// refuses with a 400 for the deleted sprint, so the pass ends with the
// refusal stored. A refresh lists the board again and the pass completes.
// Counted in wire requests.
func TestAMatchNotJudgedForADeletedSprintRecoversByTheList(t *testing.T) {
	t.Parallel()
	s := newRuleScenario(t)
	var deleted atomic.Bool
	s.answering(func(kind string, w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy) bool {
		if kind != reqMatch || deleted.Swap(true) {
			return false
		}
		del := httptest.NewRequestWithContext(r.Context(), http.MethodDelete, "/rest/agile/1.0/sprint/1", nil)
		del.Header = r.Header.Clone()
		rec := httptest.NewRecorder()
		rp.ServeHTTP(rec, del)
		if rec.Code != http.StatusNoContent {
			t.Errorf("the sprint's delete answered %d: %s", rec.Code, rec.Body)
		}
		return reply(w, http.StatusOK, `{"matches":[{"matchedIssues":[],"errors":["The value '1' does not exist for the field 'sprint'."]}]}`)
	})
	out := s.tick(t)
	if out.Failure == nil || !s.refused(t) || !deleted.Load() || s.count(reqListing) != 1 || s.count(reqMatch) != 1 || s.count(reqRowRead) != 2 {
		t.Fatalf("the tick %+v, refused %v, deleted %v after %d listings, %d matches and %d rows reads; want the refusal after 1, 1 and 2",
			out, s.refused(t), deleted.Load(), s.count(reqListing), s.count(reqMatch), s.count(reqRowRead))
	}
	out = s.refresh(t)
	if out.Failure != nil || out.More() || fmt.Sprint(out.Changes().Removed) != "["+done+"]" || s.failed(t) {
		t.Fatalf("the refresh %+v, the board failed %v; want the completed issue removed, the pass done and no failure", out, s.failed(t))
	}
	if s.count(reqListing) != 2 || s.count(reqMatch) != 2 || s.count(reqRowRead) != 3 {
		t.Fatalf("deleted %v after %d listings, %d matches and %d rows reads; want 2, 2 and 3", deleted.Load(), s.count(reqListing), s.count(reqMatch), s.count(reqRowRead))
	}
}

// The match that judges an issue the list left out answers 200 with no
// issue and no account, and the sign-in check that proves that answer
// answers 400: no request that carried the rule was refused, so the
// failure keeps the exact pass, the board is not listed again, and nothing
// is held as a refusal.
func TestASignInCheckRefusedAfterAMatchKeepsThePass(t *testing.T) {
	t.Parallel()
	s := newRuleScenario(t)
	s.answering(func(kind string, w http.ResponseWriter, _ *http.Request, _ *httputil.ReverseProxy) bool {
		switch kind {
		case reqMatch:
			return reply(w, http.StatusOK, `{"matches":[{"matchedIssues":[],"errors":[]}]}`)
		case http.MethodGet + " /rest/api/3/myself":
			return reply(w, http.StatusBadRequest, badRequest)
		}
		return false
	})
	out := s.tick(t)
	if out.Failure == nil || out.Failure.Kind != FailureTracker || len(out.Changes().Removed) != 0 {
		t.Fatalf("the tick %+v; want the sign-in check's failure and nothing removed", out)
	}
	if n, m, checks := s.count(reqListing), s.count(reqMatch), s.count(http.MethodGet+" /rest/api/3/myself"); n != 1 || m != 1 || checks != 1 {
		t.Fatalf("%d listings, %d matches, %d sign-in checks; want 1, 1 and 1", n, m, checks)
	}
	if !slices.Contains(s.pump.pass.left, done) || !s.pump.pass.openIn(s.lifetime(t)) || s.refused(t) {
		t.Fatalf("the pass %+v, refused %v; want the pass open holding %s and no refusal", s.pump.pass, s.refused(t), done)
	}
	if !s.rowStored(t, s.conn.ID, done) {
		t.Fatalf("the failed match removed the row of %s", done)
	}
}

// Jira refuses every read by ID with a 400 while every sprint is valid:
// the reconcile lists once and fails, with no sprint read. Every tick of
// the pump until a reconcile interval passed since the refusal, its delta
// succeeding, does not fail and makes no reconcile request, the refusal
// stored after each; then it lists once. Counted in wire requests.
func TestAPersistent400ListsTheBoardOnceAnInterval(t *testing.T) {
	t.Parallel()
	s := newRuleScenario(t)
	// The ticks are placed against the refusal by shift alone: time the
	// test spends between them must not move the service's clock.
	frozen := time.Now()
	s.svc.now = func() time.Time { return frozen.Add(time.Duration(s.shift.Load())) }
	// The units other than the reconcile run first, so that the ticks
	// below run each only when its interval has passed.
	if _, _, err := s.svc.syncBoard(t.Context(), s.pump, Background, func(r *run) BoardDue {
		d := s.svc.boardDue(r)
		d.Reconcile = false
		return d
	}); err != nil {
		t.Fatal(err)
	}
	s.answering(func(kind string, w http.ResponseWriter, _ *http.Request, _ *httputil.ReverseProxy) bool {
		return kind == reqRowRead && reply(w, http.StatusBadRequest, badRequest)
	})
	if out := s.tick(t); out.Failure == nil || !s.refused(t) || s.count(reqListing) != 1 || s.count(reqSprints) != 0 {
		t.Fatalf("the refused tick %+v, refused %v: %d listings, %d sprint reads; want the refusal, 1 and 0", out, s.refused(t), s.count(reqListing), s.count(reqSprints))
	}
	if !s.rowStored(t, s.conn.ID, done) {
		t.Fatal("the refusal removed the completed issue's row")
	}
	for _, at := range []time.Duration{time.Second, s.svc.cadence.Reconcile / 2, s.svc.cadence.Reconcile - time.Second} {
		s.answering(func(kind string, w http.ResponseWriter, _ *http.Request, _ *httputil.ReverseProxy) bool {
			return kind == reqRowRead && reply(w, http.StatusBadRequest, badRequest)
		})
		s.shift.Store(int64(2*time.Hour + at))
		out := s.pumpTick(t)
		if out.Failure != nil || !s.refused(t) || s.reconcileRequests() != 0 {
			t.Fatalf("the tick %v after the refusal %+v: refused %v after %d reconcile requests; want no failure, the refusal stored and none",
				at, out, s.refused(t), s.reconcileRequests())
		}
		if !slices.ContainsFunc(out.Ran, func(u UnitResult) bool { return u.Unit == UnitDelta }) {
			t.Fatalf("the tick %v after the refusal ran %+v; want its delta", at, out.Ran)
		}
	}
	s.shift.Store(int64(2*time.Hour + s.svc.cadence.Reconcile + time.Second))
	if out := s.tick(t); out.Failure == nil || s.count(reqListing) != 1 || s.count(reqSprints) != 0 {
		t.Fatalf("the tick once the interval passed %+v: %d listings, %d sprint reads; want a failure, 1 and 0", out, s.count(reqListing), s.count(reqSprints))
	}
}

// The bulk read that follows a refused or warned list, and the one that
// follows a match that confirmed an issue the list left out, carry no
// rule: a failure of either is the tick's, of its own kind (a 429's
// deadline the outcome's RetryAt), never a refusal of the rule, so the
// board is not listed again; an answer holding an issue with no ID, one
// not asked or one twice, and one without its issues and issueErrors
// lists (an empty object, an error envelope), is a malformed answer,
// never the asked issue's absence. Each keeps the stored row of the issue
// read and the pass.
func TestAFailedBulkReadIsItsOwnFailureAndKeepsTheRow(t *testing.T) {
	t.Parallel()
	malformed := map[string]string{
		"no ID":     `{"issues":[{"id":"","key":"WEB-4","fields":{}}],"issueErrors":[]}`,
		"not asked": `{"issues":[{"id":"10999","key":"WEB-99","fields":{}}],"issueErrors":[]}`,
		"twice":     `{"issues":[{"id":"10104","key":"WEB-4","fields":{}},{"id":"10104","key":"WEB-4","fields":{}}],"issueErrors":[]}`,
		"no lists":  `{}`,
		"errors":    `{"errorMessages":["Bad request"],"errors":{}}`,
		"null":      `{"issues":null,"issueErrors":[]}`,
	}
	type bulkCase struct {
		name  string
		fault *trackerfake.Fault
		body  string
		kind  FailureKind
	}
	cases := []bulkCase{
		{name: "rate limited", fault: &trackerfake.Fault{Kind: trackerfake.FaultRateLimit, RetryAfter: 120}, kind: FailureRateLimited},
		{name: "forbidden", fault: &trackerfake.Fault{Kind: trackerfake.FaultRefuse, Status: http.StatusForbidden,
			Body: []byte(`{"errorMessages":["Forbidden"],"errors":{}}`)}, kind: FailureForbidden},
	}
	for name, body := range malformed {
		cases = append(cases, bulkCase{name: "malformed, " + name, body: body, kind: FailureTracker})
	}
	for _, path := range []string{"after a refused list", "after a match"} {
		for _, tc := range cases {
			if tc.fault != nil && path == "after a match" {
				continue
			}
			t.Run(path+", "+tc.name, func(t *testing.T) {
				t.Parallel()
				s := newRuleScenario(t)
				if tc.fault != nil {
					f := *tc.fault
					f.Origin = trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
					f.Route = "POST rest/api/3/issue/bulkfetch"
					if err := s.e.AddFault(f); err != nil {
						t.Fatal(err)
					}
				}
				s.answering(func(kind string, w http.ResponseWriter, _ *http.Request, _ *httputil.ReverseProxy) bool {
					switch {
					case kind == reqRowRead && path == "after a refused list":
						return reply(w, http.StatusBadRequest, badRequest)
					case kind == reqRowRead:
						return reply(w, http.StatusOK, `{"isLast":true,"issues":[]}`)
					case kind == reqMatch:
						return reply(w, http.StatusOK, `{"matches":[{"matchedIssues":[`+done+`],"errors":[]}]}`)
					case kind == reqBulk && tc.body != "":
						return reply(w, http.StatusOK, tc.body)
					}
					return false
				})
				before := time.Now()
				out := s.tick(t)
				if out.Failure == nil || out.Failure.Kind != tc.kind || len(out.Changes().Removed) != 0 {
					t.Fatalf("the tick %+v; want a failure of kind %v that removed nothing", out, tc.kind)
				}
				if tc.kind == FailureRateLimited && (out.RetryAt.Before(before.Add(120*time.Second)) || out.RetryAt.After(time.Now().Add(121*time.Second))) {
					t.Errorf("the rate-limited bulk read's outcome retries at %v; want 120s after %v", out.RetryAt, before)
				}
				if n := s.count(reqListing); n != 1 {
					t.Errorf("%d listings; want the board not listed again", n)
				}
				if !s.rowStored(t, s.conn.ID, done) {
					t.Errorf("the failed rows read removed the row of %s", done)
				}
				if !slices.Contains(s.pump.pass.left, done) {
					t.Errorf("the pass %+v; want it kept with %s", s.pump.pass, done)
				}
			})
		}
	}
}

// A bulk read that answers both lists empty, after a refused list and
// after a match that confirmed the issue, says the issue does not exist:
// the pass completes and removes its row.
func TestAnEmptyBulkReadIsTheIssuesAbsence(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"after a refused list", "after a match"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			s := newRuleScenario(t)
			s.answering(func(kind string, w http.ResponseWriter, _ *http.Request, _ *httputil.ReverseProxy) bool {
				switch {
				case kind == reqRowRead && path == "after a refused list":
					return reply(w, http.StatusBadRequest, badRequest)
				case kind == reqRowRead:
					return reply(w, http.StatusOK, `{"isLast":true,"issues":[]}`)
				case kind == reqMatch:
					return reply(w, http.StatusOK, `{"matches":[{"matchedIssues":[`+done+`],"errors":[]}]}`)
				case kind == reqBulk:
					return reply(w, http.StatusOK, `{"issues":[],"issueErrors":[]}`)
				}
				return false
			})
			out := s.tick(t)
			if out.Failure != nil || out.More() || fmt.Sprint(out.Changes().Removed) != "["+done+"]" {
				t.Fatalf("the tick %+v; want the completed issue removed and the pass done", out)
			}
			if s.rowStored(t, s.conn.ID, done) || s.count(reqBulk) != 1 {
				t.Fatalf("the row of %s stored %v after %d bulk reads; want it removed after 1", done, s.rowStored(t, s.conn.ID, done), s.count(reqBulk))
			}
		})
	}
}

// lifetime is the board's stored lifetime.
func (s *ruleScenario) lifetime(t *testing.T) int64 {
	t.Helper()
	root, ok, err := s.st.GetMirrorRoot(s.conn.ID)
	if err != nil || !ok {
		t.Fatalf("the mirror root of %s: %v, %v", s.conn.ID, ok, err)
	}
	st, _, err := s.st.GetMirrorSync(root, string(s.scope.Board), s.svc.steady().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return st.Lifetime
}

// rowStored reports whether connection conn's mirror holds a row of issue.
func (s *fakeScenario) rowStored(t *testing.T, conn string, issue tracker.IssueID) bool {
	t.Helper()
	root, ok, err := s.st.GetMirrorRoot(conn)
	if err != nil || !ok {
		t.Fatalf("the mirror root of %s: %v, %v", conn, ok, err)
	}
	rows, _, err := s.st.ListMirrorIssueRows(root, string(issue), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(rows) > 0
}
