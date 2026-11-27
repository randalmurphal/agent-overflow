package trackerapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/keyfile"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil/providerpath"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/tempo/tempotest"
)

// TestMain keeps every test from running an installed claude or codex
// found on PATH.
func TestMain(m *testing.M) { os.Exit(providerpath.Guard(func() int { return storetest.Run(m) })) }

// Recognizable tokens, searched for on every surface a test can read.
const (
	jiraToken  = "jira-SECRET-token-0001"
	tempoToken = "tempo-SECRET-token-0002"
	linearKey  = lineartest.Key
	jiraEmail  = "user1@example.com"
	jiraSiteIn = "Example.atlassian.net/"
	jiraSiteAt = "https://example.atlassian.net"
	myselfPath = "/rest/api/3/myself"
	myselfJSON = `{"accountId":"acc-user1","displayName":"User 1","emailAddress":"user1@example.com","active":true}`
	viewerJSON = `{"data":{"viewer":{"id":"user-1","name":"User 1","displayName":"user1","email":"user1@example.com","avatarUrl":null,"active":true,"admin":false,"guest":false,"app":false,"isMe":true},"organization":{"id":"org-1","name":"Example","urlKey":"%s"}}}`
	settings   = "/4/globalconfiguration"
)

// token is test token n (a, b, c...) for a test that replaces the token
// in a key file: long enough for the transport, which refuses a secret no
// longer than secret.Redacted.
// untied is a board subscribe's tie for a test with no client to tie
// the handle to.
func untied(string) (func(), error) { return nil, nil }

func token(n string) string { return "token-" + n + "-0000000001" }

// fixture is a Service over a cloned store, with loopback Jira, Tempo and
// Linear fakes as the origins of every connection.
type fixture struct {
	t  *testing.T
	st *store.Store
	// file is st's path, for a test that writes a state no store method
	// writes (sqlOnStore).
	file    string
	svc     *Service
	creds   string
	jira    *jiratest.Server
	tempo   *tempotest.Server
	linear  *lineartest.Server
	changes atomic.Int64
	// skew moves the service's clock (Deps.Now) forward, in nanoseconds;
	// pass moves it.
	skew atomic.Int64
	// ticking makes each read of the clock a millisecond later than the
	// last, ticks counting them (tickingClock).
	ticking atomic.Bool
	ticks   atomic.Int64
	// origin, when set, replaces the fakes as the Origin.
	origin func(kind, site string) (string, error)
	// events records what the service emits and reports.
	events *eventLog
	// deps made svc; restart makes another service from them.
	deps Deps
}

// testCallbacks fills d's event, settle, client and loop callbacks: the
// events go to log, the first reads have settled and a client is
// connected unless log says otherwise.
func testCallbacks(d Deps, log *eventLog) Deps {
	d.EmitBoard = log.board
	d.EmitIssue = log.issue
	d.Settled = log.waitSettled
	d.ClientConnected = func() bool { return !log.disconnected.Load() }
	d.LoopFailed = log.loopFailed
	d.SetupChanged = func() { log.setups.Add(1) }
	d.DashboardsChanged = func(c DashboardsChange) {
		log.lastDashboards.Store(&c)
		log.dashboards.Add(1)
		if f := log.onDashboards.Load(); f != nil {
			(*f)()
		}
	}
	d.LinksChanged = log.linksChanged
	return d
}

func newFixture(t *testing.T) *fixture { return newFixtureWith(t, Cadence{}) }

// pass moves the service's clock d forward, as time passing would, safe
// while the service's goroutines read it.
func (f *fixture) pass(d time.Duration) { f.skew.Add(int64(d)) }

// newFixtureWith is newFixture with the service's sync intervals cad.
func newFixtureWith(t *testing.T, cad Cadence) *fixture {
	t.Helper()
	st, file := storetest.CloneFile(t)
	f := &fixture{t: t, st: st, file: file, creds: filepath.Join(t.TempDir(), keyfile.DirName), events: newEventLog()}
	f.jira = jiratest.NewServer(t)
	f.tempo = tempotest.NewServer(t, tempoToken)
	f.linear = lineartest.NewServer(t)
	f.deps = testCallbacks(Deps{
		Store:          f.st,
		Changed:        func() { f.changes.Add(1) },
		CredentialsDir: f.creds,
		ConfigDir:      t.TempDir(),
		Origin: func(kind, site string) (string, error) {
			if f.origin != nil {
				return f.origin(kind, site)
			}
			switch kind {
			case store.TrackerKindJira:
				return f.jira.URL(), nil
			case store.TrackerKindTempo:
				return f.tempo.URL(), nil
			}
			return f.linear.URL(), nil
		},
		Version: "1.2.3",
		Now: func() time.Time {
			now := time.UnixMilli(1_800_000_000_000).Add(time.Duration(f.skew.Load()))
			if f.ticking.Load() {
				now = now.Add(time.Duration(f.ticks.Add(1)) * time.Millisecond)
			}
			return now
		},
		Life:    t.Context(),
		Cadence: cad,
	}, f.events)
	svc, err := New(f.deps)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	t.Cleanup(svc.Stop)
	return f
}

