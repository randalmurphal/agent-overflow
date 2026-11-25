package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

func lineartestViewer(urlKey string) lineartest.Exchange {
	return lineartest.Exchange{Op: "Viewer", Body: fmt.Sprintf(viewerJSON, urlKey)}
}

// linearCheck is the Viewer a Linear client sends with each key it reads,
// to learn the key's workspace, before any call carries the key.
func linearCheck(key, urlKey string) lineartest.Exchange {
	return lineartest.Exchange{Op: "Viewer", Key: key, Body: fmt.Sprintf(viewerJSON, urlKey)}
}

func TestVerifyRecordsWhoTheTokenBelongsTo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	f.writeToken(jira, "\n  "+jiraToken+"\n")
	f.jira.On("GET", myselfPath, myself(t, jiraToken))
	lin := f.addLinear()
	f.writeToken(lin, linearKey)
	f.linear.Expect(linearCheck(linearKey, "Example"), lineartestViewer("Example"))
	tmp := f.addTempo(jira.ID)
	f.writeToken(tmp, tempoToken)
	f.tempo.On("GET", settings, nil, "", 200, `{"weekStart":1}`).Repeat = true

	for _, tc := range []struct {
		conn       Connection
		id, name   string
		wantChange bool
	}{
		{jira, "acc-user1", "User 1", true},
		{lin, "user-1", "User 1", true},
		{tmp, "", "", true},
	} {
		before := f.changes.Load()
		got := f.verify(tc.conn.ID)
		if got.Problem != nil || got.VerifiedAccountID != tc.id || got.VerifiedName != tc.name || got.VerifiedAt != 1_800_000_000_000 {
			t.Errorf("Verify of %s = %+v, problem %v", tc.conn.Kind, got.TrackerConnection, got.Problem)
		}
		if row := f.row(tc.conn.ID); row.VerifiedAt != 1_800_000_000_000 || row.VerifiedAccountID != tc.id {
			t.Errorf("the %s row records %+v", tc.conn.Kind, row)
		}
		if f.changes.Load() != before+1 {
			t.Errorf("Verify of %s did not tell Changed", tc.conn.Kind)
		}
	}
	// Tempo's settings are cached by its client; a second verification
	// still asks Tempo.
	f.verify(tmp.ID)
	if got := len(f.tempo.Requests()); got != 2 {
		t.Fatalf("two Tempo verifications sent %d requests", got)
	}
}

// Every key file state and every answer becomes the connection's
// problem, not an error; a later success clears it.
func TestVerifyFailuresBecomeTheProblem(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		contents string // "" leaves the file empty; "-" removes it
		handler  jiratest.Handler
		kind     FailureKind
		message  string
		// rateGateShut marks a failure that shut the client's rate gate,
		// so the later success needs a new client.
		rateGateShut bool
	}{
		"an empty key file":    {"", nil, FailureSetup, "the Jira API token file is empty", false},
		"a missing key file":   {"-", nil, FailureSetup, "there is no Jira API token file", false},
		"a refused key file":   {"one two", nil, FailureSetup, "holds more than one token", false},
		"a refused token":      {jiraToken, echoing(http.StatusUnauthorized), FailureSetup, "Jira refused the token", false},
		"a token without room": {jiraToken, echoing(http.StatusForbidden), FailureForbidden, "HTTP 403", false},
		"a rate limit": {jiraToken, func(w http.ResponseWriter, r *http.Request, b []byte) {
			w.Header().Set("Retry-After", "3600")
			echoing(http.StatusTooManyRequests)(w, r, b)
		}, FailureRateLimited, "Jira is limiting requests until", true},
		"a server error": {jiraToken, echoing(http.StatusInternalServerError), FailureTracker, "Jira answered HTTP 500", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			conn := f.addJira()
			switch tc.contents {
			case "":
			case "-":
				if err := os.Remove(conn.KeyFilePath); err != nil {
					t.Fatal(err)
				}
			default:
				f.writeToken(conn, tc.contents)
			}
			if tc.handler != nil {
				f.jira.On("GET", myselfPath, tc.handler)
			}
			before := f.changes.Load()
			got := f.verify(conn.ID)
			if got.Problem == nil || got.Problem.Kind != tc.kind || !strings.Contains(got.Problem.Message, tc.message) {
				t.Fatalf("problem %+v, want %s with %q", got.Problem, tc.kind, tc.message)
			}
			if got.VerifiedAt != 0 || f.changes.Load() != before+1 {
				t.Fatalf("a failed Verify recorded %+v (changes %d)", got.TrackerConnection, f.changes.Load()-before)
			}
			if tc.handler == nil && len(f.jira.Requests()) != 0 {
				t.Fatalf("a key file with no token sent %v", f.jira.Requests())
			}
			list, err := f.svc.Connections()
			if err != nil || list[0].Problem == nil || *list[0].Problem != *got.Problem {
				t.Fatalf("Connections shows %+v, %v", list, err)
			}

			f.writeToken(conn, jiraToken)
			f.jira.On("GET", myselfPath, myself(t, jiraToken))
			if tc.rateGateShut {
				if _, err := f.svc.SetEnabled(conn.ID, false); err != nil {
					t.Fatal(err)
				}
				if _, err := f.svc.SetEnabled(conn.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			if got := f.verify(conn.ID); got.Problem != nil || got.VerifiedAccountID != "acc-user1" {
				t.Fatalf("a later success = %+v, problem %v", got.TrackerConnection, got.Problem)
			}
		})
	}
}

