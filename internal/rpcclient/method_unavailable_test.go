package rpcclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/dispatchrefusal"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// answerOnce serves one call with refusal and returns a client dialed to it.
func answerOnce(t *testing.T, ctx context.Context, refusal *transport.FrameError) *Client {
	t.Helper()
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
		if err := wsjson.Write(ctx, conn, transport.ServerFrame{Type: "rpc", ID: request.ID, Error: refusal}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	t.Cleanup(func() { client.Close() })
	return client
}

// A computer's dispatcher refusal reads, wherever its error is shown, as
// the sentence a person can act on, whatever text that computer's release
// sent, and keeps its wire text. A failure a method there returned under
// the same code, which carries detail, keeps its own message: it is about
// whatever that method called, not about the computer this reached.
func TestADispatcherRefusalReadsAsTheSentence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	detail := &transport.ErrorDetail{Ref: "r1", Method: "ReadThreadRemoteLog", At: 1}
	for _, tc := range []struct {
		name      string
		refusal   *transport.FrameError
		want      string
		refused   bool
		wireLater string
	}{
		{"a release that sent the old text for a missing method", &transport.FrameError{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}, dispatchrefusal.Unsupported, true, "method not registered"},
		{"a release that sent the old text for arguments it could not read", &transport.FrameError{Code: transport.ErrCodeBadParams, Message: "main.App.GitPush: expected 1 params, got 2"}, dispatchrefusal.Unreadable, true, "main.App.GitPush: expected 1 params, got 2"},
		{"a method's failure forwarded under the same code", &transport.FrameError{Code: transport.ErrCodeMethodNotFound, Message: "Could not read the log on Nexus.", Detail: detail}, "Could not read the log on Nexus.", false, "Could not read the log on Nexus."},
		{"a method's own refusal of its arguments", &transport.FrameError{Code: transport.ErrCodeBadParams, Message: "Choose at least one project.", Detail: detail}, "Choose at least one project.", false, "Choose at least one project."},
		{"any other refusal", &transport.FrameError{Code: transport.ErrCodeMethodError, Message: "disk full", Detail: detail}, "disk full", false, "disk full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := answerOnce(t, ctx, tc.refusal).Call(ctx, "Anything", nil)
			var refused *Error
			if !errors.As(err, &refused) {
				t.Fatalf("Call = %v, want an answered refusal", err)
			}
			if got := err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
			if refused.DispatchRefusal() != tc.refused || refused.Message != tc.wireLater || refused.Code != tc.refusal.Code {
				t.Fatalf("refusal = %+v dispatch %v, want the wire code and text kept and dispatch %v", refused, refused.DispatchRefusal(), tc.refused)
			}
		})
	}
}
