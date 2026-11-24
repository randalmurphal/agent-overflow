package trackerapp

import (
	"net/http"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A reconcile's rows read that Jira refuses with a 400, under a rule
// naming the board's sprints, fails with what judging the 400 found: a
// sprint lookup that fails is the tick's failure, a 429's deadline its
// RetryAt and a 403 refused access; a bulk answer holding an issue that
// was not asked is a malformed answer, not the asked issue's absence.
// Each keeps the stored row of the issue read.
func TestARefusedRowsReadFailsWithWhatJudgingItFound(t *testing.T) {
	t.Parallel()
	const done = "10104"
	for _, tc := range []struct {
		name  string
		fault *trackerfake.Fault
		bulk  string
		kind  FailureKind
	}{
		{name: "sprint lookup rate limited", fault: &trackerfake.Fault{Kind: trackerfake.FaultRateLimit, RetryAfter: 120}, kind: FailureRateLimited},
		{name: "sprint lookup forbidden", fault: &trackerfake.Fault{Kind: trackerfake.FaultRefuse, Status: http.StatusForbidden,
			Body: []byte(`{"errorMessages":["Forbidden"],"errors":{}}`)}, kind: FailureForbidden},
		{name: "bulk answer of an issue not asked", bulk: `{"issues":[{"id":"10999","key":"WEB-99","fields":{}}],"issueErrors":[]}`, kind: FailureTracker},
		{name: "bulk answer of an issue with no ID", bulk: `{"issues":[{"id":"","key":"WEB-4","fields":{}}],"issueErrors":[]}`, kind: FailureTracker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
				site := jiraSeed(at)
				old := at.Add(-HistoryWindow + 2*time.Hour)
				site.Issues = append(site.Issues, trackerfake.JiraIssue{ID: done, Key: "WEB-4", Project: "WEB", Type: "10002",
					Summary: "Done long ago", Status: "10002", Created: old, Updated: old})
				return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
			}, "example.atlassian.net")
			var armed atomic.Bool
			s.proxied(t, func(rp *httputil.ReverseProxy) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case !armed.Load():
					case r.URL.Path == "/rest/software/1.0/board/1/issue" && strings.Contains(r.URL.Query().Get("jql"), "id in ("):
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"errorMessages":["Bad request"],"errors":{}}`))
						return
					case r.URL.Path == "/rest/api/3/issue/bulkfetch" && tc.bulk != "":
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(tc.bulk))
						return
					}
					rp.ServeHTTP(w, r)
				})
			})
			conn := s.addJira()
			s.writeToken(conn, "fake-jira-token-1")
			scope := Scope{Connection: conn.ID, Board: "1"}
			s.listBoards(conn.ID, scope.Board)
			var shift atomic.Int64
			s.svc.now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
			s.syncBoard(t, scope)
			if !s.rowStored(t, conn.ID, done) {
				t.Fatalf("the first sync stored no row of %s", done)
			}
			// The window passes the completed issue: the reconcile's listing
			// leaves it out, so its rows read judges it.
			shift.Store(int64(2 * time.Hour))
			if tc.fault != nil {
				f := *tc.fault
				f.Origin = trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
				f.Route = "GET rest/agile/1.0/sprint/{sprint}"
				if err := s.e.AddFault(f); err != nil {
					t.Fatal(err)
				}
			}
			armed.Store(true)
			before := time.Now()
			out, err := s.svc.syncTick(t.Context(), scope, BoardDue{Reconcile: true}, Background)
			if err != nil || out.Failure == nil || out.Failure.Kind != tc.kind || len(out.Changes().Removed) != 0 {
				t.Fatalf("the refused rows read: %+v, %v; want a failure of kind %v that removed nothing", out, err, tc.kind)
			}
			if tc.kind == FailureRateLimited && (out.RetryAt.Before(before.Add(120*time.Second)) || out.RetryAt.After(time.Now().Add(121*time.Second))) {
				t.Errorf("the rate-limited lookup's outcome retries at %v; want 120s after %v", out.RetryAt, before)
			}
			if !s.rowStored(t, conn.ID, done) {
				t.Errorf("the failed rows read removed the row of %s", done)
			}
		})
	}
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
