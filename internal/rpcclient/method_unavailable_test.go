package rpcclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// methodUnavailableSentence is the text both sides show for a call the far
// side does not have. Pinned here so a change to either side's sentence
// fails this package.
const methodUnavailableSentence = "The computer this reached runs a different version of Agent Overflow and does not support this. Update Agent Overflow so both run the same version."

// TestMethodNotFoundSaysTheVersionMismatch: a computer that answers a call
// with method_not_found reads, wherever its error is shown, as a version
// mismatch with what to do, and keeps its wire text for diagnosis.
func TestMethodNotFoundSaysTheVersionMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		var request transport.ClientFrame
		if err := wsjson.Read(ctx, conn, &request); err != nil {
			t.Error(err)
			return
		}
		reply := transport.ServerFrame{Type: "rpc", ID: request.ID, Error: &transport.FrameError{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}}
		if err := wsjson.Write(ctx, conn, reply); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	defer client.Close()

	err = client.Call(ctx, "AddedLater", nil)
	var refused *Error
	if !errors.As(err, &refused) {
		t.Fatalf("Call = %v, want an answered refusal", err)
	}
	if got := err.Error(); got != methodUnavailableSentence {
		t.Fatalf("Error() = %q, want %q", got, methodUnavailableSentence)
	}
	if refused.Code != transport.ErrCodeMethodNotFound || refused.Message != "method not registered" {
		t.Fatalf("refusal = %+v, want the wire code and text kept", refused)
	}
	if got := (&Error{Code: transport.ErrCodeMethodError, Message: "disk full"}).Error(); got != "disk full" {
		t.Fatalf("another refusal reads %q, want its own message", got)
	}
}

// TestFrontendSaysTheSameSentence: the frontend words the same refusal for a
// window that has no name for the computer. Two implementations, one text.
func TestFrontendSaysTheSameSentence(t *testing.T) {
	if MethodUnavailableMessage != methodUnavailableSentence {
		t.Fatalf("MethodUnavailableMessage = %q, want %q", MethodUnavailableMessage, methodUnavailableSentence)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "lib", "transport", "methodUnavailable.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "'"+methodUnavailableSentence+"'") {
		t.Fatalf("frontend/src/lib/transport/methodUnavailable.ts METHOD_UNAVAILABLE_SENTENCE is not %q", methodUnavailableSentence)
	}
}
