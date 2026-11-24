package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/dispatchrefusal"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/transport"
)

func TestRemoteErrorsPreserveSafeDetailsAndExplainUncertainAcceptance(t *testing.T) {
	t.Parallel()
	const id = "2018ba7a-359c-4a0b-92a2-0b7cae76a515"
	private := errors.New("secret /private/path?ticket=private-token")
	for _, tc := range []struct {
		name         string
		err          error
		want, absent string
	}{
		{"public", errorsx.Public("remote_capacity", "All slots are busy. Wait before retrying.", private), "All slots are busy", "private-token"},
		{"destination", &rpcclient.Error{Code: "remote_request_conflict", Message: "Use the original request."}, "Use the original request", "private-token"},
		{"timeout", context.DeadlineExceeded, "SAME request_id", "private-token"},
		{"internal", private, "SAME request_id", "private-token"},
		{"old destination", &rpcclient.Error{Code: transport.ErrCodeMethodError, Message: "method failed (id: trace)"}, "trace", "private-token"},
		{"permission", &rpcclient.Error{Code: transport.ErrCodeScopeRequired, Message: "scope missing"}, "full device access", "SAME request_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := remoteOperationErrorFor("run", remoteRef{ComputerID: id, RequestID: id}, tc.err)
			text := remoteErrorText(err)
			if !strings.Contains(text, tc.want) || strings.Contains(text, tc.absent) || !strings.Contains(text, id) {
				t.Fatalf("unexpected public text: %s", text)
			}
			if !errors.Is(err, tc.err) {
				t.Fatal("lost underlying cause")
			}
		})
	}
	text := remoteErrorText(remoteOperationErrorFor("cancel", remoteRef{ComputerID: id, RequestID: id}, context.DeadlineExceeded))
	if !strings.Contains(text, "Cancellation is not confirmed") {
		t.Fatal(text)
	}
}

// A destination's dispatcher that refused the call did not run it: it lacks
// the method or could not read the arguments, and either way it often runs
// a different release. A person or the model reads which, under a code of
// this computer's own, and is not told the call may have run. The kind
// survives forwarding, whatever release the destination runs, and so does
// what did not fit, in the wrapped cause the frame's detail lists.
func TestRemoteErrorsWordTheDestinationsDispatcherRefusal(t *testing.T) {
	t.Parallel()
	const id = "2018ba7a-359c-4a0b-92a2-0b7cae76a515"
	for _, tc := range []struct {
		name    string
		refusal *rpcclient.Error
		want    string
		inChain string
	}{
		{"a method it does not have", &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: dispatchrefusal.Unsupported, Dispatch: dispatchrefusal.UnsupportedRefusal()},
			dispatchrefusal.ForwardedUnsupported, dispatchrefusal.Unsupported},
		{"v0.0.19, a method it does not have", &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"},
			dispatchrefusal.ForwardedUnsupported, dispatchrefusal.Unsupported},
		{"arguments it cannot read", &rpcclient.Error{Code: transport.ErrCodeBadParams, Message: dispatchrefusal.Unreadable, Dispatch: dispatchrefusal.CountRefusal("RemoteCommandRun", 1, 2, false, false)},
			dispatchrefusal.ForwardedUnreadable, "RemoteCommandRun: expected 1 arguments, got 2"},
		{"v0.0.18, arguments it cannot read", &rpcclient.Error{Code: transport.ErrCodeBadParams, Message: "main.App.RemoteCommandRun: expected 1 params, got 2"},
			dispatchrefusal.ForwardedUnreadable, "main.App.RemoteCommandRun: expected 1 params, got 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, message, uncertain := remoteErrorDetails("run", tc.refusal)
			if code != "remote_unsupported" || message != tc.want || uncertain {
				t.Fatalf("details = (%q, %q, uncertain %v), want remote_unsupported, %q, certain", code, message, uncertain, tc.want)
			}
			err := remoteOperationErrorFor("run", remoteRef{ComputerID: id, ComputerName: "Nexus", RequestID: id}, tc.refusal)
			want := "[remote_unsupported] Remote run on Nexus (computer " + id + ") for request " + id + ": " + tc.want
			if text := remoteErrorText(err); text != want {
				t.Fatalf("tool error = %q, want %q", text, want)
			}
			if chain := strings.Join(errorsx.Chain(err), "\n"); !strings.Contains(chain, tc.inChain) {
				t.Fatalf("chain = %q, want it to keep %q", chain, tc.inChain)
			}
		})
	}
	// A refusal a method there returned under the same code is that
	// method's own answer, kept as it is.
	own := &rpcclient.Error{Code: transport.ErrCodeBadParams, Message: "Choose at least one project for the assistant, or all projects."}
	if code, message, uncertain := remoteErrorDetails("run", own); code != transport.ErrCodeBadParams || message != own.Message || uncertain {
		t.Fatalf("a method's own refusal = (%q, %q, uncertain %v), want it kept", code, message, uncertain)
	}
}

