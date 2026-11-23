package app

import (
	"context"
	"errors"
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
// the method or could not read the arguments, and either way it usually runs
// a different release. A person or the model reads that sentence, under a
// code of this computer's own, and is not told the call may have run. A
// computer that still sends the old wire text reads the same.
func TestRemoteErrorsWordTheDestinationsDispatcherRefusal(t *testing.T) {
	t.Parallel()
	const id = "2018ba7a-359c-4a0b-92a2-0b7cae76a515"
	for _, tc := range []struct {
		name    string
		refusal *rpcclient.Error
		want    string
	}{
		{"a method it does not have, old text", &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}, dispatchrefusal.Unsupported},
		{"a method it does not have", &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: dispatchrefusal.Unsupported}, dispatchrefusal.Unsupported},
		{"arguments it cannot read, old text", &rpcclient.Error{Code: transport.ErrCodeBadParams, Message: "App.RemoteCommandRun: expected 1 params, got 2"}, dispatchrefusal.Unreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, message, uncertain := remoteErrorDetails("run", tc.refusal)
			if code != "remote_unsupported" || message != tc.want || uncertain {
				t.Fatalf("details = (%q, %q, uncertain %v), want remote_unsupported, the sentence, certain", code, message, uncertain)
			}
			text := remoteErrorText(remoteOperationErrorFor("run", remoteRef{ComputerID: id, ComputerName: "Nexus", RequestID: id}, tc.refusal))
			want := "[remote_unsupported] Remote run on Nexus (computer " + id + ") for request " + id + ": " + tc.want
			if text != want {
				t.Fatalf("tool error = %q, want %q", text, want)
			}
		})
	}
	// A refusal a method there returned under the same code carries detail:
	// it is that method's own answer, kept as it is.
	forwarded := &rpcclient.Error{Code: transport.ErrCodeBadParams, Message: "Use a label of 80 characters or fewer.", Detailed: true}
	if code, message, uncertain := remoteErrorDetails("run", forwarded); code != transport.ErrCodeBadParams || message != forwarded.Message || uncertain {
		t.Fatalf("a method's own refusal = (%q, %q, uncertain %v), want it kept", code, message, uncertain)
	}
}

// The tray reads a remote job's log through this computer's
// ReadThreadRemoteLog. When the job's computer refused, the failure is this
// computer's method's answer: it names the job, keeps the destination's
// sentence, and reaches the page under a code and with detail the page does
// not take for a refusal by the computer it is connected to.
func TestAForwardedRefusalIsNotThisComputersOwn(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := remoteWatchThread(t, a, "codex")
	watch := registeredRemoteWatch(t, a, thread)
	refusal := &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}
	answer := transport.CallerError(a.remoteUserError("read the log of", watch.ComputerID, watch.RequestID, refusal))
	want := `Could not read the log of "build --cross-platform": ` + dispatchrefusal.Unsupported
	if answer.Code != "remote_unsupported" || !answer.Detailed || answer.Message != want {
		t.Fatalf("answer = (%q, detailed %v, %q), want (remote_unsupported, detailed, %q)", answer.Code, answer.Detailed, answer.Message, want)
	}
	if dispatchrefusal.Is(answer.Code, answer.Detailed) {
		t.Fatal("the forwarded refusal reads as the dispatcher's own")
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
