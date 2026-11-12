package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The host window is the one client holding the launch credential that the
// backend's own process shows: its page ticket is a window ticket, delivered
// by injection, and its exchange plants the host-window cookie beside the
// page cookie. Every request and connection carrying that cookie is the host
// window; a page that bought only the page cookie (a browser tab on this
// machine) is not, nor is a forged window cookie or a bearer-token client. A
// harness has no window, and its pages stand in for it.
func TestOnlyTheHostWindowsOwnPageIsTheHostWindow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		harness   bool
		mint      func(srv *Server) (string, error)
		forge     bool
		wantFirst bool
		wantAfter bool
	}{
		{name: "a window ticket", mint: (*Server).MintWindowTicket, wantFirst: true, wantAfter: true},
		{name: "a page ticket", mint: (*Server).MintPageTicket},
		{name: "a page ticket with a forged window cookie", mint: (*Server).MintPageTicket, forge: true},
		{name: "a page ticket on a harness", harness: true, mint: (*Server).MintPageTicket, wantFirst: true, wantAfter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostWindowFixture(t, tc.harness)
			ticket, err := tc.mint(f.srv)
			if err != nil {
				t.Fatal(err)
			}
			first := bootstrapWithTicket(t, f.addr, ticket)
			if got := decodeHostWindow(t, first); got != tc.wantFirst {
				t.Fatalf("first contact hostWindow = %v, want %v", got, tc.wantFirst)
			}
			cookies := first.Cookies()
			window := false
			for _, cookie := range cookies {
				if strings.HasPrefix(cookie.Name, windowCookiePrefix) {
					window = true
					if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Value == ticket {
						t.Fatalf("window cookie %+v is not an HttpOnly, Strict credential of its own", cookie)
					}
				}
			}
			if window != (tc.wantFirst && !tc.harness) {
				t.Fatalf("window cookie set %v", window)
			}
			if tc.forge {
				cookies = append(cookies, &http.Cookie{Name: cookieNameForHost(windowCookiePrefix, f.addr), Value: "forged"})
			}

			req, err := http.NewRequest(http.MethodGet, "http://"+f.addr+"/bootstrap.json", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, cookie := range cookies {
				req.AddCookie(cookie)
			}
			refetch, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeHostWindow(t, refetch); got != tc.wantAfter {
				t.Fatalf("refetch hostWindow = %v, want %v", got, tc.wantAfter)
			}

			header := http.Header{}
			parts := make([]string, 0, len(cookies))
			for _, cookie := range cookies {
				parts = append(parts, cookie.Name+"="+cookie.Value)
			}
			header.Set("Cookie", strings.Join(parts, "; "))
			if got := f.amHostWindow(t, "ws://"+f.addr+"/ws", header); got != tc.wantAfter {
				t.Fatalf("connection HostWindow = %v, want %v", got, tc.wantAfter)
			}
		})
	}
}

// A client presenting the launch token itself (a native process, the
// harness CLI) is not the host window, unless the backend is a harness.
func TestATokenClientIsNotTheHostWindow(t *testing.T) {
	for _, harness := range []bool{false, true} {
		f := newHostWindowFixture(t, harness)
		if got := f.amHostWindow(t, "ws://"+f.addr+"/ws?token=integration-token", nil); got != harness {
			t.Fatalf("harness %v: a token client's HostWindow = %v", harness, got)
		}
	}
}

type hostWindowFixture struct {
	srv  *Server
	addr string
}

func newHostWindowFixture(t *testing.T, harness bool) *hostWindowFixture {
	t.Helper()
	d := NewDispatcher()
	if _, err := d.Register(&integrationStub{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv, err := New(Config{Dispatcher: d, EventBus: NewEventBus(8), Token: "integration-token", Harness: harness})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	srv.MarkReady()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return &hostWindowFixture{srv: srv, addr: srv.Addr()}
}

func (f *hostWindowFixture) amHostWindow(t *testing.T, url string, header http.Header) bool {
	t.Helper()
	conn, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	resp := callRPC(t, conn, "AmHostWindow")
	if resp.Error != nil {
		t.Fatalf("AmHostWindow: %+v", resp.Error)
	}
	var got bool
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func decodeHostWindow(t *testing.T, resp *http.Response) bool {
	t.Helper()
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close the bootstrap body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap = %d", resp.StatusCode)
	}
	var manifest Bootstrap
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	return manifest.HostWindow
}
