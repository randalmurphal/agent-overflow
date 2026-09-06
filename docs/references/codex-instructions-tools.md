# Codex base instructions + tool exposure (verified against rust-v0.147.0)

Source-verified in the codex repo (`/home/rmurphy/repos/codex`); file:line
refs are into `codex-rs/`. Companion to [codex.md](codex.md). Motivation:
AO's system-prompt override + tool-toggle feature
(see [docs/specs/prompt-tool-overrides.md](../specs/prompt-tool-overrides.md)).

## Base instructions

### The API override is full replacement

`baseInstructions: string | null` exists on `thread/start`
(`app-server-protocol/src/protocol/v2/thread.rs:99`), `thread/resume`
(`:383`), and `thread/fork` (`:571`), but NOT on `turn/start` or
`thread/settings/update`. Not `#[experimental]`; no opt-in needed.
A sibling `developerInstructions` rides the same three methods.

The text becomes the Responses API `instructions` field **verbatim, with
nothing appended** (`core/src/client.rs:888`; for `use_responses_lite`
models it becomes the first `developer`-role message, `client.rs:873-886`).

Resolution priority (`core/src/session/mod.rs:633-657`):

1. `ConfigOverrides.base_instructions` (the API param / config override)
2. rollout `session_meta.base_instructions` (what the thread was started with)
3. `model_info.get_model_instructions(personality)`: the catalog template

⚠ (2) means a cold `thread/resume` **without** the param inherits whatever
the thread was first started with. Send the override on start *and* resume
to keep it deterministic.

⚠ Overrides (config map, baseInstructions, permissions…) are **silently
dropped** when resuming a thread still resident in the app-server process
(`request_processors/thread_processor.rs:39-153`, `:3546-3588`). AO's
one-app-server-per-session model dodges this; a shared app-server would not.

### What the default actually is

The live default is the **model-catalog `instructions_template`**, per
slug, `{{ personality }}`-substituted (`protocol/src/openai_models.rs:485-503`).
Bundled catalog (`models-manager/models.json`): gpt-5.6-sol/terra/luna
share one ~17.7k-char template; gpt-5.5, gpt-5.4, gpt-5.4-mini, gpt-5.2
each differ. The remote catalog can replace these at runtime. The repo
file `protocol/src/prompts/base_instructions/default.md` is a fallback
only (tests, fallback-metadata models).

There is **no supported way to read the effective default over the wire**
(`model/list` carries no instructions; `prompt_debug.rs` has no RPC).
Capture it from a request sink or read `models.json` for the pinned CLI.

Config-file equivalents (lower precedence than the API param,
`core/src/config/mod.rs:3904-3906`): `model_instructions_file` (abs path,
`config/src/config_toml.rs:236`) then inline `instructions`
(`config_toml.rs:214`). `experimental_instructions_file` no longer exists
in 0.147.

### What replacement does and does not break

Three separate channels, and replacing base instructions touches only the first:

- **Base instructions** → `instructions` field. Replaced wholesale.
- **AGENTS.md** ("user instructions") → separate user-role fragment
  (`core/src/context/world_state/agents_md.rs:26-31`); still delivered.
  But the default prompt's `# AGENTS.md spec` (the *interpretation
  contract*) is part of what you replaced. Carry your own if you care.
- **World-state context** (permissions block, environment context,
  collaboration mode, personality, model-switch) → independent
  developer/user fragments (`core/src/session/world_state.rs:30-230`),
  each with its own `include_*` gate. Notably `include_environment_context`
  (default true, `config_toml.rs:230`) keeps cwd/env context flowing.
  A Codex replacement prompt does NOT need to re-template environment
  facts the way a Claude one does.

Also lost with the stock prompt: `apply_patch` usage guidance and the
`update_plan` protocol text (the tool *schemas* still ship), and the
shell guidance. Safe: personality stays consistent (the override also
overwrites the models-manager template, `models-manager/src/model_info.rs:52-63`),
mid-session model switch re-injects *your* override not the stock prompt
(`context/world_state/model.rs:50-59`), and compaction reuses the
session's base instructions (`core/src/compact.rs:274`).

## Tool exposure

Tool specs are assembled in `core/src/tools/spec_plan.rs`. Everything
below **removes the schema from the request** (not just polices use),
and every key is settable **per-conversation** via the `config` map on
`thread/start`/`resume`/`fork`. Dotted keys expand into nested TOML
(`config/src/overrides.rs:9-30`; request overrides merge into the CLI
override layer, `app-server/src/config_manager.rs:236-244`). AO already
uses that map for `mcp_servers` / `model_reasoning_effort`.

| Tool(s) | Config key | Default |
|---|---|---|
| `update_plan` | `tools.update_plan.enabled = false` | true |
| `request_user_input` | `tools.experimental_request_user_input.enabled = false` | true |
| hosted `web_search` | `web_search = "disabled"` (`disabled\|cached\|indexed\|live`) | `cached` |
| `view_image` | `features.view_image = false` | true |
| all shell tools | `features.shell_tool = false` | true |
| unified exec → classic `shell` | `features.unified_exec = false` | true (non-Windows) |
| collab / multi-agent | `agents.enabled = false` (+ `features.multi_agent_v2` off) | true |
| V1 collab only | `features.multi_agent = false` | true |
| `image_generation` | `features.image_generation = false` | true |
| plugin-suggest | `features.tool_suggest = false` | true |
| MCP tools + resource tools | `mcp_servers = {}` (resource tools follow) | - |
| `apply_patch` | **no key**: catalog `apply_patch_tool_type`; only removable via startup-only `model_catalog_json` | on |

Instruction-*block* toggles (strip injected context, not tool schemas):
`include_permissions_instructions`, `include_apps_instructions`,
`include_collaboration_mode_instructions`, `include_environment_context`
(`config_toml.rs:221-230`), `skills.include_instructions`
(`config/src/skills_config.rs:31`), `project_doc_max_bytes` (the
AGENTS.md lever, `config_toml.rs:287`).

Process-only exceptions: `model_catalog_json` (per-thread no-op by its own
doc) and `-c` spawn flags. Ignore the `experimentalFeature/enablement/set`
RPC. It is process-wide and lower precedence than the config map.

## Version notes

- `baseInstructions`/`developerInstructions` present since ~0.130.0,
  unchanged through rust-v0.148.0-alpha.20. AO's floor (0.143) is above.
- The tool/feature keys above are byte-identical 0.147.0 → 0.148.0-alpha.20,
  and the relevant feature stages are all `Stable`, documented as "kept
  for ad-hoc enabling/disabling", so disabling is supported, not a hack.
- Enterprise/managed installs can veto some of these (`web_search_mode`
  is `Constrained`; `feature_requirements` entries become protected
  keys). An override can be refused on a managed machine.

### App-owned appended guidance

The current source in `app-server-protocol/src/protocol/v2/thread.rs` exposes
`developer_instructions` on start/resume/fork; `v2/config.rs` exposes it in
config/read. `core/src/session/mod.rs` resolves developer instructions from
config on cold start/resume, whereas base instructions explicitly consult
conversation history. AO therefore reads cwd-scoped config before appending
its optional remote-command guide, and omits the override when that guide is
absent. Native developer instructions remain first and unchanged.
