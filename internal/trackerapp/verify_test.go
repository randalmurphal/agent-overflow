package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/linear/lineartest"
)

func lineartestViewer(urlKey string) lineartest.Exchange {
	return lineartest.Exchange{Op: "Viewer", Body: fmt.Sprintf(viewerJSON, urlKey)}
}

func TestVerifyRecordsWhoTheTokenBelongsTo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	jira := f.addJira()
	f.writeToken(jira, "\n  "+jiraToken+"\n")
	f.jira.On("GET", myselfPath, myself(t, jiraToken))
	lin := f.addLinear()
	f.writeToken(lin, linearKey)
	f.linear.Expect(lineartestViewer("Example"))
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

func TestVerifyRefusesAKeyForAnotherWorkspace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	f.writeToken(conn, linearKey)
	f.linear.Expect(lineartestViewer("other-co"), lineartestViewer("Not A Key!"))
	got := f.verify(conn.ID)
	if got.Problem == nil || got.Problem.Kind != FailureSetup ||
		!strings.Contains(got.Problem.Message, "belongs to the workspace other-co, not example") || got.VerifiedAt != 0 {
		t.Fatalf("a key of another workspace = %+v, problem %+v", got.TrackerConnection, got.Problem)
	}
	got = f.verify(conn.ID)
	if got.Problem == nil || strings.Contains(got.Problem.Message, "Not A Key") || !strings.Contains(got.Problem.Message, "another workspace than example") {
		t.Fatalf("a workspace that is no URL key = %+v", got.Problem)
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

func TestVerifyDuringDisableRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	arrived := make(chan struct{})
	f.jira.On("GET", myselfPath, blocked(arrived))
	done := make(chan Connection)
	errs := make(chan error)
	go func() {
		got, err := f.svc.Verify(context.Background(), conn.ID)
		done <- got
		errs <- err
	}()
	<-arrived
	if _, err := f.svc.SetEnabled(conn.ID, false); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if err := <-errs; err != nil {
		t.Fatalf("Verify ended by a disable = %v", err)
	}
	if got.Enabled || got.Problem != nil || got.VerifiedAt != 0 || f.row(conn.ID).VerifiedAt != 0 {
		t.Fatalf("Verify ended by a disable = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
	if f.inUse(conn.ID) {
		t.Fatal("the disabled connection has a client")
	}
}

func TestVerifyDuringRemoveIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	f.writeToken(conn, linearKey)
	arrived := make(chan struct{})
	f.linear.Expect(lineartest.Exchange{Op: "Viewer", Wait: true, Arrived: arrived})
	errs := make(chan error)
	go func() {
		_, err := f.svc.Verify(context.Background(), conn.ID)
		errs <- err
	}()
	<-arrived
	if err := f.svc.Remove(conn.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; !isNotFound(err) {
		t.Fatalf("Verify ended by a remove = %v, want ErrNotFound", err)
	}
}

func TestStopEndsAVerifyInFlight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	arrived := make(chan struct{})
	f.jira.On("GET", myselfPath, blocked(arrived))
	errs := make(chan error, 1)
	go func() {
		_, err := f.svc.Verify(context.Background(), conn.ID)
		errs <- err
	}()
	<-arrived
	f.svc.Stop()
	if err := <-errs; !errors.Is(err, ErrStopped) {
		t.Fatalf("Verify ended by Stop = %v", err)
	}
	if f.row(conn.ID).VerifiedAt != 0 {
		t.Fatal("a Verify Stop ended recorded a verification")
	}
}

func TestCallsAfterStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	f.svc.Stop()
	f.svc.Stop()
	for name, call := range map[string]func() error{
		"Add":            func() error { _, err := f.svc.Add(NewConnection{Kind: "linear", Site: "other"}); return err },
		"Connections":    func() error { _, err := f.svc.Connections(); return err },
		"Rename":         func() error { _, err := f.svc.Rename(conn.ID, "x"); return err },
		"SetTokenExpiry": func() error { _, err := f.svc.SetTokenExpiry(conn.ID, 1); return err },
		"SetEnabled":     func() error { _, err := f.svc.SetEnabled(conn.ID, false); return err },
		"Verify":         func() error { _, err := f.svc.Verify(t.Context(), conn.ID); return err },
		"Remove":         func() error { return f.svc.Remove(conn.ID) },
		"PrepareKeyFile": func() error { _, err := f.svc.PrepareKeyFile(conn.ID); return err },
		"TokenPage":      func() error { _, err := f.svc.TokenPage(conn.ID); return err },
		"Provider":       func() error { _, err := f.svc.Provider(t.Context(), conn.ID); return err },
	} {
		if err := call(); !errors.Is(err, ErrStopped) {
			t.Errorf("%s after Stop = %v", name, err)
		}
	}
	if row := f.row(conn.ID); row.Label != "" || !row.Enabled {
		t.Fatalf("a call after Stop changed the row: %+v", row)
	}
}
