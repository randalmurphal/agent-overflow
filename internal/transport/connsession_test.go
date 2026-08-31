package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestTheConnectionPrincipalReachesABoundMethod — the session a handler
// scopes durable state by must be the one the UPGRADE admitted, never
// something a later frame supplied. Both halves ride one ConnState, and
// this pins that they arrive together and survive to the RPC.
func TestTheConnectionPrincipalReachesABoundMethod(t *testing.T) {
	f := newSessionFixture(t)
	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+f.addr+"/ws?token=integration-token&did=screen-abcdef01&conn=live-abcdef01", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	if got, want := whoAmI(t, conn), "sess-1|screen-abcdef01"; got != want {
		t.Fatalf("principal = %q, want %q", got, want)
	}
}

// TestAConnectionNamingNoSessionStillCarriesItsScreen — every
// launch-credential client (the harness CLI, the e2e rig) presents no
// session and must stay attributable, or the scope they fall back to would
// have nothing to key on.
func TestAConnectionNamingNoSessionStillCarriesItsScreen(t *testing.T) {
	f := newSessionFixtureWith(t, func(cfg *Config) { cfg.SessionForRequest = nil })
	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+f.addr+"/ws?token=integration-token&did=screen-abcdef01", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	if got, want := whoAmI(t, conn), "|screen-abcdef01"; got != want {
		t.Fatalf("principal = %q, want %q", got, want)
	}
}

func whoAmI(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	resp := callRPC(t, conn, "WhoAmI")
	if resp.Error != nil {
		t.Fatalf("WhoAmI: %+v", resp.Error)
	}
	var got string
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return got
}

// TestWatchWindowsApplyTheDefaultsAndTheExemptions pins the resolution
// table in one place, because every branch of it is a decision that is
// invisible at the call site.
func TestWatchWindowsApplyTheDefaultsAndTheExemptions(t *testing.T) {
	cases := []struct {
		name         string
		recheck      time.Duration
		lifetime     time.Duration
		loopback     bool
		canCheck     bool
		wantRecheck  time.Duration
		wantLifetime time.Duration
	}{
		{
			name: "remote defaults", canCheck: true,
			wantRecheck: defaultSessionRecheck, wantLifetime: defaultRemoteConnLifetime,
		},
		{
			name: "loopback keeps the re-check and loses the cap", loopback: true, canCheck: true,
			wantRecheck: defaultSessionRecheck, wantLifetime: 0,
		},
		{
			name: "no hook means no re-check", canCheck: false,
			wantRecheck: 0, wantLifetime: defaultRemoteConnLifetime,
		},
		{
			name: "negative disables either half", recheck: -1, lifetime: -1, canCheck: true,
			wantRecheck: 0, wantLifetime: 0,
		},
		{
			name: "explicit values survive", recheck: time.Second, lifetime: time.Hour, canCheck: true,
			wantRecheck: time.Second, wantLifetime: time.Hour,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recheck, lifetime := resolveWatchWindows(tc.recheck, tc.lifetime, tc.loopback, tc.canCheck)
			if recheck != tc.wantRecheck {
				t.Errorf("recheck = %s, want %s", recheck, tc.wantRecheck)
			}
			if lifetime != tc.wantLifetime {
				t.Errorf("lifetime = %s, want %s", lifetime, tc.wantLifetime)
			}
		})
	}
}

// TestAConnectionClosesWhenItsSessionStopsBeingLive is the case
// revocation's synchronous teardown does NOT cover: a session that simply
// expired, or one revoked by something that is not this process. Without
// the interval re-check such a connection streams until the client
// disconnects.
func TestAConnectionClosesWhenItsSessionStopsBeingLive(t *testing.T) {
	f := newSessionFixtureWith(t, func(cfg *Config) {
		cfg.SessionRecheckInterval = 20 * time.Millisecond
	})
	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+f.addr+"/ws?token=integration-token", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	deadline := time.Now().Add(2 * time.Second)
	for f.srv.SessionConns().CountForSession("sess-1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the connection never joined the live-session registry")
		}
		time.Sleep(2 * time.Millisecond)
	}

	f.setSessionDead("sess-1")

	readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(readCtx); err != nil {
			break
		}
		if readCtx.Err() != nil {
			t.Fatal("the connection outlived the session it named")
		}
	}
}

// TestAConnectionNamingNoSessionIsNeverReChecked — the ordinary local
// connection today. Arming a watchdog for it would spend a goroutine and a
// timer per connection to ask a question nothing can answer.
func TestAConnectionNamingNoSessionIsNeverReChecked(t *testing.T) {
	f := newSessionFixtureWith(t, func(cfg *Config) {
		cfg.SessionRecheckInterval = 10 * time.Millisecond
	})
	f.session = ""
	conn, _, err := websocket.Dial(context.Background(),
		"ws://"+f.addr+"/ws?token=integration-token", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// Every session id is dead. A connection that named one would be gone
	// within a couple of ticks; this one must still answer.
	f.setSessionDead("")
	f.setSessionDead("sess-1")
	time.Sleep(60 * time.Millisecond)

	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err != nil {
		t.Fatalf("a sessionless connection was torn down: %v", err)
	}
}

// TestCloseCauseNamesEveryServerSideTeardown — all three cancel the
// connection context, which at the terminal error alone is
// indistinguishable from a server shutdown.
func TestCloseCauseNamesEveryServerSideTeardown(t *testing.T) {
	cases := []struct {
		cause int32
		want  string
	}{
		{closeCauseNone, "server shutdown"},
		{closeCauseRevoked, "session revoked"},
		{closeCauseSessionEnded, "session no longer live"},
		{closeCauseLifetime, "connection lifetime reached"},
	}
	for _, tc := range cases {
		h := &connHandler{}
		h.closeCause.Store(tc.cause)
		if got := h.closeReason(context.Canceled); got != tc.want {
			t.Errorf("cause %d reported as %q, want %q", tc.cause, got, tc.want)
		}
	}
}