// The tray reads a remote job's log through this computer's
// ReadThreadRemoteLog. When the job's computer refused, the failure is this
// computer's method's answer: it names the job, says which refusal, and
// reaches the page under a code of its own and without the dispatch mark,
// so the page never takes it for a refusal by the computer it is connected
// to. These are the frames the page receives, which
// frontend/src/test/fixtures/forwardedRefusalFrames.json holds for the
// tray's test (RemoteJobTrayRow.test.ts).
func TestAForwardedRefusalIsNotThisComputersOwn(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := remoteWatchThread(t, a, "codex")
	watch := registeredRemoteWatch(t, a, thread)
	type wireDetail struct {
		Method string   `json:"method"`
		Chain  []string `json:"chain"`
	}
	type wireFrame struct {
		Name     string                   `json:"name"`
		Code     string                   `json:"code"`
		Message  string                   `json:"message"`
		Detail   *wireDetail              `json:"detail"`
		Dispatch *dispatchrefusal.Refusal `json:"dispatch,omitempty"`
	}
	var frames []wireFrame
	for _, tc := range []struct {
		name    string
		refusal *rpcclient.Error
		want    string
	}{
		{"unsupported", &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: dispatchrefusal.Unsupported, Dispatch: dispatchrefusal.UnsupportedRefusal()}, dispatchrefusal.ForwardedUnsupported},
		{"unsupported, v0.0.19", &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}, dispatchrefusal.ForwardedUnsupported},
		{"unreadable", &rpcclient.Error{Code: transport.ErrCodeBadParams, Message: dispatchrefusal.Unreadable, Dispatch: dispatchrefusal.CountRefusal("RemoteCommandReadLog", 3, 4, false, false)}, dispatchrefusal.ForwardedUnreadable},
	} {
		d := transport.NewDispatcher()
		receiver := &forwardingReceiver{err: a.remoteUserError("read the log of", watch.ComputerID, watch.RequestID, tc.refusal)}
		if _, err := d.Register(receiver, transport.RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
			t.Fatal(err)
		}
		m, fe := d.ResolveForOrigin(0, "ReadThreadRemoteLog", true)
		if fe != nil {
			t.Fatal(fe)
		}
		_, fe = d.Invoke(context.Background(), m, nil)
		want := `Could not read the log of "build --cross-platform": ` + tc.want
		if fe == nil || fe.Code != "remote_unsupported" || fe.Message != want || fe.Dispatch != nil || fe.Detail == nil {
			t.Fatalf("%s: frame = %+v, want remote_unsupported, %q, detail, and no dispatch mark", tc.name, fe, want)
		}
		frames = append(frames, wireFrame{Name: tc.name, Code: fe.Code, Message: fe.Message, Detail: &wireDetail{Method: fe.Detail.Method, Chain: fe.Detail.Chain}})
	}
	got, err := json.MarshalIndent(frames, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("frontend", "src", "test", "fixtures", "forwardedRefusalFrames.json")
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(held) != string(got)+"\n" {
		t.Fatalf("%s is not the frames a forwarder sends; it should read:\n%s", path, got)
	}
}

// forwardingReceiver is a method that fails with err, as this computer's
// ReadThreadRemoteLog does when the job's computer refused it.
type forwardingReceiver struct{ err error }

func (r *forwardingReceiver) ReadThreadRemoteLog() error { return r.err }

