# internal/settings/

User preferences behind ONE service, persisted across three homes: the
HOST tier in a JSON file with sparse serialization (zero-value fields are
omitted so defaults are forward-compatible), and — once `AttachTierStore`
has wired the `ui_state` table in — the USER tier in the reserved
`user:default` scope and the DEVICE tier in the CALLING connection's own
bucket. `tier.go` decides which is which; `residency.go` is where the
split lives. A service with no store attached keeps every tier in the
file, which is what the pre-database boot readers in `main.go` and
`main_desktop.go` depend on.

## Layout

- `settings.go`: `Settings` struct, `Load` / `Save`, the sparse
  JSON marshal/unmarshal, schema versioning (`CurrentSchemaVersion`).
- `validate.go`: enum allow-lists (timestamp format, provider,
  reasoning effort, text-generation provider) plus the
  `ValidateRemoteEndpointURL` / `ValidateRemoteEndpointToken` helpers
  used by both the App-level remote-endpoint mutators and the
  `--connect` URL parser. Single `Validate` entry point for the
  Settings struct as a whole. The generic list/string normalizers live
  here too (`dedupeTrimmed` and `truncateRuneSafe`) because each has
  callers in more than one feature file; `truncateRuneSafe` bounds a
  length without splitting a rune AT THE CUT and is deliberately not a
  UTF-8 repair pass, so an already-invalid stored value keeps its bytes
  instead of collapsing to empty.
