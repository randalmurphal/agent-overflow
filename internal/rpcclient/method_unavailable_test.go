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
// the sentence a person can act on followed by what did not fit, whatever
// release that computer runs, and keeps its wire text. Who refused is the
// frame's dispatch mark, or an older release's exact text: a failure a
// method returned under the same code keeps its own message, detail or not.
func TestADispatcherRefusalReadsAsTheSentence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	detail := &transport.ErrorDetail{Ref: "r1", Method: "ReadThreadRemoteLog", At: 1}
	count := dispatchrefusal.CountRefusal("HarnessInfo", 0, 1, false, false)
	for _, tc := range []struct {
		name    string
		refusal *transport.FrameError
		want    string
		refused string
	}{
		{"this release, a missing method", &transport.FrameError{Code: transport.ErrCodeMethodNotFound, Message: dispatchrefusal.Unsupported, Dispatch: dispatchrefusal.UnsupportedRefusal()}, dispatchrefusal.Unsupported, "unsupported"},
		{"this release, the argument count", &transport.FrameError{Code: transport.ErrCodeBadParams, Message: dispatchrefusal.Unreadable, Dispatch: count}, dispatchrefusal.Unreadable + " (HarnessInfo: expected 0 arguments, got 1)", "unreadable"},
		{"v0.0.19, a missing method", &transport.FrameError{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}, dispatchrefusal.Unsupported, "unsupported"},
		{"v0.0.18, the argument count", &transport.FrameError{Code: transport.ErrCodeBadParams, Message: "main.App.GitPush: expected 1 params, got 2"}, dispatchrefusal.Unreadable + " (main.App.GitPush: expected 1 params, got 2)", "unreadable"},
		{"v0.0.17, an argument that did not decode", &transport.FrameError{Code: transport.ErrCodeBadParams, Message: "bad parameter 2", Detail: detail}, dispatchrefusal.Unreadable + " (bad parameter 2)", "unreadable"},
		{"v0.0.16, a method's failure forwarded under the same code", &transport.FrameError{Code: transport.ErrCodeMethodNotFound, Message: "Could not read the log on Nexus."}, "Could not read the log on Nexus.", ""},
		{"a method's own refusal of its arguments", &transport.FrameError{Code: transport.ErrCodeBadParams, Message: "Choose at least one project.", Detail: detail}, "Choose at least one project.", ""},
		{"any other refusal", &transport.FrameError{Code: transport.ErrCodeMethodError, Message: "disk full", Detail: detail}, "disk full", ""},
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
			kind := ""
			if r := refused.DispatchRefusal(); r != nil {
				kind = r.Kind
			}
			if kind != tc.refused || refused.Message != tc.refusal.Message || refused.Code != tc.refusal.Code {
				t.Fatalf("refusal = %+v kind %q, want the wire code and text kept and kind %q", refused, kind, tc.refused)
			}
			if refused.Unsupported() != (tc.refused == "unsupported") {
				t.Fatalf("Unsupported = %v", refused.Unsupported())
			}
		})
	}
}
