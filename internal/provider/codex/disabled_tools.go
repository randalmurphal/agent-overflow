package codex

import (
	"log"
	"sort"
	"strings"
)

// Codex has no flat disallow list. Every tool it can drop is dropped by a
// config key, and the keys are not one-per-tool — collab agents need three
// — so AO exposes a CURATED TOGGLE ID per user-meaningful capability and
// owns the id → keys table here.
//
// Every key below is settable per conversation through the `config` map on
// thread/start / thread/resume / thread/fork, and every one removes the
// tool's SCHEMA from the request rather than policing its use. Dotted keys
// are expanded into nested TOML server-side (config/src/overrides.rs), so
// they are emitted as flat dotted strings exactly as written. Verified
// against rust-v0.147.0 — see docs/references/codex-instructions-tools.md
// for the source citations and for the two toggles deliberately NOT
// offered (shell/unified-exec, which lobotomizes a session, and
// apply_patch, which is catalog-driven and only removable at startup).
const (
	ToggleWebSearch        = "web_search"
	ToggleUpdatePlan       = "update_plan"
	ToggleViewImage        = "view_image"
	ToggleRequestUserInput = "request_user_input"
	ToggleCollabAgents     = "collab_agents"
	ToggleImageGeneration  = "image_generation"
	ToggleToolSuggest      = "tool_suggest"
)

// disabledToolConfigKeys maps a toggle id to the config entries that turn
// the capability off. `web_search` is the odd one out: it is a mode enum
// rather than a bool, so it is disabled by value, not by `false`.
var disabledToolConfigKeys = map[string]map[string]any{
	ToggleWebSearch:  {"web_search": "disabled"},
	ToggleUpdatePlan: {"tools.update_plan.enabled": false},
	ToggleViewImage:  {"features.view_image": false},
	ToggleRequestUserInput: {
		"tools.experimental_request_user_input.enabled": false,
	},
	ToggleCollabAgents: {
		// agents.enabled gates the capability; the two feature flags are
		// the V2 and V1 implementations behind it. All three are needed —
		// clearing only the first leaves a build that still routes through
		// a legacy path exposing the tools.
		"agents.enabled":          false,
		"features.multi_agent_v2": false,
		"features.multi_agent":    false,
	},
	ToggleImageGeneration: {"features.image_generation": false},
	ToggleToolSuggest:     {"features.tool_suggest": false},
}

// DisabledToolToggleIDs returns every recognised toggle id, sorted. The
// settings layer does not enum-validate its list (it cannot import this
// package), so this is the one enumeration — for tests and for anything
// that needs to present the curated set.
func DisabledToolToggleIDs() []string {
	ids := make([]string, 0, len(disabledToolConfigKeys))
	for id := range disabledToolConfigKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// DisabledToolConfigOverrides folds a toggle-id list into the per-thread
// `config` entries that disable those tools.
//
// An id this build does not know is SKIPPED with a log line, never a
// failure: the list is user-editable settings data that outlives any one
// AO version, and refusing to start a session because a toggle was renamed
// upstream would be worse than starting one with that tool still exposed.
// Silence is not an option either — the user would see a toggle they set
// having no effect with nothing to explain it.
func DisabledToolConfigOverrides(toggles []string) map[string]any {
	if len(toggles) == 0 {
		return nil
	}
	overrides := map[string]any{}
	for _, raw := range toggles {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		keys, ok := disabledToolConfigKeys[id]
		if !ok {
			log.Printf("codex: unknown disabled-tool toggle %q — ignoring (known: %s)",
				id, strings.Join(DisabledToolToggleIDs(), ", "))
			continue
		}
		for key, value := range keys {
			overrides[key] = value
		}
	}
	if len(overrides) == 0 {
		return nil
	}
	return overrides
}
