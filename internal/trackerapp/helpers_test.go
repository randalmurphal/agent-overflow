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
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/tempo/tempotest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

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

// fixture is a Service over a cloned store, with loopback Jira, Tempo and
// Linear fakes as the origins of every connection.
type fixture struct {
	t       *testing.T
	st      *store.Store
	svc     *Service
	creds   string
	jira    *jiratest.Server
	tempo   *tempotest.Server
	linear  *lineartest.Server
	changes atomic.Int64
	// origin, when set, replaces the fakes as the Origin.
	origin func(kind, site string) (string, error)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, st: storetest.Clone(t), creds: filepath.Join(t.TempDir(), keyfile.DirName)}
	f.jira = jiratest.NewServer(t)
	f.tempo = tempotest.NewServer(t, tempoToken)
	f.linear = lineartest.NewServer(t)
	svc, err := New(Deps{
		Store:          f.st,
		Changed:        func() { f.changes.Add(1) },
		CredentialsDir: f.creds,
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
		Now:     func() time.Time { return time.UnixMilli(1_800_000_000_000) },
		Life:    t.Context(),
	})
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
