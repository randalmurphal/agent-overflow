# Claude Code References

When touching `internal/provider/claude/` or any Claude-specific
behavior, use these as the source of truth, not guesses derived from
our own code.

## Reference Repos

- **Claude Code source**: local mirror at
  `~/repos/claude-code-source-code/` (home-relative: the same checkout
  exists on each development host, and an absolute path here named one
  of them).
  - TypeScript source for an older Claude Code release. Its
    `package.json` `version` is the release it decompiles — 2.1.88 at
    the time of writing.
  - Useful when the installed `claude` binary's behavior is unclear
    and grepping minified strings is too noisy.
  - **Caveat:** the local copy is older than the installed binary in
    `~/.local/share/claude/versions/<version>` — well over a hundred
    releases behind, so treat the lag as the default rather than the
    exception — and may lag specific behaviors. Cross-check what you find against the installed
    binary's `strings` output before relying on it. Examples of
    drift seen in practice: the resume picker filter set
    (`utils/sessionStorage.ts:enrichLog`) gained additional rules
    in newer binaries; `initializeEntrypoint` (`main.tsx`) added
    overrides that rewrite preset env values.

- **Anthropic Claude Agent SDK**: `@anthropic-ai/claude-agent-sdk`.
  - Authoritative wire format and option shapes for stream-json
    invocations.
  - Read when the question is "what does the SDK actually send to
    the CLI?"

## Docs

- Claude Code overview: https://docs.claude.com/en/docs/claude-code
- Stream-JSON I/O reference: see `claude --help` (`--input-format`,
  `--output-format`, `--include-partial-messages`).

## Workflow

1. For wire shapes, start with `docs/references/claude-wire.md`.
   It pins canonical examples and known ambiguities.
2. For runtime behavior questions (how `--resume` filters, how the
   CLI decides entrypoint, how telemetry is tagged), grep the local
   `claude-code-source-code/src` first; it's faster than parsing the
   binary.
3. Confirm any source finding against the installed binary's
   strings if the area is one where drift has been observed.
4. If both sources disagree or are silent, follow
   `docs/references/spike-policy.md` and write an isolated spike.

## OAuth credentials on disk (spiked 2.1.257)

- **`refreshTokenExpiresAt` is the session deadline.** The
  `claudeAiOauth` object carries `expiresAt` (access token, 8h, also
  the rotation-chain position) and `refreshTokenExpiresAt`, both epoch
  milliseconds. The CLI sets the second one at sign-in from the
  server's `refresh_token_expires_in`, defaulting to 30 days when the
  response omits it, and a token refresh does NOT extend it. Observed
  lifetime is about 30 days from sign-in.
- **Past that date the account is already signed out.** The next
  refresh answers `invalid_grant` and the CLI overwrites the credential
  with a sign-out husk: the object stays, `accessToken` and
  `refreshToken` go blank. The CLI itself warns "Your login expires in
  N days · run /login to renew" inside the last 3 days. AO reads the
  field through `claude.RefreshTokenExpiresAt` and refuses to spawn a
  probe that would produce the husk.
- **Three locks guard the credential file**, all `proper-lockfile`
  style: a lock IS a directory created with `mkdir`, staleness is
  judged by its mtime, and an abandoned one is reclaimed by removing
  it.
  - `$CLAUDE_CONFIG_DIR/.oauth_refresh.lock` is held across the whole
    refresh (locked read, token POST, write), stale after 60s, mtime
    touched every 5s while held.
  - `${realpath(configDir)}.lock` is the legacy lock, taken second in
    the same section.
  - `$CLAUDE_CONFIG_DIR/.storage-write.lock` is the secureStorage
    mutation lock, taken innermost, inside the refresh section. It
    wraps every secureStorage write (invalidate cache, read, mutate,
    write), which means the credential read-modify-write AND logout,
    not only refreshes, so it is the only one of the three a sign-in or
    a sign-out takes. It retries 10 times at 100ms, is stale after 15s,
    and is created with `realpath: false`, so it is named from the
    unresolved config dir. Its base is
    `$CLAUDE_SECURESTORAGE_CONFIG_DIR` when that is set. It guards
    `.credentials.json` and the macOS keychain entry, not
    `~/.claude.json`.