func TestVerifyKeyFileStatesShowInTheList(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	for _, tc := range []struct {
		contents string
		state    keyfile.State
	}{
		{"", keyfile.StateEmpty},
		{"a b", keyfile.StateRefused},
		{linearKey, keyfile.StatePresent},
	} {
		f.writeToken(conn, tc.contents)
		list, err := f.svc.Connections()
		if err != nil || list[0].KeyFile.State != tc.state {
			t.Errorf("%q shows %+v, %v; want %s", tc.contents, list, err, tc.state)
		}
	}
	if err := os.Remove(conn.KeyFilePath); err != nil {
		t.Fatal(err)
	}
	if list, err := f.svc.Connections(); err != nil || list[0].KeyFile.State != keyfile.StateMissing {
		t.Errorf("a removed file shows %+v, %v", list, err)
	}
}

// A key of another workspace is refused by the connection's client before
// any call carries it: provider reads and writes fail unsent, Verify and
// the list show the setup problem, and the refusal holds until the key
// file holds another key. Then, with no restart, Verify checks the new key
// and the same provider's calls flow.
func TestALinearKeyOfAnotherWorkspaceIsRefusedUntilReplaced(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	const otherKey = "lin_api_fake1111111111"
	f.writeToken(conn, otherKey)
	f.linear.Expect(linearCheck(otherKey, "other-co"))
	p := f.provider(conn.ID)
	refused := func(what string, err error) {
		t.Helper()
		_, wrong := errors.AsType[*linear.WrongWorkspaceError](err)
		_, notSent := errors.AsType[*trackerapi.NotSentError](err)
		if !wrong || !notSent {
			t.Fatalf("%s with another workspace's key = %v, want it refused unsent", what, err)
		}
	}
	_, err := p.Projects(t.Context())
	refused("a read", err)
	write := p.AddComment(t.Context(), tracker.CommentInput{Issue: "issue-1", Markdown: "hello"})
	if len(write.Parts) != 1 || write.Parts[0].Outcome.State() != tracker.StateNotSent {
		t.Fatalf("a write with another workspace's key = %+v", write)
	}
	refused("a write", write.Parts[0].Outcome.Err())
	setup := "this Linear API key belongs to the workspace other-co, not example; make a key in example"
	for range 2 {
		got := f.verify(conn.ID)
		list, err := f.svc.Connections()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []Connection{got, list[0]} {
			if c.Problem == nil || c.Problem.Kind != FailureSetup || c.Problem.Message != setup || c.VerifiedAt != 0 {
				t.Fatalf("a key of another workspace shows %+v, problem %+v", c.TrackerConnection, c.Problem)
			}
		}
	}
	// The fake answers only the one check expected; any other request
	// with the refused key fails the test.

	teams := lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))}
	f.writeToken(conn, linearKey)
	f.linear.Expect(linearCheck(linearKey, "example"), lineartestViewer("example"), teams)
	if got := f.verify(conn.ID); got.Problem != nil || got.VerifiedAt == 0 {
		t.Fatalf("Verify after the key was replaced = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
	if _, err := p.Projects(t.Context()); err != nil {
		t.Fatalf("a read after the key was replaced = %v", err)
	}

	// Another workspace's key put back in the file is checked once a list
	// sees it, and refused.
	f.writeToken(conn, otherKey)
	f.linear.Expect(linearCheck(otherKey, "other-co"))
	if _, err := f.svc.Connections(); err != nil {
		t.Fatal(err)
	}
	_, err = p.Projects(t.Context())
	refused("a read after the key file was replaced again", err)

	// A key read again after a 401 is checked too.
	f.writeToken(conn, linearKey)
	f.linear.Expect(linearCheck(linearKey, "example"), lineartestViewer("example"))
	f.verify(conn.ID)
	f.writeToken(conn, otherKey)
	unauthorized := teams
	unauthorized.Status = http.StatusUnauthorized
	unauthorized.Body = `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`
	f.linear.Expect(unauthorized, linearCheck(otherKey, "other-co"))
	_, err = p.Projects(t.Context())
	if _, wrong := errors.AsType[*linear.WrongWorkspaceError](err); !wrong {
		t.Fatalf("a read whose 401 made the client read another workspace's key = %v", err)
	}
}

// A key Linear refuses (401 on the workspace check) is a refused token,
// as it was before the client checked workspaces.
func TestALinearKeyRefusedAtTheCheckIsARefusedToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	f.writeToken(conn, linearKey)
	f.linear.Expect(lineartest.Exchange{Op: "Viewer", Status: http.StatusUnauthorized,
		Body: `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`})
	got := f.verify(conn.ID)
	if got.Problem == nil || got.Problem.Kind != FailureSetup ||
		got.Problem.Message != "Linear refused the token in the key file; put a valid token in it" {
		t.Fatalf("a key refused at the check = %+v", got.Problem)
	}
}