func (f *fixture) add(c NewConnection) Connection {
	f.t.Helper()
	conn, err := f.svc.Add(c)
	if err != nil {
		f.t.Fatalf("Add(%+v): %v", c, err)
	}
	return conn
}

func (f *fixture) addJira() Connection {
	return f.add(NewConnection{Kind: store.TrackerKindJira, Site: jiraSiteIn, Account: " User1@Example.com "})
}

func (f *fixture) addTempo(parent string) Connection {
	return f.add(NewConnection{Kind: store.TrackerKindTempo, Site: tempo.EUBaseURL, ParentID: parent})
}

func (f *fixture) addLinear() Connection {
	return f.add(NewConnection{Kind: store.TrackerKindLinear, Site: "https://linear.app/example/"})
}

// listBoards makes boards the whole list of boards an app project lists
// through connection id, one project a connection, as a person picking
// the boards of a project would; the mirror syncs only listed boards.
func (f *fixture) listBoards(id string, boards ...tracker.BoardID) {
	f.t.Helper()
	listBoards(f.t, f.st, id, boards...)
}

func listBoards(tb testing.TB, st *store.Store, id string, boards ...tracker.BoardID) {
	tb.Helper()
	project := store.Project{ID: "boards-" + id, Path: "/tmp/boards-" + id, Name: "boards", CreatedAt: 1, UpdatedAt: 1}
	if _, err := st.GetProject(project.ID); err != nil {
		if _, err := st.CreateProject(project); err != nil {
			tb.Fatal(err)
		}
	}
	list := make([]store.ProjectTrackerBoard, len(boards))
	for i, b := range boards {
		list[i] = store.ProjectTrackerBoard{ConnectionID: id, BoardID: string(b)}
	}
	current, err := st.ListProjectTrackerBoards(project.ID)
	if err != nil {
		tb.Fatal(err)
	}
	held := make([]store.HeldTrackerBoard, len(current))
	for i, b := range current {
		held[i] = store.HeldTrackerBoard{ConnectionID: b.ConnectionID, BoardID: b.BoardID, OpensFirst: b.OpensFirst}
	}
	if _, _, err := st.ReplaceProjectTrackerBoards(project.ID, held, list); err != nil {
		tb.Fatalf("list boards %v: %v", boards, err)
	}
}

// writeToken puts contents in conn's key file, as the person pasting a
// token into it would.
func (f *fixture) writeToken(conn Connection, contents string) {
	f.t.Helper()
	if err := os.WriteFile(conn.KeyFilePath, []byte(contents), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) verify(id string) Connection {
	f.t.Helper()
	conn, err := f.svc.Verify(context.Background(), id)
	if err != nil {
		f.t.Fatalf("Verify(%s): %v", id, err)
	}
	return conn
}

func (f *fixture) row(id string) store.TrackerConnection {
	f.t.Helper()
	row, err := f.st.GetTrackerConnection(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

func (f *fixture) inUse(id string) bool {
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	return f.svc.clients[id] != nil
}

// basic is the Authorization a Jira request with token carries.
func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(jiraEmail+":"+token))
}

// myself answers the sign-in check for token. A request with no
// credential is answered 401, as Jira answers it: the transport sends one
// when ForgetCredential dropped the credential between its read and the
// send, and reads the key file again. Any other credential fails the
// test.
func myself(t *testing.T, token string) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch got := r.Header.Get("Authorization"); got {
		case basic(token):
			jiratest.Reply(http.StatusOK, myselfJSON)(w, r, nil)
		case "":
			jiratest.Reply(http.StatusUnauthorized, `{}`)(w, r, nil)
		default:
			t.Errorf("GET myself carried %q", got)
			jiratest.Reply(http.StatusUnauthorized, `{}`)(w, r, nil)
		}
	}
}

// blocked answers nothing until its request ends, after telling arrived
// that it is in.
func blocked(arrived chan<- struct{}) jiratest.Handler {
	return func(_ http.ResponseWriter, r *http.Request, _ []byte) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}
}

// echoing answers status with a body that repeats the request's
// credential, as a careless tracker might.
func echoing(status int) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		auth := r.Header.Get("Authorization")
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"errorMessages":["refused %s (%s)"]}`, auth, raw))
	}
}

// leakChecker gathers every surface a caller can read: errors, the
// connections as JSON and the failures, and fails the test if any holds
// a token in any of its forms.
type leakChecker struct {
	mu       sync.Mutex
	surfaces []string
}

func (l *leakChecker) add(v any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch v := v.(type) {
	case nil:
	case error:
		l.surfaces = append(l.surfaces, v.Error(), fmt.Sprintf("%+v", v))
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		l.surfaces = append(l.surfaces, string(raw), fmt.Sprintf("%+v", v))
	}
}

func (l *leakChecker) check(t *testing.T) {
	t.Helper()
	forms := []string{jiraToken, tempoToken, linearKey, basic(jiraToken), strings.TrimPrefix(basic(jiraToken), "Basic ")}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.surfaces) == 0 {
		t.Fatal("no surface was gathered")
	}
	for _, s := range l.surfaces {
		for _, form := range forms {
			if strings.Contains(s, form) {
				t.Errorf("a surface holds a token: %s", s)
			}
		}
	}
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
