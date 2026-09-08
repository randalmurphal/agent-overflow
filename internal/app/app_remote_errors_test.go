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
			err := remoteOperationError("run", id, id, tc.err)
			text := remoteErrorText(err)
			if !strings.Contains(text, tc.want) || strings.Contains(text, tc.absent) || !strings.Contains(text, id) {
				t.Fatalf("unexpected public text: %s", text)
			}
			if !errors.Is(err, tc.err) {
				t.Fatal("lost underlying cause")
			}
		})
	}
	text := remoteErrorText(remoteOperationError("cancel", id, id, context.DeadlineExceeded))
	if !strings.Contains(text, "Cancellation is not confirmed") {
		t.Fatal(text)
	}
}
