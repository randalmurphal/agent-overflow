# Antigravity (`agy`) ACP Wire Reference

Antigravity CLI (`agy`) speaks Agent Client Protocol (ACP) v1 over newline-delimited JSON-RPC 2.0 on stdio (`agy acp`).

## Process lifecycle and sharing

- Launch: `agy acp` (single shared process per binary + environment + binary identity + login home). Do not pass `--process-per-session` or `--dangerously-skip-permissions`.
- Environment (`agy.Home.Launch` builds every agy launch that reads the login: sessions, account and catalog probes, imports, sign-ins, `agy mcp list` and text generation):
  - Pin `AGY_CLI_DISABLE_AUTO_UPDATE=1` so background self-updates do not replace the running binary mid-session.
  - Scrub ambient `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GOOGLE_CLOUD_PROJECT`, and `GOOGLE_APPLICATION_CREDENTIALS` unless explicitly configured by the user.
  - Login store and home: a launcher decides its `agy.Home` as it starts, with its own context and inside its own time limit (`agy.HomeSource`). A process for the canonical login runs on the store the account manager reads (`provideraccountapp.Manager.AgyCanonicalHome`): `AGY_ACP_FORCE_FILE_STORAGE=1` and `SSH_CONNECTION` for the file, the flag cleared for the macOS Keychain item (a custom `SSH_CONNECTION` never reaches agy; an inherited one is left alone on the Keychain). The store is the one the reader reads the login from: the Keychain item when there is one, else a fallback file the reader accepts; a fallback file it refuses fails the launch. Its home is agy's default under `HOME` when that holds the manager's directory, and otherwise (an isolated boot that keeps the person's `HOME`) that directory passed as `--gemini-dir`/`--app-data-dir`, which only the file store may do. A home directory is always absolute; a relative one, as under a relative `HOME`, is refused. Only `agy acp` is known to take those flags, so `agy mcp list` and `agy -p` refuse a home that names its directory. An inherited `GEMINI_HOME` and `AGY_APP_DATA_DIR` are always cleared. A sign-in, a probe of a temporary home, and an import from another directory pass `--gemini-dir`/`--app-data-dir` on file storage. These variables and `HOME` are reserved in the custom environment.
  - A pooled server's key includes the home it started on, so a session start, a catalog probe or an import never reuses a server on another store or directory. Sessions already on a server whose store was left behind stay on it until they detach.
  - `agy --version` and `agy update` are not routed through an `agy.Home`; nothing in the repo shows either reads the login.
  - Tests isolate credentials with `AGY_ACP_FORCE_FILE_STORAGE=1` and a detached `HOME` (unsetting `GEMINI_HOME` and `AGY_APP_DATA_DIR`).
- Idle connections are kept warm in `sessionruntime.ConnPool` for 5 minutes and drained on binary upgrade or shutdown.

## Session open and live reconfiguration

- `initialize`: negotiates `protocolVersion: 1`, `agentInfo.version`, `agentCapabilities`, and `authMethods`.
- `session/new` and `session/resume`:
  - Pass `cwd`, `mcpServers` (HTTP entries with `type: "http"`, `url`, and `headers: [{name, value}]`), and `_meta.antigravity.systemPrompt.append` for appended instructions.
  - Immediately apply desired `model`, `thought_level`, and mode after open (`session/set_config_option` and `session/set_mode`) because cold resumes reset session options to defaults.
- Live updates (`acp.PlanLiveUpdate` + `Session.ApplyLiveUpdate`):
  - `model` -> `session/set_config_option` (`configId: "model"`).
  - `reasoningEffort` -> `session/set_config_option` (`configId: "thought_level"`).
  - `interactionMode` / `runtimeMode` -> `session/set_mode` (`plan`, `auto`, `ask`).
- Prompt execution:
  - `session/prompt` stays open until the entire turn finishes (`stopReason`: `end_turn`, `cancelled`, `max_tokens`, `refusal`).
  - `acp.Session.Send` writes `session/prompt` synchronously and awaits its RPC result on a tracked goroutine so callers can send `session/cancel` or permission responses while a turn runs.

## Streaming, tools, approvals, and usage

- `session/update` notifications:
  - `agent_message_chunk` and `agent_thought_chunk`: coalesced into `EventTextDelta` and `EventThinking`.
  - `tool_call` / `tool_call_update`: normalized by `agy.Flavor` (`read_file`, `edit_file`, `write_file`, `run_command`, `invoke_subagent`, `ask_question`).
  - `write_file` / `edit_file` with `ArtifactMetadata.RequestFeedback == true` emits `EventProposedPlan`.
  - `usage_update` carries `used` and `size` (context window); prompt `result.usage` carries Gemini token totals (`inputTokens`, `outputTokens`, `cachedReadTokens`, `thoughtTokens`).
  - `available_commands_update` maps to `EventCommandsUpdated`.
- `session/request_permission`:
  - Auto-decided under `auto`/`full-access`, denied under `read-only` for non-read tools, or surfaced to `EventApprovalRequest` (carrying `option._meta["agy.security.warning"]` when present).
- Silent turn errors:
  - Antigravity can return `stopReason: "end_turn"` after recording an error step (`step_type = 17`, `CORTEX_STEP_TYPE_ERROR_MESSAGE`) in `<appDataDir>/conversations/<sessionId>.db`.
  - `agy.Flavor.BeforePrompt` / `AfterPrompt` snapshots the max error step index via a read-only SQLite connection and classifies new error steps (including `QUOTA_EXHAUSTED` / 429 as `FailureReasonUsageLimit`).

## Catalog discovery, import, fork, and updates

- Live model catalog (`agy.ProbeCatalogOnConn`): opens one temporary session, cycles `session/set_config_option(model=...)` without sending prompts, collects offered `thought_level` values and `usage_update.size`, and closes the temporary session.
- Session import: lists sessions with `session/list` (filtered client-side by `cwd`) and replays indexed sessions with `session/load` (`acp.Conn.ReplayLoad`), never calling `session/load` on unindexed UUIDs.
- Fork and anchored revert (`agy.ForkConversation` / `agy.ForkConversationBeforeTarget`): snapshots `<appDataDir>/conversations/<id>.db` via SQLite `VACUUM INTO`, verifies the schema (`trajectory_meta`, `steps`), slices rows with `idx > cutIdx` from `steps`, `gen_metadata`, `executor_metadata`, `parent_references`, and `battle_mode_infos`, and rewrites `trajectory_meta.{trajectory_id, cascade_id}`. Live prompts stamp their native `agy-step:<idx>:<stepHash>` anchor into `provider_parent_uuid` on confirmation; fork/revert slices directly at `idx - 1` (verifying the step hash), falls back to exact content/neighbor/timestamp matching for pre-existing unstamped messages, and refuses loudly rather than guessing an ordinal when no unambiguous match exists.
- Thread transfer (`agy.TransferFiles`, `agy.CopyTransferFiles`, `agy.ValidateStagedTransferFiles`): snapshots `conversations/<id>.db` via `VACUUM INTO` plus companion files under `brain/<id>/`; copy transfers derive a deterministic UUIDv5 and rewrite `trajectory_meta`.
- Text generation (`textgen.RunAgy`): runs `agy -p --output-format json --json-schema <schema> --disable-slash-commands [--model <m>] [--effort <e>]` with `AGY_CLI_DISABLE_AUTO_UPDATE=1`.
- Multi-account, sign-in, and quota (`agy.StartLogin`, `agy.IdentifyAccount`, `agy.ProbeRateLimitsFromCredentialData`): manages `<appDataDir>/antigravity-oauth-token` via `provideraccounts`, signs in with `agy acp --login` in an isolated home (`AGY_ACP_FORCE_FILE_STORAGE=1`, `SSH_CONNECTION`), identifies accounts via Google OAuth2 userinfo, and reads 5-hour/weekly quota buckets from `v1internal:retrieveUserQuotaSummary` (with `fetchAvailableModels` fallback).
- CLI updates (`internal/providerupdate`): checks `https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/<os>_<arch>`, runs `agy update`, and reads the `## Antigravity CLI` section of `https://antigravity.google/docs/changelog.md`.

## Upstream ACP limitations (`agy 1.5.16`)

- **MCP tool call timeout:** `agy 1.5.16` enforces a hardcoded 180-second default MCP tool call timeout when `timeoutSeconds` is zero, and ACP `session/new` `mcpServers` entries do not accept a per-session `timeoutSeconds` override (only global `mcp_config.json` does). AO caps `wait_seconds` on built-in MCP tools (`ao-thread-tools`, `ao-workflow-tools`, `ao-remote-tools`) at 150 seconds for `agy` threads (`provider.Capabilities.MaxMCPWaitSeconds`) so calls return a backgrounded handle before `agy` aborts the HTTP request.
- **Live MCP server refresh:** ACP v1 exposes no `reconnect_mcp_server` RPC. Paired-computer updates to `ao-thread-tools` and `ao-remote-tools` tool lists take effect on the next `agy` session start or reconnect rather than refreshing in place.
- **Compaction progress signal:** `agy 1.5.16` emits no live `session/update` notification while compaction is in progress. AO records the post-compaction boundary when `step_type = 23` (`CORTEX_STEP_TYPE_CHECKPOINT`) completes in `conversations/<sessionId>.db`, without a live "Compacting..." status indicator.
- **Backgrounded `run_command` wire output:** `agy 1.5.16`'s ACP server only attaches `toolOutput()` on `tool_call_update` frames with `status: "completed"` or `"failed"`, never on `in_progress`. When a running `run_command` crosses `WaitMsBeforeAsync` (or parent model output resumes while it is still open), AO correlates `steps.task_details` in `conversations/<sessionId>.db` to promote that launch row into the background tray with its `task-<idx>.log` path, and emits a separate completion sibling (`EventBackgroundTaskTerminal`) when `tool_call_update(completed)` arrives.
- **Subagent lifecycle and transcript:** `agy 1.5.16` ACP does not stream subagent model/effort, thinking, tool calls, or replies on the parent wire. AO settles subagent lifecycle strictly from `agy`'s own parent-inbox signals under `<parent>/.system_generated/messages/` (direct reports, system error notices, idle exits, forced stops) and `SUBAGENT_STATE_KILLED` in `.system_generated/subagents/<childId>.json`, and tails the child's `transcript_full.jsonl` (falling back to `transcript.jsonl`) forward-only behind an `os.Stat` size gate to populate resolved model/effort and agent-pane items without inspecting child conversation databases.