func TestVerifyTempoRefusalNamesTheRegion(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	tmp := f.addTempo(jira.ID)
	f.writeToken(tmp, tempoToken)
	// The client reads the key file again after a 401, then gives up.
	f.tempo.On("GET", settings, nil, "", 401, `{"errors":[{"message":"bad token `+tempoToken+`"}]}`).Repeat = true
	got := f.verify(tmp.ID)
	if got.Problem == nil || got.Problem.Kind != FailureSetup || !strings.Contains(got.Problem.Message, "region") {
		t.Fatalf("a refused Tempo token = %+v", got.Problem)
	}
}

// With no origin for its tracker, a connection's client is never built,
// nothing is sent, and the reason is the connection's problem.
func TestVerifyWithoutAnOriginSendsNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.origin = func(string, string) (string, error) { return "", errors.New("tracker requests are off in this run") }
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	got := f.verify(conn.ID)
	if got.Problem == nil || got.Problem.Kind != FailureSetup || !strings.Contains(got.Problem.Message, "tracker requests are off in this run") {
		t.Fatalf("Verify without an origin = %+v", got.Problem)
	}
	if f.inUse(conn.ID) || len(f.jira.Requests()) != 0 {
		t.Fatalf("a connection without an origin reached %v", f.jira.Requests())
	}
	if _, err := f.svc.Provider(t.Context(), conn.ID); !errors.As(err, new(*ClientError)) {
		t.Fatalf("Provider without an origin = %v", err)
	}
}

// No error, connection or problem carries a token in any form, even when
// the tracker repeats it in its answer.
func TestNoSurfaceCarriesTheToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var seen leakChecker
	jira := f.addJira()
	f.writeToken(jira, jiraToken)
	lin := f.addLinear()
	f.writeToken(lin, linearKey)
	tmp := f.addTempo(jira.ID)
	f.writeToken(tmp, tempoToken)

	f.tempo.On("GET", settings, nil, "", 500, `{"errors":[{"message":"echo `+tempoToken+`"}]}`).Repeat = true
	f.linear.Expect(lineartest.Exchange{Op: "Viewer", Status: 400,
		Body: `{"errors":[{"message":"key ` + linearKey + ` refused","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`})
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusInternalServerError} {
		f.jira.On("GET", myselfPath, echoing(status))
		got, err := f.svc.Verify(t.Context(), jira.ID)
		seen.add(got)
		seen.add(err)
	}
	for _, id := range []string{lin.ID, tmp.ID} {
		got, err := f.svc.Verify(t.Context(), id)
		seen.add(got)
		seen.add(err)
		if got.Problem == nil {
			t.Fatalf("Verify of %s did not fail", id)
		}
	}
	list, err := f.svc.Connections()
	seen.add(list)
	seen.add(err)
	p, err := f.svc.Provider(t.Context(), jira.ID)
	seen.add(err)
	_, err = p.Identity(t.Context())
	seen.add(err)
	seen.check(t)
}

