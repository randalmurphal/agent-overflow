package attachedbackends

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestDeviceNameSyncChecksPeerAndPersistsOnlySuccess(t *testing.T) {
	for _, tc := range []struct {
		name, peer                  string
		capability, refuse, success bool
	}{
		{"success", "peer", true, false, true}, {"rename during failure", "peer", true, false, true}, {"rename while finishing", "peer", true, false, true}, {"wrong identity", "other", true, false, false},
		{"old peer", "peer", false, false, false}, {"refusal", "peer", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var renamed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/ticket" {
					json.NewEncoder(w).Encode(map[string]string{"ticket": "test"})
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), time.Second)
				defer cancel()
				caps := []string{}
				if tc.capability {
					caps = append(caps, transport.CapabilityDeviceName)
				}
				if wsjson.Write(ctx, conn, map[string]any{"type": "hello", "backendId": tc.peer, "protocolVersion": transport.ProtocolVersion, "capabilities": caps}) != nil {
					return
				}
				var frame transport.ClientFrame
				if wsjson.Read(ctx, conn, &frame) != nil {
					return
				}
				call := calls.Add(1)
				if frame.Method != "UpdateClientDeviceName" || len(frame.Params) != 2 {
					t.Error("wrong RPC")
				}
				var name string
				json.Unmarshal(frame.Params[0], &name)
				expected := "Studio"
				failingRename := tc.name == "rename during failure" && call == 1
				if failingRename {
					expected = "Previous"
					renamed.Store(true)
				}
				if name != expected {
					t.Errorf("name=%q, want %q", name, expected)
				}
				response := map[string]any{"type": "rpc", "id": frame.ID, "result": nil}
				if tc.refuse || failingRename {
					response["error"] = map[string]string{"code": "denied", "message": "denied"}
				}
				wsjson.Write(ctx, conn, response)
			}))
			defer server.Close()
			manager, dir := newManager(t)
			seed(t, dir, deviceclient.Session{BackendID: "peer", Endpoint: server.URL, SessionID: "session", Credential: "credential", Label: "old", Nickname: "my override", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()})
			c, err := manager.carrier("peer")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if tc.name == "rename during failure" {
				c.labelGetter = func() (string, error) {
					if renamed.Load() {
						return "Studio", nil
					}
					return "Previous", nil
				}
				c.syncDeviceName()
				for c.client.Session().Label != "Studio" || c.nameSync.Load() {
					select {
					case <-ctx.Done():
						t.Fatal("lost newer name after failed RPC")
					case <-time.After(time.Millisecond):
					}
				}
				if calls.Load() != 2 {
					t.Fatalf("calls=%d, want failed old name then new name", calls.Load())
				}
			} else if tc.name == "rename while finishing" {
				var first atomic.Bool
				c.labelGetter = func() (string, error) {
					if first.CompareAndSwap(false, true) {
						// A rename notification arrives while the worker is
						// deciding that its previous name needs no update.
						c.syncDeviceName()
						return "old", nil
					}
					return "Studio", nil
				}
				c.syncDeviceName()
				for c.client.Session().Label != "Studio" || c.nameSync.Load() {
					select {
					case <-ctx.Done():
						t.Fatal("lost a rename at worker completion")
					case <-time.After(time.Millisecond):
					}
				}
			} else {
				err = c.sendDeviceName(ctx, "Studio")
				if (err == nil) != tc.success {
					t.Fatalf("success=%v error=%v", tc.success, err)
				}
			}
			got, err := deviceclient.LoadSession(dir, "peer")
			if err != nil {
				t.Fatal(err)
			}
			want := "old"
			if tc.success {
				want = "Studio"
			}
			if got.Label != want || got.Nickname != "my override" || got.Credential != "credential" {
				t.Fatalf("profile=%+v", got)
			}
			if !tc.capability || tc.peer != "peer" {
				if calls.Load() != 0 {
					t.Fatal("mutated unverified peer")
				}
			}
		})
	}
}