// A job's label goes first in a person's error, and a page shows only the
// first 256 characters of it (frontend/src/lib/transport/frames.ts): the
// label is shortened so what to do is never cut.
func TestARemoteErrorKeepsItsRemedyWithinWhatAPageShows(t *testing.T) {
	t.Parallel()
	frames, err := os.ReadFile(filepath.Join("frontend", "src", "lib", "transport", "frames.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(frames), fmt.Sprintf("const LOG_STRING_MAX = %d;", pageMessageLimit)) {
		t.Fatalf("frames.ts no longer clamps at %d", pageMessageLimit)
	}
	for _, label := range []string{strings.Repeat("x", 300), strings.Repeat("\U0001F600", 120), strings.Repeat(`"`, 120), "build"} {
		message := remoteUserMessage("read the log of", label, strings.Repeat("n", 80), dispatchrefusal.ForwardedUnreadable)
		if utf16Len(message) > pageMessageLimit || !strings.HasSuffix(message, ": "+dispatchrefusal.ForwardedUnreadable) {
			t.Fatalf("message of %d code units = %q, want at most %d ending with the remedy", utf16Len(message), message, pageMessageLimit)
		}
	}
	if got := remoteUserMessage("stop", "build", "Nexus", "Timed out."); got != `Could not stop "build" on Nexus: Timed out.` {
		t.Fatalf("short label = %q", got)
	}
}

// A person reads a computer and a job by name; the model retries by id. The
// tool error carries both, and the text a row shows carries neither.
func TestRemoteErrorsNameTheComputerAndTheJobForPeopleAndKeepIDsForTheModel(t *testing.T) {
	t.Parallel()
	const computer = "2018ba7a-359c-4a0b-92a2-0b7cae76a515"
	const request = "98312d67-2222-4222-8222-222222222222"
	notPaired := errorsx.Public("remote_not_paired", "This computer is no longer paired. Reconnect it in Remote access before retrying.", nil)
	ref := remoteRef{ComputerID: computer, ComputerName: "Nexus", RequestID: request, Label: "Go tests"}
	text := remoteErrorText(remoteOperationErrorFor("status", ref, notPaired))
	for _, want := range []string{`Remote status on Nexus (computer ` + computer + `) for "Go tests" (request ` + request + `): This computer is no longer paired.`, "Keep these IDs"} {
		if !strings.Contains(text, want) {
			t.Fatalf("tool error %q lacks %q", text, want)
		}
	}
	replaced := errorsx.Public("remote_pairing_replaced", "This computer was paired again while the call was on its way. Retry it.", nil)
	if text := remoteErrorText(remoteOperationErrorFor("status", ref, replaced)); !strings.Contains(text, "Keep these IDs") {
		t.Fatalf("tool error for a replaced pairing %q does not keep the ids", text)
	}
	unnamed := remoteErrorText(remoteOperationErrorFor("cancel", remoteRef{ComputerID: computer, RequestID: request}, notPaired))
	if !strings.HasPrefix(unnamed, "[remote_not_paired] Remote cancel on computer "+computer+" for request "+request+": ") {
		t.Fatalf("unnamed tool error: %s", unnamed)
	}
	issue := remoteIssueText("status", notPaired)
	if issue != "This computer is no longer paired. Reconnect it in Remote access before retrying." {
		t.Fatalf("row issue text: %s", issue)
	}
	if strings.Contains(issue, computer) || strings.Contains(issue, request) || strings.Contains(issue, "[remote_not_paired]") {
		t.Fatalf("row issue text carries ids or a code: %s", issue)
	}
	// An error already wrapped for the tool is not re-prefixed.
	wrapped := remoteOperationErrorFor("status", ref, notPaired)
	if again := remoteOperationErrorFor("cancel", ref, wrapped); again != wrapped {
		t.Fatalf("re-wrapped: %v", again)
	}
}

func TestRemoteUserErrorSpeaksForTheJobThePersonActedOn(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := remoteWatchThread(t, a, "codex")
	watch := registeredRemoteWatch(t, a, thread)
	err := a.remoteUserError("stop", watch.ComputerID, watch.RequestID, context.DeadlineExceeded)
	code, message, ok := errorsx.PublicDetails(err)
	if !ok || code != "remote_request_timeout" || !strings.HasPrefix(message, `Could not stop "build --cross-platform": Timed out waiting for the destination.`) {
		t.Fatalf("user error: %v", err)
	}
	if strings.Contains(message, watch.ComputerID) || strings.Contains(message, watch.RequestID) {
		t.Fatalf("user error carries ids: %s", message)
	}
	if a.remoteUserError("stop", watch.ComputerID, watch.RequestID, nil) != nil {
		t.Fatal("nil error wrapped")
	}
}