// A tracker that echoes the token in a successful answer (Jira's display
// name and account id, Linear's viewer name at the workspace check and
// the call, a Tempo setting) puts it in no returned value, stored row or
// error. Changed, the only event the service raises, carries nothing.
func TestNoSurfaceCarriesATokenEchoedInASuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var seen leakChecker
	jira := f.addJira()
	f.writeToken(jira, jiraToken)
	lin := f.addLinear()
	f.writeToken(lin, linearKey)
	tmp := f.addTempo(jira.ID)
	f.writeToken(tmp, tempoToken)

	encoded := strings.TrimPrefix(basic(jiraToken), "Basic ")
	f.jira.On("GET", myselfPath, jiratest.Reply(http.StatusOK,
		`{"accountId":"`+encoded+`","displayName":"`+jiraToken+` (`+basic(jiraToken)+`)","emailAddress":"user1@example.com","active":true}`))
	echo := strings.NewReplacer(`"name":"User 1"`, `"name":"`+linearKey+`"`, `"displayName":"user1"`, `"displayName":"`+linearKey+`"`)
	viewer := echo.Replace(fmt.Sprintf(viewerJSON, "example"))
	f.linear.Expect(
		lineartest.Exchange{Op: "Viewer", Body: viewer},
		lineartest.Exchange{Op: "Viewer", Body: viewer},
		lineartest.Exchange{Op: "Viewer", Body: viewer},
	)
	f.tempo.On("GET", settings, nil, "", 200, `{"approvalPeriod":"`+tempoToken+`","weekStart":1}`).Repeat = true

	for _, id := range []string{jira.ID, lin.ID, tmp.ID} {
		got, err := f.svc.Verify(t.Context(), id)
		if err != nil || got.Problem != nil || got.VerifiedAt == 0 {
			t.Fatalf("Verify of %s = %+v, %v, problem %v", id, got.TrackerConnection, err, got.Problem)
		}
		seen.add(got)
		seen.add(f.row(id))
	}
	for _, id := range []string{jira.ID, lin.ID} {
		p, err := f.svc.Provider(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		user, err := p.Identity(t.Context())
		seen.add(user)
		seen.add(err)
	}
	list, err := f.svc.Connections()
	seen.add(list)
	seen.add(err)
	seen.check(t)
}

// An older verification that answers after a newer one changes nothing:
// neither the row's verification nor the problem.
func TestAnOlderVerifyNeverOverwritesANewerOne(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ older, newer int }{
		"older fails after newer succeeds": {http.StatusForbidden, http.StatusOK},
		"older succeeds after newer fails": {http.StatusOK, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			conn := f.addJira()
			f.writeToken(conn, jiraToken)
			arrived := make(chan struct{})
			release := make(chan struct{})
			var n atomic.Int32
			f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				status := tc.newer
				if n.Add(1) == 1 {
					arrived <- struct{}{}
					<-release
					status = tc.older
				}
				jiratest.Reply(status, myselfJSON)(w, r, nil)
			})
			older := make(chan Connection, 1)
			go func() {
				got, err := f.svc.Verify(context.Background(), conn.ID)
				if err != nil {
					t.Error(err)
				}
				older <- got
			}()
			<-arrived
			newer := f.verify(conn.ID)
			close(release)
			late := <-older
			list, err := f.svc.Connections()
			if err != nil {
				t.Fatal(err)
			}
			for what, got := range map[string]Connection{"the older Verify's answer": late, "the list": list[0]} {
				if (got.Problem == nil) != (newer.Problem == nil) || got.VerifiedAt != newer.VerifiedAt {
					t.Errorf("%s = verified %d, problem %v; the newer Verify left verified %d, problem %v",
						what, got.VerifiedAt, got.Problem, newer.VerifiedAt, newer.Problem)
				}
			}
			if row := f.row(conn.ID); row.VerifiedAt != newer.VerifiedAt {
				t.Errorf("the row's verification is %d, want the newer Verify's %d", row.VerifiedAt, newer.VerifiedAt)
			}
		})
	}
}

