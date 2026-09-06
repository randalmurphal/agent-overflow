package localcontrol

import (
	"agent-overflow/internal/transport"
	"context"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateRendezvousLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := Publish(root, "0.0.0.0:8123", "first-launch"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, Filename))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private endpoint permissions: %v, %v", info, err)
	}
	endpoint, err := Read(root)
	if err != nil || endpoint.Address != "127.0.0.1:8123" {
		t.Fatalf("read = %+v, %v", endpoint, err)
	}
	if err := Publish(root, "0.0.0.0:8124", "next-launch"); err != nil {
		t.Fatal(err)
	}
	if err := Withdraw(root, "first-launch"); err != nil {
		t.Fatal(err)
	}
	endpoint, err = Read(root)
	if err != nil || endpoint.Token != "next-launch" {
		t.Fatal("old shutdown withdrew the new endpoint")
	}
	if err := Withdraw(root, "next-launch"); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root); err == nil {
		t.Fatal("endpoint remained after shutdown")
	}
}
func TestRefusesNonlocalRendezvous(t *testing.T) {
	for _, address := range []string{"192.168.1.2:8123", "localhost:8123", "127.0.0.1:0", "127.0.0.1:65536"} {
		if err := Publish(t.TempDir(), address, "launch"); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
}
func TestClientUsesExistingRPCWireWithoutSubscribing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != transport.WSPath || r.Header.Get("Authorization") != "Bearer private-launch" {
			t.Error("missing local credential or wrong route")
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "hello"})
		var request transport.ClientFrame
		if err := wsjson.Read(r.Context(), conn, &request); err != nil {
			t.Error(err)
			return
		}
		if request.Type != "rpc" || request.Method != "DevicePairingStatus" || len(request.Params) != 1 {
			t.Errorf("unexpected request: %+v", request)
			return
		}
		_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "rpc", "id": request.ID, "result": map[string]string{"state": "pending"}})
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	client, err := Dial(t.Context(), Endpoint{Address: strings.TrimPrefix(server.URL, "http://"), Token: "private-launch"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var answer map[string]string
	if err := client.Call(context.Background(), "DevicePairingStatus", &answer, "link"); err != nil {
		t.Fatal(err)
	}
	if answer["state"] != "pending" {
		t.Fatalf("answer: %v", answer)
	}
}
