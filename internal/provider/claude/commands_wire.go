package claude

import (
	"encoding/json"
	"strings"

	"agent-overflow/internal/provider"
)

// commands_wire.go — the three surfaces Claude reports provider-executed
// commands on. All three are documented in docs/references/claude-wire.md
// §"Slash commands" (verified 2.1.219, 2026-08-03 live probe).
//
//  1. `initialize` control_response `commands[]` — the RICH list
//     ({name, description, argumentHint}), read for free off the zero-token
//     account probe. Decoded here, cached by internal/claudecommands.
//  2. `system/init` `slash_commands[]` — names only, but the ONLY surface
//     that includes MCP prompt commands (`mcp__server__prompt`). Decoded on
//     provider.SessionInfo by parse_system.go.
//  3. `system/commands_changed` — a spontaneous push carrying the FULL rich
//     list again after mid-session skill discovery or a plugin reload. Its
//     contract is REPLACE.
//
// Names never carry the leading slash on any surface (the CLI's own zod
// schema: "Skill name (without the leading slash)").

// maxWireCommands bounds every list this file decodes. A real 2.1.219 install
// reports 52 entries; the cap exists so a malformed or hostile envelope cannot
// push an unbounded list into the per-probe cache and, from there, onto the
// websocket. Entries past the cap are dropped, not truncated mid-entry.
const maxWireCommands = 512

const (
	maxCommandNameRunes         = 128
	maxCommandDescriptionRunes  = 512
	maxCommandArgumentHintRunes = 128
)

// decodeWireCommands pulls the `commands` array out of an initialize
// control_response's inner payload.
//
// A missing array decodes to nil, which is a REAL answer (a CLI too old to
// report commands) and not an error — the same rule decodeWireModels follows,
// and for the same reason: a consumer must be able to tell "the binary reports
// none" from "we could not read what it reported".
func decodeWireCommands(payload json.RawMessage) ([]provider.SlashCommand, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var inner struct {
		Commands []provider.SlashCommand `json:"commands"`
	}
	if err := json.Unmarshal(payload, &inner); err != nil {
		return nil, err
	}
	return normalizeWireCommands(inner.Commands), nil
}

// normalizeWireCommands trims, bounds, and drops nameless entries, preserving
// wire order (the CLI orders its own picker). Duplicates are NOT collapsed: the
// wire is the authority on what it offers, and two entries sharing a name would
// be a CLI bug worth seeing rather than one this layer hides.
func normalizeWireCommands(in []provider.SlashCommand) []provider.SlashCommand {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.SlashCommand, 0, min(len(in), maxWireCommands))
	for _, cmd := range in {
		if len(out) >= maxWireCommands {
			break
		}
		name := strings.TrimSpace(cmd.Name)
		if name == "" {
			continue
		}
		out = append(out, provider.SlashCommand{
			Name:         boundRunes(name, maxCommandNameRunes),
			Description:  boundRunes(strings.TrimSpace(cmd.Description), maxCommandDescriptionRunes),
			ArgumentHint: boundRunes(strings.TrimSpace(cmd.ArgumentHint), maxCommandArgumentHintRunes),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeCommandNames applies the same bounds to a names-only list
// (`system/init.slash_commands`, `skills`). Empty and blank entries are
// dropped; order is preserved.
func normalizeCommandNames(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(in), maxWireCommands))
	for _, raw := range in {
		if len(out) >= maxWireCommands {
			break
		}
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		out = append(out, boundRunes(name, maxCommandNameRunes))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizePlugins bounds `system/init.plugins`. A nameless entry is dropped;
// path and source ride along as data.
func normalizePlugins(in []provider.PluginInfo) []provider.PluginInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.PluginInfo, 0, min(len(in), maxWireCommands))
	for _, plugin := range in {
		if len(out) >= maxWireCommands {
			break
		}
		name := strings.TrimSpace(plugin.Name)
		if name == "" {
			continue
		}
		out = append(out, provider.PluginInfo{
			Name:   boundRunes(name, maxCommandNameRunes),
			Path:   boundRunes(strings.TrimSpace(plugin.Path), maxCommandDescriptionRunes),
			Source: boundRunes(strings.TrimSpace(plugin.Source), maxCommandNameRunes),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeCommandsChanged reads the `commands` array off a
// `system/commands_changed` envelope. The second result reports whether the
// envelope carried a `commands` key at all: an envelope WITHOUT one says
// nothing and must be dropped, while `"commands": []` is a real replacement
// with an empty list. Collapsing the two would let a malformed push silently
// wipe a session's command list.
func decodeCommandsChanged(raw json.RawMessage) ([]provider.SlashCommand, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var commands []provider.SlashCommand
	if json.Unmarshal(raw, &commands) != nil {
		return nil, false
	}
	return normalizeWireCommands(commands), true
}

// boundRunes caps a rune length WITHOUT an ellipsis suffix. These values are
// identifiers and short hints rendered in a picker, not prose: a name that
// gained "..." would no longer be the thing the CLI answers to.
func boundRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if len(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}
