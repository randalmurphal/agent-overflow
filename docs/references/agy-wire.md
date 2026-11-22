# Antigravity (`agy`) ACP Wire Reference

Antigravity CLI (`agy`) speaks Agent Client Protocol (ACP) v1 over newline-delimited JSON-RPC 2.0 on stdio (`agy acp`).

## Process lifecycle and sharing

- Launch: `agy acp` (single shared process per binary + environment + binary identity). Do not pass `--process-per-session` or `--dangerously-skip-permissions`.
- Environment:
  - Pin `AGY_CLI_DISABLE_AUTO_UPDATE=1` so background self-updates do not replace the running binary mid-session.
  - Scrub ambient `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GOOGLE_CLOUD_PROJECT`, and `GOOGLE_APPLICATION_CREDENTIALS` unless explicitly configured by the user.
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
- Fork and anchored revert (`agy.ForkConversation`): snapshots `<appDataDir>/conversations/<id>.db` via SQLite `VACUUM INTO`, verifies the schema (`trajectory_meta`, `steps`), slices rows with `idx > cutIdx` from `steps`, `gen_metadata`, and `executor_metadata`, and rewrites `trajectory_meta.{trajectory_id, cascade_id}`.
- Thread transfer (`agy.TransferFiles`, `agy.CopyTransferFiles`, `agy.ValidateStagedTransferFiles`): snapshots `conversations/<id>.db` via `VACUUM INTO` plus companion files under `brain/<id>/`; copy transfers derive a deterministic UUIDv5 and rewrite `trajectory_meta`.
- Text generation (`textgen.RunAgy`): runs `agy -p --output-format json --json-schema <schema> --disable-slash-commands [--model <m>] [--effort <e>]` with `AGY_CLI_DISABLE_AUTO_UPDATE=1`.
- Multi-account, sign-in, and quota (`agy.StartLogin`, `agy.IdentifyAccount`, `agy.ProbeRateLimitsFromCredentialData`): manages `<appDataDir>/antigravity-oauth-token` via `provideraccounts`, signs in with `agy acp --login` in an isolated home (`AGY_ACP_FORCE_FILE_STORAGE=1`, `SSH_CONNECTION`), identifies accounts via Google OAuth2 userinfo, and reads 5-hour/weekly quota buckets from `v1internal:retrieveUserQuotaSummary` (with `fetchAvailableModels` fallback).
- CLI updates (`internal/providerupdate`): checks `https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/<os>_<arch>`, runs `agy update`, and reads the `## Antigravity CLI` section of `https://antigravity.google/docs/changelog.md`.