// rows is the connection list, failing the test on an error.
func (f *fixture) rows() []Connection {
	f.t.Helper()
	list, err := f.svc.Connections()
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

// A check counts for the token in the key file only while the file holds
// the token it verified: a token moved into place with an older time is
// not it, the checked token put back is, and a restart keeps the binding.
func TestAVerificationCountsWhileTheFileHoldsItsToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	f.jira.On("GET", myselfPath, myself(t, jiraToken))
	if got := f.verify(conn.ID); !got.CheckedTokenInFile || got.VerifiedAccountID != "acc-user1" {
		t.Fatalf("a verified token = %+v, checked token in file %v", got.TrackerConnection, got.CheckedTokenInFile)
	}

	// Another token, saved an hour before the check, moved over the file.
	saved := filepath.Join(filepath.Dir(conn.KeyFilePath), "saved")
	if err := os.WriteFile(saved, []byte(token("b")), 0o600); err != nil {
		t.Fatal(err)
	}
	hourAgo := time.Now().Add(-time.Hour)
	if err := os.Chtimes(saved, hourAgo, hourAgo); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, conn.KeyFilePath); err != nil {
		t.Fatal(err)
	}
	if row := f.rows()[0]; row.CheckedTokenInFile || row.VerifiedAccountID != "acc-user1" {
		t.Fatalf("a token moved over the checked one lists as checked: %+v", row)
	}

	f.writeToken(conn, jiraToken)
	f.restart(t)
	if row := f.rows()[0]; !row.CheckedTokenInFile {
		t.Fatalf("the checked token, after a restart, lists as not checked: %+v", row)
	}
}

// A fresh check's failure is about the token the file holds, however the
// file was saved: the same token pasted and saved again keeps its 403
// current, the same failure of the same token tells no one, and a token
// saved after it makes the 403 history until that token's own failure.
func TestAFailureIsAboutTheTokenItsCallUsed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	f.jira.On("GET", myselfPath, echoing(http.StatusForbidden))
	if got := f.verify(conn.ID); got.Problem == nil || got.Problem.Kind != FailureForbidden || !got.Problem.Current {
		t.Fatalf("a 403 = problem %+v", got.Problem)
	}
	f.writeToken(conn, jiraToken)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(conn.KeyFilePath, later, later); err != nil {
		t.Fatal(err)
	}
	before := f.changes.Load()
	if got := f.verify(conn.ID); got.Problem == nil || !got.Problem.Current {
		t.Fatalf("a 403 after the same token was saved again = problem %+v", got.Problem)
	}
	if f.changes.Load() != before {
		t.Fatal("the same failure of the same token told the list")
	}

	f.writeToken(conn, token("b"))
	if row := f.rows()[0]; row.Problem == nil || row.Problem.Current {
		t.Fatalf("a 403 of the token the file no longer holds lists as current: %+v", row.Problem)
	}
	before = f.changes.Load()
	if got := f.verify(conn.ID); got.Problem == nil || !got.Problem.Current {
		t.Fatalf("the new token's 403 = problem %+v", got.Problem)
	}
	if f.changes.Load() != before+1 {
		t.Fatal("the new token's failure did not tell the list")
	}
	// A token replaced with no list between: the check's own key file
	// read hands it to the client, and the failure is about it.
	f.writeToken(conn, token("c"))
	if got := f.verify(conn.ID); got.Problem == nil || !got.Problem.Current {
		t.Fatalf("a check of a token no list read yet = problem %+v", got.Problem)
	}
}

// A connection printed whole names no token digest.
func TestAPrintedConnectionCarriesNoTokenDigest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	f.jira.On("GET", myselfPath, echoing(http.StatusForbidden))
	got := f.verify(conn.ID)
	digest := tokenDigest(conn.ID, []byte(jiraToken))
	printed := fmt.Sprintf("%v %+v %#v %v", got, got, *got.Problem, got.Problem)
	for _, form := range []string{fmt.Sprintf("%d", digest[:]), fmt.Sprintf("%x", digest[:]), fmt.Sprintf("%d %d", digest[0], digest[1])} {
		if strings.Contains(printed, form) {
			t.Fatalf("a printed connection carries its token digest: %s", printed)
		}
	}
}

