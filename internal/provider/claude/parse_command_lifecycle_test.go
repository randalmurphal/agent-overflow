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
		provider.CommandDiscarded,
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

// TestCommandResultCarriesActiveCommandUUID pins the correlation window: a
// synthetic assistant envelope between a command's `started` and `completed`
// lifecycle marks belongs to that command, so its EventCommandResult carries
// the command_uuid on Meta. Outside the window (or after a `cancelled`),
// results stay uncorrelated — Meta absent, never an error.
func TestCommandResultCarriesActiveCommandUUID(t *testing.T) {
	parser := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		events, err := parser.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		return events
	}
	synthetic := func(id, text string) string {
		return `{"type":"assistant","message":{"id":"` + id + `","model":"<synthetic>","role":"assistant",` +
			`"content":[{"type":"text","text":"` + text + `"}]}}`
	}
	resultMeta := func(events []provider.ProviderEvent) (provider.CommandResultMeta, bool) {
		t.Helper()
		if len(events) != 1 || events[0].Kind != provider.EventCommandResult {
			t.Fatalf("events = %+v, want one command result", events)
		}
		if events[0].Meta == nil {
			return provider.CommandResultMeta{}, false
		}
		var meta provider.CommandResultMeta
		if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
			t.Fatalf("unmarshal command result meta: %v", err)
		}
		return meta, true
	}

	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"queued"}`)
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"started"}`)
	meta, ok := resultMeta(parse(synthetic("m1", "Set effort level to xhigh (this session only)")))
	if !ok || meta.CommandUUID != "cmd-1" {
		t.Fatalf("in-window meta = %+v (present=%v), want CommandUUID cmd-1", meta, ok)
	}
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-1","state":"completed"}`)
	if meta, ok = resultMeta(parse(synthetic("m2", "orphan output"))); ok {
		t.Fatalf("post-completed meta = %+v, want uncorrelated", meta)
	}

	parse(`{"type":"command_lifecycle","command_uuid":"cmd-2","state":"started"}`)
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-2","state":"cancelled"}`)
	if meta, ok = resultMeta(parse(synthetic("m3", "late output"))); ok {
		t.Fatalf("post-cancelled meta = %+v, want uncorrelated", meta)
	}

	// `discarded` (2.1.224+: the session ended with the message still
	// queued) is terminal in exactly the same way — the started window it
	// may have opened must close, or the next synthetic output inherits a
	// uuid whose command can never answer.
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-3","state":"started"}`)
	parse(`{"type":"command_lifecycle","command_uuid":"cmd-3","state":"discarded"}`)
	if meta, ok = resultMeta(parse(synthetic("m4", "post-discard output"))); ok {
		t.Fatalf("post-discarded meta = %+v, want uncorrelated", meta)
	}
}
