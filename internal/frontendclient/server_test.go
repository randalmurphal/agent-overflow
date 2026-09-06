package frontendclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestFrontendOpensAndManagesItsComputersWithoutContactingThem(t *testing.T) {
	kerneltest.IsolateSpawns(t)
	var requests atomic.Int32
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "computer unavailable", http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	profiles := t.TempDir()
	for _, id := range []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"} {
		if err := deviceclient.SaveSession(profiles, deviceclient.Session{BackendID: id, BackendName: id,
			Endpoint: remote.URL, SessionID: "fixture-session", Credential: "fixture-credential", RefreshSecret: "fixture-refresh"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Profiles: profiles, ConfigDir: t.TempDir(), ClientID: "fixture-client", ComputerID: "11111111-1111-4111-8111-111111111111",
		Assets: fstest.MapFS{"index.html": {Data: []byte("frontend fixture")}}, Version: "test"}
	configured := 0
	cfg.ConfigureUpdater = func(service *appupdate.Service) {
		if service == nil {
			t.Fatal("missing frontend updater")
		}
		configured++
	}
	s, err := Serve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	page, err := url.Parse(s.AppURL())
	if err != nil {
		t.Fatal(err)
	}
	if page.Query().Get("mode") != "frontend" || page.Query().Get("computer") != cfg.ComputerID || page.Query().Get("cid") != cfg.ClientID {
		t.Fatalf("wrong frontend page identity: %s", page)
	}
	bootstrap := func() transport.Bootstrap {
		t.Helper()
		ticket, err := s.MintPageTicket()
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.Get("http://" + s.Addr() + "/bootstrap.json?t=" + url.QueryEscape(ticket))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("bootstrap: %d", res.StatusCode)
		}
		var manifest transport.Bootstrap
		if err := json.NewDecoder(res.Body).Decode(&manifest); err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	manifest := bootstrap()
	if manifest.BackendID != "" || manifest.ReplicaGeneration != "" || len(manifest.Backends) != 2 {
		t.Fatalf("controller claimed an execution store or lost computers: %+v", manifest)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+s.Addr()+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + s.transport.Token()}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	call := func(method string, args ...any) transport.ServerFrame {
		t.Helper()
		params := make([]json.RawMessage, len(args))
		for i, arg := range args {
			params[i], _ = json.Marshal(arg)
		}
		if err := wsjson.Write(ctx, conn, transport.ClientFrame{Type: "rpc", ID: "call", Method: method, Params: params}); err != nil {
			t.Fatal(err)
		}
		for {
			var frame transport.ServerFrame
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.ID == "call" {
				return frame
			}
		}
	}
	if result := call("ListBackends"); result.Error != nil || !strings.Contains(string(result.Result), cfg.ComputerID) {
		t.Fatalf("computer list: %+v", result)
	}
	if result := call("CheckForUpdate"); result.Error != nil || !strings.Contains(string(result.Result), `"supported":false`) || configured != 1 {
		t.Fatalf("local updater boot/status: %+v, configured=%d", result, configured)
	}
	if result := call("HighlightCode", map[string]string{"lang": "go", "source": "func answer() int { return 42 }"}); result.Error != nil || !strings.Contains(string(result.Result), `"lang":"go"`) || !strings.Contains(string(result.Result), `"r":`) {
		t.Fatalf("local stateless highlighting: %+v", result)
	}
	if result := call("GetThread", "irrelevant"); result.Error == nil || result.Error.Code != transport.ErrCodeMethodNotFound {
		t.Fatalf("controller exposed execution: %+v", result)
	}
	if result := call("RemoveBackend", cfg.ComputerID); result.Error != nil {
		t.Fatalf("remove: %+v", result)
	}
	if len(bootstrap().Backends) != 1 {
		t.Fatal("removing the launch target lost another computer")
	}
	if requests.Load() != 0 {
		t.Fatalf("opening/managing the frontend contacted a computer %d times", requests.Load())
	}
	// The local root contains presentation files, never an execution history DB.
	matches, err := filepath.Glob(filepath.Join(cfg.ConfigDir, "*.db"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("unexpected local database: %v %v", matches, err)
	}
	res, err := http.Get("http://" + s.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(body) != "frontend fixture" {
		t.Fatalf("assets: %q %v", body, err)
	}
}
