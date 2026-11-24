package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

// A retry under the ID of a start whose reply was lost reports that earlier
// attempt whatever refuses this one, the switch for agent commands and a
// computer that cannot be reached included: the original ID, that it may
// have run, and to read its status first, beside its own failure.
func TestRemoteAdmissionRefusedRetryReportsTheEarlierAttempt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	cases := []struct {
		name   string
		refuse func(t *testing.T, source *App, receiver *remoteAdmissionReceiver, ctx context.Context, input *AgentRemoteRequest)
		// says is what the retry's own failure says, when the row checks it.
		says string
	}{
		{"agent commands turned off", func(t *testing.T, source *App, _ *remoteAdmissionReceiver, ctx context.Context, input *AgentRemoteRequest) {
			if err := source.SetAgentComputerEnabled(ctx, input.ComputerID, false); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"a computer that cannot be reached", func(t *testing.T, _ *App, receiver *remoteAdmissionReceiver, _ context.Context, _ *AgentRemoteRequest) {
			stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := receiver.wire.srv.Shutdown(stop); err != nil {
				t.Fatalf("stop the destination: %v", err)
			}
		}, "nothing was sent"},
		{"a label this attempt cannot use", func(t *testing.T, _ *App, _ *remoteAdmissionReceiver, _ context.Context, input *AgentRemoteRequest) {
			input.Label = "two\nlines"
		}, ""},
		{"a timeout this attempt cannot use", func(t *testing.T, _ *App, _ *remoteAdmissionReceiver, _ context.Context, input *AgentRemoteRequest) {
			input.Request.TimeoutSeconds = -1
		}, ""},
		{"a conversation being stopped", func(t *testing.T, source *App, _ *remoteAdmissionReceiver, ctx context.Context, _ *AgentRemoteRequest) {
			scope, _ := transport.CallerScopeFrom(ctx)
			t.Cleanup(source.resting.close(scope.ThreadID))
		}, ""},
		{"a conversation reserved for a transfer", func(t *testing.T, source *App, _ *remoteAdmissionReceiver, ctx context.Context, _ *AgentRemoteRequest) {
			scope, _ := transport.CallerScopeFrom(ctx)
			digest := sha256.Sum256([]byte("activation"))
			if _, err := source.store.CreateThreadTransfer(store.ThreadTransfer{ID: uuid.NewString(), ThreadID: scope.ThreadID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing", ActivationHash: hex.EncodeToString(digest[:]), PrivateState: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			receiver.lostReply.Store(true)
			if _, err := source.AgentRemoteStart(ctx, input); err == nil {
				t.Fatal("a start whose reply was lost succeeded")
			}
			c.refuse(t, source, receiver, ctx, &input)
			_, err := source.AgentRemoteStart(ctx, input)
			_, message, _ := errorsx.PublicDetails(err)
			want := []string{input.Request.ID, "earlier attempt with this request_id may already have run", "Check remote_status with these same IDs"}
			if c.says != "" {
				want = append(want, c.says)
			}
			for _, said := range want {
				if !strings.Contains(message, said) {
					t.Fatalf("refused retry = %q (%v), want it to say %q", message, err, said)
				}
			}
			if receiver.calls.Load() != 1 {
				t.Fatalf("the destination saw %d starts, want the first alone", receiver.calls.Load())
			}
			if watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err != nil || watch.Notification != "pending" {
				t.Fatalf("the first attempt's watch = %#v %v, want it pending", watch, err)
			}
		})
	}
}

// A retry under the ID of a start the destination refused before accepting
// it stands for nothing that may have run, so its own refusal names no
// earlier attempt.
func TestRemoteAdmissionRetryOfARefusedStartReportsOnlyItsOwnRefusal(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	receiver.refusal = errorsx.Public("remote_capacity", "Destination capacity is full.", nil)
	if _, err := source.AgentRemoteStart(ctx, input); err == nil {
		t.Fatal("a start the destination refused succeeded")
	}
	if err := source.SetAgentComputerEnabled(ctx, input.ComputerID, false); err != nil {
		t.Fatal(err)
	}
	_, err := source.AgentRemoteStart(ctx, input)
	code, message, _ := errorsx.PublicDetails(err)
	if code != "remote_access_disabled" || strings.Contains(message, "earlier attempt") {
		t.Fatalf("retry of a refused start = %q %q, want only that agent commands are off", code, message)
	}
}
