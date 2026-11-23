package app

import (
	"context"
	"errors"
	"strings"
	"testing"

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

// A destination without the method runs a different release and did not run
// the call: a person or the model reads that, with what to do, and is not
// told the call may have run.
func TestRemoteErrorsSayAMissingMethodIsAVersionMismatch(t *testing.T) {
	t.Parallel()
	const id = "2018ba7a-359c-4a0b-92a2-0b7cae76a515"
	missing := &rpcclient.Error{Code: transport.ErrCodeMethodNotFound, Message: "method not registered"}
	code, message, uncertain := remoteErrorDetails("run", missing)
	if code != transport.ErrCodeMethodNotFound || message != rpcclient.MethodUnavailableMessage || uncertain {
		t.Fatalf("details = (%q, %q, uncertain %v), want the version mismatch, certain", code, message, uncertain)
	}
	text := remoteErrorText(remoteOperationErrorFor("run", remoteRef{ComputerID: id, ComputerName: "Nexus", RequestID: id}, missing))
	want := "[method_not_found] Remote run on Nexus (computer " + id + ") for request " + id + ": " + rpcclient.MethodUnavailableMessage
	if text != want {
		t.Fatalf("tool error = %q, want %q", text, want)
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
