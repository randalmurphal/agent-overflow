package rpcclient

import (
	"context"
	"encoding/json"
	"errors"
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

// TestDoneReportsAConnectionEndedWhileIdle: between calls the reader skips
// what the far side pushes and notices when it hangs up.
func TestDoneReportsAConnectionEndedWhileIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hangUp := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for range 2 {
			var request transport.ClientFrame
			if err := wsjson.Read(ctx, conn, &request); err != nil {
				t.Error(err)
				return
			}
			for range 3 {
				_ = wsjson.Write(ctx, conn, transport.ServerFrame{Type: "event", Channel: "thread:updated"})
			}
			_ = wsjson.Write(ctx, conn, transport.ServerFrame{Type: "rpc", ID: request.ID, Result: json.RawMessage(`1`)})
		}
		<-hangUp
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	defer client.Close()
	for _, method := range []string{"One", "Two"} {
		if err := client.Call(ctx, method, nil); err != nil {
			t.Fatal(err)
		}
	}
	close(hangUp)
	select {
	case <-client.Done():
	case <-ctx.Done():
		t.Fatal("an idle connection's end went unnoticed")
	}
	if err := client.Call(ctx, "Three", nil); err == nil {
		t.Fatal("a call on an ended connection succeeded")
	}
}

// TestACancelledCallEndsTheConnection: the abandoned reply must not be read
// as the next call's answer, so the connection goes with the call.
func TestACancelledCallEndsTheConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		var request transport.ClientFrame
		_ = wsjson.Read(ctx, conn, &request)
		close(received)
		_, _, _ = conn.Read(ctx)
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	defer client.Close()
	call, stop := context.WithCancel(ctx)
	go func() { <-received; stop() }()
	if err := client.Call(call, "Unanswered", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: %v", err)
	}
	select {
	case <-client.Done():
	case <-ctx.Done():
		t.Fatal("the connection outlived the cancelled call")
	}
}
