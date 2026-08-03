package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func decodeCommandLifecycle(t *testing.T, evt provider.ProviderEvent) provider.CommandLifecycleMeta {
	t.Helper()
	if evt.Kind != provider.EventCommandLifecycle {
		t.Fatalf("kind = %q, want %q", evt.Kind, provider.EventCommandLifecycle)
	}
	var meta provider.CommandLifecycleMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("decode command lifecycle meta: %v", err)
	}
	return meta
}

func TestParseCommandLifecycle_EveryState(t *testing.T) {
	for _, state := range []provider.CommandLifecycleState{
		provider.CommandQueued,
		provider.CommandStarted,
		provider.CommandCompleted,
		provider.CommandCancelled,
	} {
		t.Run(string(state), func(t *testing.T) {
			parser := NewParser()
			events, err := parser.ParseLine(testThread, []byte(
				`{"type":"command_lifecycle","command_uuid":"3f6a1c9e-0000-4000-8000-000000000001","state":"`+string(state)+`"}`))
			if err != nil {
				t.Fatalf("command_lifecycle: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			meta := decodeCommandLifecycle(t, events[0])
			if meta.State != state {
				t.Fatalf("State = %q, want %q", meta.State, state)
			}
			if meta.CommandUUID != "3f6a1c9e-0000-4000-8000-000000000001" {
				t.Fatalf("CommandUUID = %q", meta.CommandUUID)
			}
			// The uuid also rides ItemID so a consumer that never decodes
			// Meta can still correlate.
			if events[0].ItemID != meta.CommandUUID {
				t.Fatalf("ItemID = %q, want the command uuid", events[0].ItemID)
			}
		})
	}
}

// An undocumented, version-dependent enum must not leak a value no
// consumer has a branch for.
func TestParseCommandLifecycle_UnknownStateDropped(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(
		`{"type":"command_lifecycle","command_uuid":"u1","state":"reticulating"}`))
	if err != nil {
		t.Fatalf("command_lifecycle: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0 for an unknown state", len(events))
	}
}

func TestParseCommandLifecycle_MissingUUIDDropped(t *testing.T) {
	parser := NewParser()
	events, err := parser.ParseLine(testThread, []byte(`{"type":"command_lifecycle","state":"queued"}`))
	if err != nil {
		t.Fatalf("command_lifecycle: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0 without a command_uuid", len(events))
	}
}
