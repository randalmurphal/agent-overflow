package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

// A retry under the ID of a start whose reply was lost reports that earlier
// attempt whatever refuses this one, the switch for agent commands
// included: the original ID, that it may have run, and to read its status
// first.
func TestRemoteAdmissionRefusedRetryReportsTheEarlierAttempt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	cases := []struct {
		name   string
		refuse func(t *testing.T, source *App, ctx context.Context, input *AgentRemoteRequest)
	}{
		{"agent commands turned off", func(t *testing.T, source *App, ctx context.Context, input *AgentRemoteRequest) {
			if err := source.SetAgentComputerEnabled(ctx, input.ComputerID, false); err != nil {
				t.Fatal(err)
			}
		}},
		{"a label this attempt cannot use", func(t *testing.T, _ *App, _ context.Context, input *AgentRemoteRequest) {
			input.Label = "two\nlines"
		}},
		{"a timeout this attempt cannot use", func(t *testing.T, _ *App, _ context.Context, input *AgentRemoteRequest) {
			input.Request.TimeoutSeconds = -1
		}},
		{"a conversation being stopped", func(t *testing.T, source *App, ctx context.Context, _ *AgentRemoteRequest) {
			scope, _ := transport.CallerScopeFrom(ctx)
			t.Cleanup(source.resting.close(scope.ThreadID))
		}},
		{"a conversation reserved for a transfer", func(t *testing.T, source *App, ctx context.Context, _ *AgentRemoteRequest) {
			scope, _ := transport.CallerScopeFrom(ctx)
			digest := sha256.Sum256([]byte("activation"))
			if _, err := source.store.CreateThreadTransfer(store.ThreadTransfer{ID: uuid.NewString(), ThreadID: scope.ThreadID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing", ActivationHash: hex.EncodeToString(digest[:]), PrivateState: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			source, receiver, ctx, input := remoteAdmissionFixture(t)
			receiver.lostReply.Store(true)
			if _, err := source.AgentRemoteStart(ctx, input); err == nil {
				t.Fatal("a start whose reply was lost succeeded")
			}
			c.refuse(t, source, ctx, &input)
			_, err := source.AgentRemoteStart(ctx, input)
			_, message, _ := errorsx.PublicDetails(err)
			for _, said := range []string{input.Request.ID, "earlier attempt with this request_id may already have run", "Check remote_status with these same IDs"} {
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