- **The race that matters is the rotation drop.** A live CLI re-reads
  `.credentials.json` under its own lock immediately before every
  refresh, so replacing the file does not make a running process
  refresh a stale token. But if a write lands inside a CLI's token POST
  round trip, its compare-and-swap sees a different refresh token on
  disk, adopts the disk value, and discards the rotation it just paid
  for, retiring the outgoing refresh token server-side. Every AO write
  to the canonical credential takes all three locks in the CLI's order,
  each with the CLI's own stale threshold for that lock
  (`internal/provideraccounts/claude_refresh_lock.go`).
- On macOS the canonical credential lives in the keychain rather than
  the file, and all three locks still apply: they are named from the
  config home either way.

## Version-gated behaviors worth knowing

- **Todo/task tool surface (≥2.1.233)**: the CLI removes
  `TodoWrite` and the `TaskCreate`/`TaskUpdate`/`TaskGet`/`TaskList`
  tools for modern models (opus ≥4.8; sonnet/fable/mythos ≥5, with older
  families keeping them) unless the session opts in: a truthy
  `CLAUDE_CODE_ENABLE_TODO_TOOLS` (1/true/yes/on), one of the five
  names in `--allowedTools`, background/job mode, or a remote statsig
  gate (`tengu_rosy_wren`) that must never be depended on. Modern
  models get only the Task\* family even when opted in. `TodoWrite`
  stays absent. AO opts every session in: headless via
  `claude.withClaudeSessionEnvDefaults`, the TUI via `claudetui.buildEnv`
  (a user-overridable default on both paths, deliberately not a
  reserved pin; see `internal/provider/pinnedenv.go`).
  Spike-verified on 2.1.233: sonnet-5 `system/init` listed none of the
  four Task\* tools bare and all four with the env var.
  `CLAUDE_CODE_ENABLE_TASKS=false` is the separate whole-feature
  opt-OUT (default on).
- **Task-reminder nudges**: the CLI injects a "The task tools haven't
  been used recently…" reminder after 10 turns without a task write
  (and at most every 10 turns; turn-based, not token-based). Double
  gated: the producer requires `TaskUpdate` in the session's tool set
  AND the reminder mode ≠ "off", so removing the tools also removes
  the nudges. `CLAUDE_CODE_TODO_REMINDER_MODE=off` (env override over
  the `tengu_soft_slate_nudge` statsig gate, default "baseline") kills
  the nudges while KEEPING the tools. AO's todo-nudges toggle, in the
  Tools section of the Claude Code settings page, exports it on both
  spawn paths, same user-overridable-default posture as the opt-in
  above. Verified on 2.1.233 binary analysis.
- **Completed task lists self-delete (≥2.1.233)**: once every task in
  a list is `completed`, the CLI arms a 5s timer and then deletes the
  list's `~/.claude/tasks/<list-id>/*.json` files, bumping the
  high-water mark first so later ids stay monotonic. Nothing is
  emitted on the wire for it. AO mirrors the 5s constant in its
  read-side auto-hide (`app_live_state.go`) and refuses to cold-seed
  an all-completed stored list (`triage.seedTasksFromStoredTodo`).
- **Task lists are keyed by `CLAUDE_CODE_TASK_LIST_ID`** (read ahead of
  the team-name and session-id fallbacks; 2.1.257 binary analysis,
  honored end to end since the 2.1.233 spike). Unset, the list is keyed
  by session id: plain `--resume` keeps it, while `--fork-session` and
  AO's rollback slice mint a new id and orphan it. The CLI copies a
  session-keyed list to a fork only on the TUI's left-arrow background
  fork, never for `--fork-session`. AO pins the variable to the thread id
  on both Claude spawn paths so the list follows the thread through
  restarts, reverts and provider round-trips, and never clears
  `threads.live_todo` on those paths. A resume re-emits NO task events,
  so AO learns task state only from live `TaskCreate` / `TaskUpdate` or
  its own `threads.live_todo`. `--resume-session-at` (the crash-repair
  path) is untested.
