package triage

import (
	"encoding/json"
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/slicesx"
)

// provider_commands.go — the live per-thread projection of which slash
// commands the provider CLI will execute itself.
//
// Same posture as fast_mode.go, and for the same reasons: this is session
// state, never history (root CLAUDE.md principle 2). The CLI restates the whole
// set on every `system/init` and re-pushes it wholesale on
// `system/commands_changed`, so the newest frame is the whole answer, nothing
// is persisted, and a frontend that missed frames re-learns at the next session
// boundary.
//
// Emitted ONLY when the wire carried a list. Absence is silence: a CLI too old
// to report commands, or a provider with no command concept, must not produce
// an empty frame that a menu would render as "this session has none".
//
// Triage deliberately does NOT overlay the richer descriptions the account
// probe's `initialize` response carries (internal/claudecommands). Two reasons:
// the probe answers for a probe identity, not for this thread's session, and
// triage may not reach into a provider-specific cache. It emits what each wire
// frame actually said — names from `system/init`, full entries from
// `commands_changed` — and the binding layer that has both does the overlay.

// ProviderCommandsEvent is the payload of `provider:commands`.
//
// Replace is always true today and is on the wire anyway because it is the
// contract, not an implementation detail: both producers restate the whole set,
// and a consumer must never merge one frame into another. A future producer
// that can only report a delta has to say so here rather than silently
// changing what an existing frame means.
type ProviderCommandsEvent struct {
	ThreadID string                  `json:"threadId"`
	Provider string                  `json:"provider"`
	Replace  bool                    `json:"replace"`
	Commands []provider.SlashCommand `json:"commands"`
}

// emitSessionSlashCommands forwards the names `system/init` carried. No-op on
// an empty list — see the absence rule above.
func (r *Router) emitSessionSlashCommands(threadID string, names []string) {
	if r == nil || threadID == "" || len(names) == 0 {
		return
	}
	commands := make([]provider.SlashCommand, 0, len(names))
	for _, name := range names {
		commands = append(commands, provider.SlashCommand{Name: name})
	}
	r.emitProviderCommands(threadID, commands)
}

// handleCommandsChanged routes Claude's `system/commands_changed` push.
//
// Unlike the init path, an EMPTY list here IS emitted: the parser only produces
// this event when the envelope actually carried a `commands` key, so an empty
// payload is the CLI saying "nothing is available now" — a real replacement a
// live menu has to apply.
func (r *Router) handleCommandsChanged(evt provider.ProviderEvent) error {
	var meta provider.CommandsChangedMeta
	if len(evt.Meta) == 0 || json.Unmarshal(evt.Meta, &meta) != nil {
		// The parser marshals this shape itself, so a failure here means the
		// event was synthesized by something that got the contract wrong.
		// Loud, and not a routing error that would fail the read loop.
		log.Printf("triage: commands_changed on %s carried no readable command list; dropping", evt.ThreadID)
		return nil
	}
	r.emitProviderCommands(evt.ThreadID, meta.Commands)
	return nil
}

func (r *Router) emitProviderCommands(threadID string, commands []provider.SlashCommand) {
	if r == nil || threadID == "" {
		return
	}
	// The provider name is read from the thread row rather than hardcoded:
	// both wire producers are Claude envelopes today, but claude-tui replays
	// the same shapes under its own provider name, and a frontend that keys a
	// command menu by provider must not be told the wrong one. A missing
	// thread row leaves it empty — the thread id already identifies the
	// target, so an unknown provider is not worth dropping the frame over.
	attribution, err := r.store.GetThreadContextSettings(threadID)
	if err != nil {
		log.Printf("triage: provider commands attribution for %s: %v", threadID, err)
	}
	r.emit(eventchan.ProviderCommands, ProviderCommandsEvent{
		ThreadID: threadID,
		Provider: attribution.Provider,
		Replace:  true,
		// Explicit empty slice so the wire carries [] rather than null and a
		// replace frame stays type-honest for a non-nullable frontend field.
		Commands: slicesx.OrEmpty(commands),
	})
}
