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

// TestCallSaysWhetherItsFrameWasSent: a call that fails before its frame
// reaches the socket says so, and the far side reads nothing; a call the
// far side read and never answered does not.
func TestCallSaysWhetherItsFrameWasSent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received := make(chan string, 4)
	hangUp := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		go func() {
			<-hangUp
			_ = conn.CloseNow()
		}()
		for {
			var request transport.ClientFrame
			if err := wsjson.Read(ctx, conn, &request); err != nil {
				return
			}
			received <- request.Method
		}
	}))
	defer server.Close()
	dial := func() *Client {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		client := New(conn)
		t.Cleanup(client.Close)
		return client
	}
	nothingReceived := func(what string) {
		t.Helper()
		select {
		case method := <-received:
			t.Fatalf("%s: the far side read %s", what, method)
		default:
		}
	}

	client := dial()
	if err := client.Call(ctx, "Unencodable", nil, make(chan int)); err == nil || !NotSent(err) {
		t.Fatalf("an argument that cannot be encoded = %v, want it not sent", err)
	}
	var notAPointer struct{}
	if err := client.Call(ctx, "NoDestination", notAPointer); err == nil || !NotSent(err) {
		t.Fatalf("a result that cannot be written = %v, want it not sent", err)
	}
	done, stop := context.WithCancel(ctx)
	stop()
	if err := client.Call(done, "Cancelled", nil); !errors.Is(err, context.Canceled) || !NotSent(err) {
		t.Fatalf("a call whose context had ended = %v, want it not sent", err)
	}

	// Read by the far side, then abandoned: it may have run.
	call, abandon := context.WithCancel(ctx)
	failed := make(chan error, 1)
	go func() { failed <- client.Call(call, "Unanswered", nil) }()
	select {
	case method := <-received:
		if method != "Unanswered" {
			t.Fatalf("the far side read %s first", method)
		}
	case <-ctx.Done():
		t.Fatal("the far side never read the call")
	}
	abandon()
	if err := <-failed; !errors.Is(err, context.Canceled) || NotSent(err) {
		t.Fatalf("a call the far side read = %v, want it maybe sent", err)
	}

	// A connection that has already ended sends nothing.
	ended := dial()
	if err := ended.Call(ctx, "StartsTheReader", nil, make(chan int)); !NotSent(err) {
		t.Fatalf("an unencodable call = %v, want it not sent", err)
	}
	close(hangUp)
	select {
	case <-ended.Done():
	case <-ctx.Done():
		t.Fatal("the hang-up went unnoticed")
	}
	if err := ended.Call(ctx, "AfterTheEnd", nil); err == nil || !NotSent(err) {
		t.Fatalf("a call on an ended connection = %v, want it not sent", err)
	}
	nothingReceived("before or after the abandoned call")
}