// A key file that gave no token is the failure while the file holds
// none, and history once it holds one.
func TestAKeyFileReadFailureIsHistoryOnceTheFileHoldsAToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	if got := f.verify(conn.ID); got.Problem == nil || !strings.Contains(got.Problem.Message, "is empty") || !got.Problem.Current {
		t.Fatalf("a check of an empty key file = problem %+v", got.Problem)
	}
	f.writeToken(conn, jiraToken)
	if row := f.rows()[0]; row.Problem == nil || row.Problem.Current {
		t.Fatalf("an empty key file's failure lists as current once the file holds a token: %+v", row.Problem)
	}
}

// A token that changed while the call ran (the file was replaced and the
// client read it again after a 401) could have answered either way, so
// the failure is history, not the new token's; a check settles it. The
// same holds on a client that had sent nothing before the call.
func TestAFailureDuringATokenChangeIsAboutNeither(t *testing.T) {
	t.Parallel()
	for _, warm := range []bool{true, false} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			conn := f.addJira()
			f.writeToken(conn, jiraToken)
			if warm {
				f.jira.On("GET", myselfPath, myself(t, jiraToken))
				f.verify(conn.ID)
			}
			var calls atomic.Int32
			f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, body []byte) {
				if calls.Add(1) == 1 {
					if err := os.WriteFile(conn.KeyFilePath, []byte(token("b")), 0o600); err != nil {
						t.Error(err)
					}
				}
				echoing(http.StatusUnauthorized)(w, r, body)
			})
			got := f.verify(conn.ID)
			if calls.Load() != 2 {
				t.Fatalf("the identity call was sent %d times, want a retry after the 401", calls.Load())
			}
			if got.Problem == nil || got.Problem.Kind != FailureSetup || got.Problem.Current {
				t.Fatalf("a refusal answered while the token changed = problem %+v", got.Problem)
			}
			if got := f.verify(conn.ID); got.Problem == nil || !got.Problem.Current {
				t.Fatalf("the next check's refusal = problem %+v", got.Problem)
			}
		})
	}
}

// A check of token A on a client that has sent nothing, whose answer
// arrived before the file was replaced with B and a check of B began,
// binds no token: its User 1 is not recorded as B's, before B's own
// check fails or after a restart.
func TestACheckWhoseTokenChangedBeforeItSettledBindsNoToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	arrivedB, releaseB := make(chan struct{}, 1), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseB) })
	t.Cleanup(release) // before the fake's own, which waits for its handlers
	f.jira.On("GET", myselfPath, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch got := r.Header.Get("Authorization"); got {
		case basic(jiraToken):
			jiratest.Reply(http.StatusOK, myselfJSON)(w, r, body)
		case basic(token("b")):
			arrivedB <- struct{}{}
			<-releaseB
			jiratest.Reply(http.StatusUnauthorized, `{}`)(w, r, body)
		default:
			t.Errorf("GET myself carried %q", got)
			jiratest.Reply(http.StatusUnauthorized, `{}`)(w, r, body)
		}
	})
	var paused atomic.Bool
	checkB := make(chan Connection, 1)
	f.svc.afterIdentity = func() {
		if paused.Swap(true) {
			return // B's check
		}
		f.writeToken(conn, token("b"))
		go func() {
			got, err := f.svc.Verify(context.Background(), conn.ID)
			if err != nil {
				t.Errorf("B's Verify: %v", err)
			}
			checkB <- got
		}()
		<-arrivedB
	}
	gotA := f.verify(conn.ID)
	if gotA.CheckedTokenInFile || gotA.VerifiedToken.Known() || gotA.VerifiedAccountID != "acc-user1" {
		t.Fatalf("A's check, settled after B was handed = %+v, checked token in file %v", gotA.TrackerConnection, gotA.CheckedTokenInFile)
	}
	release()
	if got := <-checkB; got.CheckedTokenInFile || got.Problem == nil || !got.Problem.Current {
		t.Fatalf("B's refused check = %+v, checked token in file %v, problem %+v", got.TrackerConnection, got.CheckedTokenInFile, got.Problem)
	}
	f.restart(t)
	if row := f.rows()[0]; row.CheckedTokenInFile || row.VerifiedAccountID != "acc-user1" {
		t.Fatalf("after a restart, B lists as %+v, checked token in file %v", row.TrackerConnection, row.CheckedTokenInFile)
	}
}
