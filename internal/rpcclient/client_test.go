package rpcclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestCallReplacesSnapshotAndPreservesItOnDecodeFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	replies := []string{
		`{"members":[{"key":"host","backendId":"computer"},{"key":"phone"}],"count":2}`,
		`{"members":[{"key":"phone"},{"key":"host","backendId":"computer"}],"count":2}`,
		`{"members":[{"key":"partially decoded"}],"count":"invalid"}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for _, reply := range replies {
			var request transport.ClientFrame
			if err := wsjson.Read(ctx, conn, &request); err != nil {
				t.Error(err)
				return
			}
			if err := wsjson.Write(ctx, conn, transport.ServerFrame{Type: "rpc", ID: request.ID, Result: json.RawMessage(reply)}); err != nil {
				t.Error(err)
				return
			}
		}
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	defer client.Close()
	type member struct {
		Key       string `json:"key"`
		BackendID string `json:"backendId,omitempty"`
	}
	var result struct {
		Members []member `json:"members"`
		Count   int      `json:"count"`
	}
	if err := client.Call(ctx, "List", &result); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "List", &result); err != nil {
		t.Fatal(err)
	}
	if result.Members[0].BackendID != "" || result.Members[1].BackendID != "computer" {
		t.Fatalf("omitted fields retained an earlier row's identity: %+v", result)
	}
	before := append([]member(nil), result.Members...)
	if err := client.Call(ctx, "List", &result); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if !reflect.DeepEqual(result.Members, before) || result.Count != 2 {
		t.Fatalf("decode failure changed last good snapshot: %+v", result)
	}
}