- `providerenv.go`: the `ProviderEnvVar` shape (user-defined
  environment for a provider's subprocesses), its name/value rules,
  the reserved-name deny-list, the load-time sanitizer, the
  `RedactProviderEnvVars` wire projection, and the
  `SetProviderEnvVar` / `DeleteProviderEnvVar` Service mutators.
- `promptoverrides.go`: the `PromptOverride` shape (per-provider
  system-prompt replacement scoped to a model list) plus the disabled-tool
  lists, their bounds, the strict validators and the lenient load-time
  sanitizers, and the `PromptOverridesForProvider` /
  `DisabledToolsForProvider` selectors. Both selectors route `claude-tui`
  onto the Claude lists, exactly like `HiddenModelsForProvider`: it is the
  same binary, and the interactive TUI honors `--system-prompt-file` and
  `--disallowedTools` the same way headless does (spike-verified 2.1.234;
  `internal/provider/claudetui/launch.go` passes both).
  Neither tool list is enum-checked
  here: they speak two different vocabularies (Claude raw tool names,
  Codex curated toggle ids) and validating either against a table this
  package cannot see would make a settings file that outlives one AO
  version unloadable. Unknown Codex ids are skipped with a log line in
  `internal/provider/codex`; an unknown Claude name is inert to the CLI.
  The prompt lists carry TWO byte bounds, and the aggregate one is the
  load-bearing half: `GetSettings` ships both lists whole on every read,
  including to a LAN client, so `MaxPromptOverrideLen` alone would put
  50 × 64 KB per provider on the wire. `MaxPromptOverridesTotalLen` caps
  the sum: the strict path refuses an over-cap list, the lenient path
  keeps whole entries until the sum would exceed it and logs what it
  dropped. Every cap this file applies to a hand-authored list is
  audible: a tail that vanishes on load is otherwise indistinguishable
  from a save that never happened.
- `claudesession.go` holds the three Claude-only session axes that reach the
  CLI through its `--settings` block rather than a flag: `outputStyle`
  (closed allowlist of the four built-in styles), `claudeSubagentLimits`
  (spawn depth + concurrency, bounded), and `claudeToolMemoryLimit` (a
  size string). Each has a STRICT validator used by `Validate` and a
  LENIENT sanitizer used on load, following this package's usual pair.
  Two rules are worth stating because they are the difference between a
  setting that works and one that silently does nothing:
  - **Empty is "the CLI's default", never a value.** Every one of these
    axes is emitted only when non-empty/non-zero, because the CLI's
    unset behavior is a real policy (a subagent limit of 0 is unsendable,
    since the CLI's schema is `int({min:1})`) and overwriting it with our
    idea of a default would be a behavior change nobody asked for.
    `claudecrosssession.go` is the one deliberate exception, and it is
    documented there: for that axis the CLI's unset behavior is a silent
    discard, so "enabled" always sends an explicit value.
  - **`ClaudeSessionAxesForProvider` answers zero for anything but
    `claude`.** Unlike `PromptOverridesForProvider` /
    `DisabledToolsForProvider`, `claude-tui` is deliberately NOT routed
    onto Claude's values: its PTY launch passes no `--settings` flag at
    all, so returning them would advertise settings that cannot reach
    the binary.
  The file also holds `claudeThinking`, which is in this file for
  proximity (it is the fifth Claude-only session axis) but is NOT part of
  that bundle and deliberately not on `ClaudeSessionAxes`: it reaches the
  CLI as spawn FLAGS (`--thinking` / `--max-thinking-tokens` /
  `--thinking-display`) and as a `set_max_thinking_tokens` control
  request on a session that is already running, so it rides
  `provider.SessionOptions` (the struct `claude.PlanLiveUpdate` diffs)
  rather than the spawn-only block. `mode` is `""` / `off` / `budget`,
  `budgetTokens` is required and bounded (1024..128000) in budget mode
  and forced to zero outside it (a stored number nothing reads is not a
  setting), and `display` is `""` / `summarized` / `omitted`.
  `ClaudeThinkingForProvider` answers zero for anything but `claude` for
  the same shape of reason as the bundle, but a DIFFERENT one in
  substance: claude-tui drives the same binary and would take the flags,
  but it has no control-request channel and no live-update surface, so
  the axis would be half a feature there.
  The tool-memory limit's grammar mirrors the CLI's own parser
  (`/^(\d+(?:\.\d+)?)\s*([kmgt]?)(?:i?b)?$/i` plus its falsy words,
  extended with `none`), and it only takes effect when the CLI runs on
  Linux. It is implemented as a memory cgroup write. The WSL backend
  counts; a native Windows or macOS backend does not.
- `claudecrosssession.go`: `claudeCrossSession`, Claude Code's
  machine-wide peer inbox (`ListAgents` / `SendMessage` between sessions
  on one host). TWO mechanisms in one struct, which is why it is not
  simply a fourth entry in `claudesession.go`: `enabled` opens the CLI's
  experiment gate (a spawn environment variable) and passes a `--name`,
  while `inbound` is the `--settings` delivery policy that only means
  anything once the gate is open. Two rules with teeth:
  - **`hold` is refused by name, and an enabled-but-empty policy resolves
    to `accept`** (`EffectiveInbound`). Which of the CLI's three schema
    values AO emits, why an absent key is as bad as `hold`, and why a
    DISABLED session states `refuse` rather than staying silent are all
    in `internal/provider/claude/AGENTS.md` §Cross-session messaging. The
    off-means-refuse wall lives in `internal/provider/claude/options.go`,
    not here: "off" is a provider-level refusal, not a policy the user
    chose.
  - **Off by default.** Letting another process start a turn in the
    user's thread is opt-in, so `ClaudeCrossSession` stays out of
    `DefaultSettings`, the same rule as `ClaudeTUIEnabled`.
  Unlike the axes above, a change here is reconciled onto RUNNING
  sessions: `app_settings.go` fans out `reconcileLiveClaudeSessions` on
  the patch key and the axis rides `provider.SessionOptions`, so
  `claude.PlanLiveUpdate` queues a deferred restart rather than leaving
  the change for whenever the next session happens to start.
- `spinner.go` holds the composer working-indicator knobs' bounds and
  validators: the custom-verb list, the animation EXCLUSION list, and the
  compaction-animation selection. Four rules worth stating:
  - **`spinnerVerbsEnabled` defaults TRUE and therefore lives in
    `DefaultSettings`**, the mirror image of `ClaudeTUIEnabled`: the verb
    is what the indicator has always shown, so an absent key in an older
    file must read as on. `spinnerAnimationsEnabled` is the opt-in half
    and stays out of `DefaultSettings` by construction.
  - **A verb is refused, never truncated.** Over-long or control-bearing
    entries fail the write; a verb cut mid-word would render as something
    the user never typed. The cap counts RUNES, because this is display
    text in any script.
  - **The animation list is an exclusion list.** Ids the user unchecked,
    so a sprite dropped into `<configDir>/spinners` tomorrow joins the
    random pool automatically. The id grammar is duplicated from
    `internal/spinner` rather than imported. This package stays
    dependency-free, and the vocabulary here is wider anyway (built-in
    sprites ship with the frontend and never appear in that directory).
  - **`spinnerCompactionAnimation` stores `""` for "never chosen", never
    a concrete id.** The frontend catalog names the default sprite;
    baking that id in here would duplicate a frontend vocabulary AND make
    the settings UI's "Default" selection unrepresentable (the echo would
    match no `<option>` and the select would render blank). `"none"` is
    the explicit nothing; anything else is an id the frontend resolves.
- `remote.go`: the `RemoteEndpoint` shape and its CRUD helpers
  (`Add` / `Update` / `Delete` / `Touch`). Backs the `--connect`
  target list the desktop binary's settings panel exposes.
- `network.go`: `NetworkSettings` — how this backend is REACHED — plus
  the usual strict/lenient pair. It carries the LAN bind toggle, the
  listen port, the canonical domain, the DNS-01 hook argv, the external
  cert/key pair, and the tailnet toggle with its coordination-server URL
  (`docs/specs/remote-access.md` §7). The PORT is 0 for automatic (an
  ephemeral bind cached in `transport-port.json`, which is every install
  that never touched this) or 1-65535 — privileged ports included,
  because a backend with `CAP_NET_BIND_SERVICE` can hold one and this
  package cannot see that capability. An out-of-range value is REFUSED,
  never clamped: a clamp would bind some port and report success, leaving
  the operator looking for their backend at a number nothing chose. Four
  rules the validator
  enforces on the domain half, each because the alternative is a
  backend that cannot serve what it claims: a domain is a bare hostname
  (`validateBareHostname`, the same rule the GitLab host allowlist
  uses — reused rather than restated, because "is this a hostname" has
  one answer), a hook
  without a domain is refused because there is nothing to order a
  certificate FOR, the external pair is both-or-neither and absolute
  paths only, and the pair is refused without a domain because SNI is
  what selects it. A domain with NEITHER a hook nor a pair is
  deliberately allowed: that is the deployment where something else
  terminates TLS in front. The tailnet half has one: a control URL, if
  given, parses to an absolute `http`/`https` URL with a host, because
  an unusable one is a node that can never come up.
  **The lenient path keeps `BindAll` and validates the three halves
  SEPARATELY.** The bind toggle is independent of all of it; a
  half-configured domain is one the reconciler could act on wrongly, so
  that half drops whole — and it drops WITHOUT taking the tailnet with
  it, because a stale domain typo must not be able to pull this backend
  off the tailnet. When the tailnet half is itself unusable, the
  ENABLED BIT drops with the URL: an empty control URL means the public
  coordination server, so keeping the toggle alone would register the
  node somewhere the user never named. The BIND half is two values and
  only one of them can be wrong: an out-of-range port drops to 0, which
  means automatic — what every install did before the field existed — so
  the backend still binds and still starts.
- `mutate.go`: the SINGLE persisted-write path. Every mutator in this
  package (`Update`, `AddRecentWorkspace`, the remote-endpoint CRUD, the
  provider-environment CRUD) is a closure handed to `Service.mutate`,
  which loads the file, projects the before state per key, applies,
  writes, restamps the cache, and reports which keys moved to the
  registered `ChangeObserver`. `internal/app` uses that observer to emit
  `settings:updated` so a second attached client converges without a
  refresh, which is why the chokepoint is enforced rather than
  conventional: `TestOnlyMutateWritesSettings` fails if anything else
  calls `writeSparse`. It also ROUTES: after apply, the keys that moved
  are split by tier and each group is persisted to its own home, so the
  file is not rewritten for a write that only moved a font size. Three
  properties the callers depend on: the observer runs AFTER the write
  lock is released (an observer that reads settings back would otherwise
  deadlock), a write that moved no key reports nothing at all, and a
  write spanning two homes is not one transaction — SQLite and a JSON
  file cannot be — so a failure part-way through announces nothing and
  the next read sees whatever landed.
- `tier.go`: the host / user / device taxonomy (`docs/specs/remote-access.md`
  §6) as a total map from settings key to tier, plus the per-key JSON
  projection and diff `mutate` reports with. Total is the point: a new
  settings field fails `TestEverySettingsKeyHasATier` until it is placed,
  because an unplaced key is one that silently stops announcing itself —
  and since phase 4 an unplaced key is also one with no home, because
  this map is the STORAGE routing table. Placing a key is now two
  decisions in one: who may write it, and where it lives.
- `residency.go`: the three homes. `AttachTierStore` wires the `ui_state`
  table and seeds it from the file; `Service.For(bucket)` is the service
  seen from one connection, with the device tier resolved out of THAT
  caller's bucket. Storage format is one row per key, spelled exactly as
  the settings JSON key, holding the JSON encoding of the typed value —
  the store stays opaque, and typed validation happens here before the
  write. The non-collision argument against the frontend's own
  `appStorage` keys is stated in the file and is worth keeping true: every
  `appStorage` key is either colon-namespaced or one of three named legacy
  spellings, and no settings key contains a colon.
- `gendefaults.go` + `gendefaults/`: the generator that makes
  `DefaultSettings` the SINGLE source of settings defaults. It reflects
  the struct's json tags (the `knownSettingsFieldNames` walk), takes each
  value from `DefaultSettings`, materializes zero values explicitly
  (`omitempty` is ignored; a nil slice emits `[]`), and writes
  `frontend/src/lib/generated/settingsDefaults.ts`. See "Frontend
  defaults" below.

## Responsibility boundary

- What BELONGS here:
  - Reading / writing the user's JSON settings file.
  - Validating enum fields against the allowed sets.
  - Default values (implicit via Go zero values).
- What does NOT belong here:
  - Settings UI. That's the frontend.
  - Provider-specific runtime config. That's derived from `Settings`
    plus `store.ThreadView` at session creation.

## Extension points

- To add a new setting: add the field + a default + (if enum) an
  allow-list + a `Validate` branch + its tier in `tier.go` + a test that
  asserts round-trip. The tier is now two decisions at once — who may
  write the key, and which of the three homes it lives in — so a key
  that a REMOTE screen should own its own copy of goes to the device
  tier, a preference that follows the person across screens goes to
  user, and anything this backend's own behaviour reads goes to host or
  user but never device (one backend behaviour cannot be driven by a
  per-screen value).
  A field whose intended default is the Go zero value stays OUT of
  `DefaultSettings`. That is what makes an absent key read as the
  default for every settings file written before the field existed.
  `ClaudeTUIEnabled` is the deliberate example (opt-in claude-tui
  visibility, 2026-08-18): `ClaudeEnabled` / `CodexEnabled` beside it
  default true, so the inversion is documented at the field and pinned
  by `TestClaudeTUIEnabledDefaultsOffAndRoundTrips`. Do not "fix" it by
  adding it to `DefaultSettings`. `writeSparse` persists what differs
  from the defaults, so that would drop the user's `true` on write.
  Then decide the field's FRONTEND default: either let the generator emit
  it or add it to `frontendDefaultsDenied` with a reason, and run
  `go generate ./internal/settings`. `TestFrontendDefaultsDenyListIsTotal`
  fails until you have chosen one, and
  `TestFrontendDefaultsSourceIsCheckedIn` fails until you regenerate.
- To change allowed values for an existing enum: update the map in
  `validate.go` and the migration note; old values are normalized on
  load, never at write time.

## Frontend defaults

The frontend does NOT hand-mirror these defaults. `gendefaults.go` renders
`DefaultSettings` into `frontend/src/lib/generated/settingsDefaults.ts`
(`SETTINGS_DEFAULTS`), which the settings store, `activityRunPrefs` and
`test/helpers/settings.ts` all read. Regenerate with
`go generate ./internal/settings`.

Three rules, each with a test behind it:

- **The emitted set is exactly the keys the frontend wants defaulted.**
  Every json field is either emitted or listed in `frontendDefaultsDenied`
  with a one-line reason (`TestFrontendDefaultsDenyListIsTotal`). Two kinds
  of reason live there: a field with no TypeScript counterpart at all
  (`$schemaVersion`, `window`, `editor`) or a redacted one the store must
  not materialize (`remoteEndpoints`), and a field the TS `Settings` type
  declares OPTIONAL where absence is the meaning — the prompt/tool
  overrides and the Claude session axes, where "" or absent means "the
  provider decides". Materializing that second kind changes merge
  semantics for anything that tests presence, so it is a deliberate
  choice, never a default-on.
- **Zero values are emitted explicitly.** `omitempty` drops them on the
  wire, so `mergeSettingsWithDefaults` is what puts them back; a key
  missing here leaves the store holding `undefined` for a field the
  backend considers set.
- **`satisfies Settings` in the generated file is the TS-side tripwire.**
  A new required TS field, or a Go field removed under one, breaks
  `pnpm run check` rather than drifting silently.

## Retired fields

A setting that MOVED OUT OF THIS PACKAGE (not one that was deleted, and
not one that merely changed tier) is retired rather than kept: it comes
out of the `Settings` struct and goes into `retiredSettingsFieldNames()`.

A key that changed TIER is a different thing and must not be retired: it
is still a settings key, still on the wire, still validated here — only
its storage moved. `writeSparse` drops it from the file because it is no
longer file-resident, and `loadFromFile` rolls back what a stale copy in
the file would otherwise set. Retiring it would take it off the wire and
break the frontend. The name in that set does two things.
`captureUnknownFields` skips it, so the sparse writer does NOT round-trip
it, and `Validate` never sees it because the struct has no field for it.

The consequence is the part to get right: a retired value is **consumed
once and then gone**. Unmarshalling drops it, nothing republishes it, and
the next `Update` (any update, from anywhere) rewrites the file without
it. It is not "left on disk".

`Service.RetiredString(field)` is the one legitimate reader: a raw read of
the FILE (not the typed cache, which cannot hold a field the type no
longer has) that answers `""` for every failure. It exists for the
one-time migration that moves the old value to wherever it now lives.
Two rules follow, and both are load-bearing:

- The migration must run on the BOOT path, before any `Update` can reach
  the file. `app_startup.go`'s `initThemeDirectory` is the live example
  (`theme` → `<configDir>/themes/appearance.json`,
  `docs/specs/theme-system.md` §6.2), and `app_appearance_test.go` pins the
  ordering in both directions.
- A migration that can FAIL must carry the value in process state, because
  the drop happens whether or not the migration succeeded.
  `theme.Service` keeps it in `bootPending` / `pendingLegacy` and retries
  from the next read.

## Secrets on the wire

Two fields hold material that must not cross the transport boundary in
bulk: `RemoteEndpoints[*].Token` and the values of custom environment
variables flagged `sensitive`. `GetSettings` is reachable from a
LAN-attached client, so `redactedSettings` (app_settings.go) clears both
on every read path.

That makes the generic patch path unsafe for those fields. A
`GetSettings -> mutate -> Update` round trip would write the redaction
back. `Service.Update` therefore REJECTS `remoteEndpoints`,
`claudeCustomEnv`, and `codexCustomEnv`; each has dedicated mutators
that read the persisted value before writing. Any future field that
gets redacted on read must follow the same pattern in the same commit.

`workflowPaused` is refused by the same guard for a different reason:
persisting a pause the engine never heard about is not the same act as
pausing. Its one write path is `WorkflowSetGlobalPause`, which applies
the pause and then persists it through `Service.SetWorkflowPaused`. The
shape generalizes — a key whose write has a CONSEQUENCE beyond the file
does not belong on the generic patch path.

## Anti-patterns

- Do NOT import `internal/provider`. Two tables here are duplicated
  from that package on purpose, to avoid a dependency cycle:
  the allowed-reasoning-efforts map (from
  `provider.AllReasoningEfforts`) and the custom-environment
  deny-list (from `provider.ReservedEnvNames`). Update both sides
  together; the deny-list has a root-package test
  (`TestReservedEnvNamesMatchTheProviderPins`) that fails on drift in
  either direction.
- Do NOT assume nothing under `internal/` imports this package. One does:
  `internal/promptoverride` imports it for the `PromptOverride` shape,
  the only inbound edge from another `internal/` package, and the reason
  this package's types have to stay dependency-free. It is the pure half
  of the same feature (matching an entry to a session model, rendering
  its placeholders) and lives there because it needs
  `internal/provider`'s slug normalizer, which is exactly the import
  banned above. Keep the direction one-way: nothing here may reach back
  into `promptoverride`, or the cycle returns through the front door.
- Do NOT sneak business logic into `Validate`. It enforces shape, not
  behavior.
- Do NOT write partial settings silently. If validation fails, the
  save is an error and the caller decides the user-facing message.
- Do NOT add a mutator that calls `writeSparse` (or `Save`) directly.
  It would persist correctly and announce nothing, so every other
  attached client would sit on stale state until its next full read.
  Route it through `mutate` and place its keys in `tier.go`.

## References

- The frontend reads / writes settings through Wails bindings; the
  binding generator picks up `Settings` automatically, and the defaults
  generator above keeps its DEFAULT VALUES in step.
