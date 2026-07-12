# M0 Delegation Ledger

Campaign: workflows-system M0 (four parallel lanes off one base commit).
Lanes live under `~/repos/ao-lanes/`; logs under the orchestrating session's
scratchpad. Review protocol: WIP-commit the lane immediately on return, then
scope/assumptions/gaming audits + independent gate re-runs before merge.

| Packet | Branch | Lane | Model / effort | Session id | Status |
|---|---|---|---|---|---|
| P0.1 turn-observer registry | `m0/p01-turn-observers` | `~/repos/ao-lanes/p01` | gpt-5.6-sol / high | run1 `019f5435-f377…` (BLOCKED, valid); run2 `019f545b-1d9d-7f41-be57-997dd740ddfe` | **merged** |
| P0.2 project slugs + config dirs | `m0/p02-project-slugs` | `~/repos/ao-lanes/p02` | gpt-5.6-sol / high | `019f5436-71b2-7c21-becc-69506f21bc46` | **merged** |
| P0.3 OS notifications | `m0/p03-os-notifications` | `~/repos/ao-lanes/p03` | gpt-5.6-sol / high | `019f5437-1334-7ad1-9d71-4b04bb7b8439` | PARKED (valid BLOCKED) |
| P0.4 docs hygiene | `m0/p04-docs-hygiene` | `~/repos/ao-lanes/p04` | gpt-5.6-sol / high | `019f5434-5f98-7ba2-9bea-3b89c2d30656` | **merged** |
| P1.1 `internal/workflow/def` | `m1/p11-workflow-def` | `~/repos/ao-lanes/p11` | gpt-5.6-sol / high | `019f545e-ab3a-78f2-a7bc-66e4f65a8164` | **merged** |
| P1.2 `internal/workflow/profile` | `m1/p12-profile` | `~/repos/ao-lanes/p12` | gpt-5.6-sol / high | `019f5478-b246-7863-b9ac-80977313ffc5` | **merged** |
| P1.3 `ao` CLI skeleton | `m1/p13-ao-cli` | `~/repos/ao-lanes/p13` | gpt-5.6-sol / high | `019f5478-bdb3-75c0-b1c4-4ee5139ae19a` | **merged** |
| P1.4 starters + `ao workflow new` | `m1/p14-starters` | `~/repos/ao-lanes/p14` | gpt-5.6-sol / high | `019f548e-a038-7f82-87ab-750b8b20957d` | **merged** |
| P2.1 workflow persistence | `m2/p21-workflow-persistence` | `~/repos/ao-lanes/p21` | gpt-5.6-sol / high | run1 `019f549e-e74f-7721-98dd-9abcb4fc730b` (dead on arrival); run2 `019f54ee-a5e8-7b41-8825-6ccb1b790a3b` | **merged** |
| P2.2 engine core | `m2/p22-engine-core` | `~/repos/ao-lanes/p22` | gpt-5.6-sol / **xhigh** | `019f5505-71d3-73d0-89d5-f5e55b7230a9` | **merged** |
| P2.3 provider envelope wiring | `m2/p23-provider-envelope` | `~/repos/ao-lanes/p23` | gpt-5.6-sol / high | `019f54ee-b30b-72d1-b57e-58f8456c56d0` | **merged** |
| P2.4 phase runner + app wiring | `m2/p24-phase-runner` | `~/repos/ao-lanes/p24` | gpt-5.6-sol / **xhigh** | `019f553e-41c5-76d1-9bb2-433541267c0b` | **merged** |
| P2.5 reliability | `m2/p25-reliability` | `~/repos/ao-lanes/p25` | gpt-5.6-sol / **xhigh** | run1 `019f556f-851a-79c0-8910-2c3ba63ff505` (dead on arrival — usage limit); run2 `019f5601-046a-7d11-96ce-38b7df0d666d` | **merged** |
| P2.6 workspace isolation | `m2/p26-workspace-isolation` | `~/repos/ao-lanes/p26` | gpt-5.6-sol / **xhigh** | `019f5635-3455-7483-9849-9a35a0805bb4` | **merged** |
| P2.7 harness workflows | `m2/p27-harness-workflows` | `~/repos/ao-lanes/p27` | gpt-5.6-sol / **xhigh** | `019f566e-6df8-7e71-9f2d-843f7b228a5d` | **merged** |
| P0.3 rev2 OS notifications | `m3/p03r2-os-notifications` | `~/repos/ao-lanes/p03r2` | gpt-5.6-sol / **xhigh** | `019f56ae-e129-7612-be10-c9c677757ed9` | **merged** `6cb823f4` |
| P3.1 thread modes + take-over | `m3/p31-thread-modes` | `~/repos/ao-lanes/p31` | gpt-5.6-sol / **xhigh** | run1 `019f56fc-e0c2-78e0-acf4-6979f21e303e` (dead on arrival — usage limit); run2 `019f5714-98e5-77f2-8d0a-cc289a6bf374` | **merged `3ee937f8`** (workflows-system) |
| P3.2 disposition + UI data + notifications | `m3/p32-disposition-uidata` | `~/repos/ao-lanes/p32` | gpt-5.6-sol / **xhigh** | `019f576c-444e-70b3-9bd5-0afadd50c1de` | **merged `6d329261`** (workflows-system) |
| P3.4+P3.5 workflows pane frontend | `m3/p345-workflows-pane` | `~/repos/ao-lanes/p345` | gpt-5.6-sol / **xhigh** | `019f57c7-df07-70c3-b29f-0af4eb40af07` | dispatched |

## Events

- Base commit for all lanes: `c49e2bd7` (packets committed on main).
- Lane bootstrap finding: `make go-build` fails in a fresh worktree because
  `main.go` embeds `all:frontend/dist` (gitignored build output) — every Go
  lane needs `pnpm install && pnpm run build` in `frontend/` before the Go
  gates run. Done for p01/p02/p03 before dispatch; baselines green.
- p03 pre-dispatch baseline included a full `make e2e` pass in the lane
  (harness + Playwright work under worktree isolation).
- All four dispatched with `--dangerously-bypass-approvals-and-sandbox`,
  banner-verified sol/high. Logs: session scratchpad `p0N-codex.log`.
- p04 log shows a startup `rmcp` MCP worker error (`AuthorizationRequired`) —
  an unauthenticated MCP server in the user's codex config; unrelated to the
  packet, run proceeds.
- **P0.1 BLOCKED at baseline (valid).** `make test-race` timed out (10m,
  root package) on the UNTOUCHED base tree while three other lanes saturated
  the machine; the named test had 0s elapsed at panic (predecessors ate the
  budget). Claude repro running on a quieter machine to split
  pre-existing-regression vs load-flake before resume/fix.
- **P0.3 PARKED on a valid BLOCKED with partial work** (WIP-committed on the
  lane branch). Blocker 1: the real Windows path splits UI (native launcher)
  from backend (`-tags nogui` in WSL) — no notification bridge exists between
  them; needs a scope/design decision (user earlier deferred remote/notif
  delivery fixes). Blocker 2 VERIFIED by Claude in the pinned fork
  (`pkg/services/notifications/notifications_linux.go:658`): dbus close
  reason 2 (dismiss) is surfaced as `DefaultActionIdentifier` — dismiss and
  click are indistinguishable, so activation nav would misfire; fix belongs
  in the user-owned wails fork. Also flagged: sync macOS auth can block
  startup up to 180s (must go async); cold-start activations need a bounded
  pending queue until frontend hydration. Its `go.mod` change is only the
  indirect `go-toast` dep pulled by the mandated wails package — not a
  violation. Rev2 packet to be authored (target: M3 boundary) with the
  bridge decision, fork patch, async auth, activation queue.
- **User rulings on P0.3 (mid-campaign):** (1) if the Windows launcher
  needs the remote/bridge changes, do them — rev2 scope includes the
  launcher↔backend notification bridge rather than deferring it. (2) Wails
  fork changes must be logged as upstream candidates once fully verified —
  user wants to submit fix PRs upstream. **Upstream verification DONE for
  the dismiss-vs-click bug:** identical code in `wailsapp/wails` master at
  `v3/pkg/services/notifications/notifications_linux.go:658` — dbus
  `NotificationClosed` reason 2 (user dismissed, per freedesktop spec)
  synthesizes a `NotificationResponse` with `DefaultActionIdentifier`,
  indistinguishable from a real click (real clicks arrive via
  `ActionInvoked`, line 566/576). Apps navigate on dismiss. macOS
  distinguishes these (`UNNotificationDismissActionIdentifier`), so the
  cross-platform surface is inconsistent too. → UPSTREAM PR CANDIDATE.
  **STANDING CONSTRAINT (user, verbatim intent): may fix the issue and
  create the branch off latest upstream, but do NOT open the upstream PR
  until the user has explicitly approved it.**
- **P0.1 root-cause CONFIRMED (not a load flake):** `make test-race` fails
  identically on a quiet machine — the root package hits the 600s per-binary
  `-timeout` with tests still progressing (same test reached at panic in
  both runs = deterministic order + honest slowness, no hang). The harness
  commit (6c2bfcbb) grew the root package's -race runtime past the budget.
  Fix: measure honest runtime (`go test -race -timeout 1800s .` timing run),
  raise the Makefile `test-race` timeout with margin for parallel-lane load,
  land on main, reset the p01 lane, fresh dispatch (per skill: tree reset
  under a session → fresh, not resume; it had produced only BLOCKED.md).
  Observed debt (not fixed now): root-package -race runtime >10min deserves
  a test-speed pass someday.
- **Gate fix verified on main (`bc1d28b9`):** full `make test-race` green;
  root package 679s under two-codex-lane load (would have failed the old
  600s budget; 1800s has ~2.6x headroom), triage 374s.
- **P1.1 reviewed + merged.** 2795-line def package, scope exact. Claude
  audit highlights: D2a envelope generator byte-deterministic (sorted
  required + Go map-key marshal order); ValidateEnvelope enforces strict
  three-shape mutual exclusion + all-findings-sorted feedback errors + size
  cap with write-to-a-file guidance; "ancestor" formalized as DOMINANCE on
  the loop-free forward graph (statically sound choice — loop targets are
  guaranteed executed on every path; consistent with D2/D5 producer rules);
  unbounded forward cycles rejected (bounded loop routes only); first-match
  route-order dead-route detection; optionality propagates through dotted
  paths via required-field absence; interpolation provably inert
  (ReplaceAllStringFunc, no rescan) with `(not provided)`; prompt files
  template-validated against declared inputs with symlink-confined paths;
  BindingsUnchecked is a distinct visible status. Assumptions all
  reasonable (per-phase inputs redeclare consumer schema; 1MiB/4MiB read
  caps; scheduler blocks live outside the workflow doc per spec §11;
  sub-workflows post-v1). Claude re-ran go-build + focused -race + full
  go-test independently: green.
- **P0.2 flagged an internal packet contradiction** (standing rules ban all
  git ops; a gate asked for `git diff --stat` output) — resolved
  conservatively, no git run. Future packets: standing rules should permit
  read-only git inspection explicitly.
- **P0.2 reviewed + merged.** Migration v21 + deterministic backfill
  (ordered `created_at,id`, in-memory collision set, unique index created
  post-dedupe in the same tx), single-sourced slugifier at the CreateProject
  chokepoint (caller-supplied slug ignored; single-connection SQLite makes
  check-then-insert race-free), `EnsureForWorkspace` reloads the persisted
  row (flagged in manifest), bindings regen slug-only, schema.md pointer +
  slug docs. It experimented with a bound-method change and RESTORED it for
  scope compliance (flagged). Claude re-ran go-test + frontend check/build
  independently: green. One rider fix at merge: reverted a stray unicode
  quote artifact in an existing v20 test comment. Known gap (deliberate,
  assumption-flagged): bound `App.CreateProject` returns the pre-insert
  struct with `Slug == ""`; every store read returns the real slug. M1
  consumers read via store; consider returning the persisted row when a
  caller actually needs it.
- **P0.4 reviewed + merged.** Scope exact (2 permitted files), 5 benign
  assumptions, ran full repo gates unprompted (honest output pasted).
  Claude audit: 7 verification-map anchors spot-checked against code
  (threadmode immutability, /design/ loopback guard, 2 MCP tools +
  8-tile cap + clip note, watcher .tmp suppression + polling fallback,
  prompt override path, .picked/LatestUnpickedOptionSet, workdirs under
  dbDir) — all accurate. Report preserved at `reports/P0.4-report.md`.
- **P1.2 reviewed + merged (`372e2691`).** Strict loader per D6+amendments;
  compile-time def.Bindings assertion; discriminated per_item_budget
  (tokens|usd|wall_clock, exactly one); explicit ResolveSecrets with mask
  list + no-leak formatting test. Claude rider: trim trailing newline on
  file-sourced secrets — an untrimmed mask ("abc\n") can never match the
  value as it appears in narrative text, so D2-8 masking would miss it.
- **P1.3 reviewed + merged (`7e654c88`).** cmd/ao + internal/aocli, offline
  validate/list per D15/M1 fence; exit codes 0/1/2; bindability status
  always visible. Claude rider: extracted the config-root fallback chain
  into internal/appdirs (main.go bootSettingsDir + aocli now share it) —
  the packet's main*.go fence had correctly forced codex to mirror the
  chain; two live copies of a platform-subtle resolution invites drift.
  Follow-up folded into P1.4: `ao workflow validate --project` should load
  the project profile as def.Bindings now that P1.2 is merged.
- **P0.1 run2 reviewed + merged.** Registry + discussion migration reviewed
  (dispatch point exactly after triage.Handle; snapshot under RLock, invoke
  outside; idempotent unsubscribe with empty-bucket cleanup; inline `[4]`
  fast path; discussion logic + emitWireErrorToThread text byte-preserved;
  installDiscussionTurnObserver via sync.Once in NewApp with a defensive
  call in sessionEventHandler). One flag: its first final `make test-race`
  FAILED with the diagnostic lost to codex-side log truncation (honestly
  disclosed), passed on exact retry (626s). Adjudicated as environment
  flake, not a race: Claude ran an independent FULL `make test-race` in the
  lane — green end to end (root package 799s, no FAIL/DATA RACE lines),
  on top of codex's focused -race observer runs and its retry pass. Report
  at `reports/P0.1-report.md`.
- **Codex usage limit hit 2026-07-12 ~00:35 EDT** (resets 02:02). P2.1's
  first dispatch died at its first request — zero work consumed; lane p21
  stays bootstrapped + baselined (full go-build/go-test green) for a fresh
  dispatch after reset. P1.4 was mid-run at the time and kept working;
  if it dies on a subsequent request, RESUME its session (context is the
  asset — tree dirty, work standing) rather than fresh-dispatch.
- **P1.4 reviewed + merged — M1 complete.** Finished before the usage
  window closed. Scope exact; assertions extended, never weakened; content
  reviewed as product surface and rated strong (fenced untrusted seeds,
  envelope-aware endings, evidence-based diagnosis with bounded loops,
  dominance-correct loop targets, effect-aware Jira dedup). CI validity
  test enforces documented/used binding parity both directions +
  fenced-interpolation prompt contract; scaffold writes confined via
  os.Root + O_EXCL with rollback and a directory-swap escape test.
  Notable accepted assumptions: starter phases pin `gpt-5.6-sol` (schema
  requires a concrete model; maintenance note), scaffolds publish the
  embedded authoring schema beside the scope dir so the YAML `$schema`
  header resolves. Claude rider: a differing existing
  `workflow.schema.json` no longer fails the scaffold (it's an editor
  aid) — publication is skipped with a stderr note; test updated.
  Claude re-ran full go-build/go-test independently (81 packages ok)
  pre-rider + focused aocli/starters post-rider.
- **P2.3 reviewed + merged.** Faithful to every D2a verdict: claude
  session-sticky `--json-schema` via Config.OutputSchema (inline, flag
  coexistence tested), payload copied from `result.structured_output`
  (absent → nil, no synthesized error); codex per-turn `outputSchema`
  via SendOptions with pending-schema state bound to the turn id under
  either response/notification ordering (latch prevents double-bind;
  ordering tests both ways), final-agentMessage-wins retention where an
  invalid FINAL payload deletes an earlier valid one (nil → engine
  parks), turn-complete consumes state (leak-free), Close clears all.
  Provider layer transports bytes only — json.Valid syntax check, zero
  validation (engine authority preserved). Assumptions all sound (Event
  → ProviderEvent naming adaptation; agentMessage-only source;
  assistantMessage alias stays transcript-only). No rider needed.
  Claude re-ran full gates + focused provider -race independently:
  86 packages ok, no races. Report at `reports/P2.3-report.md`.
- **P2.1 reviewed + merged.** Migrations v22 (five workflow tables +
  usage_ledger work_item_id, D8-exact DDL with json_valid CHECKs +
  composite drain-order indexes) and v23 (threads.mode gains 'workflow'
  via rebuild preserving the CURRENT shape incl. claude-tui provider and
  max/ultra efforts). Bare CRUD only, no engine logic; transactional
  complete-set queue reorder with duplicate/foreign/partial rejection;
  idempotent effect recording preserving original payload; cursors
  CASCADE with automations (scheduler state, not run history — run-record
  tables keep zero FKs per D8 retention); token aggregate avoids Codex
  reasoning double-count. Assumptions all sound; pagination correctly
  declined as out-of-fence. Claude rider: QueryWorkItemUsage rejects ''
  (the unattributed marker — summing it would report the whole ledger as
  one item's spend) + test. Claude re-ran full go-build/go-test
  independently post-rider: green. Report at `reports/P2.1-report.md`.
- **Carried forward to P2.5 authoring (from P2.1 review):** per-item USD
  budgets cannot rely on `QueryWorkItemUsage.CostUSD` alone — Codex rows
  persist `cost_source='none'` with zero wire cost, so the reliability
  packet must compose `internal/usagecost` estimates for USD budget checks
  (tokens/wall-clock budgets unaffected).
- **P2.2 reviewed + merged.** Line-level review of every correctness core
  (evaluator, fsm, semaphores, drain, human, rebuild, engine loop) — all
  sound, no rider needed. D5-exact evaluator: ordered first-match with
  short-circuit, exhausted loop max falls through (traced in
  `ExhaustedLoops`), post-exhaustion list end → retries-exhausted vs plain
  no-match → wiring-error; `big.Rat` exact numeric comparison; strict
  decode (UseNumber + trailing-value rejection); JSON null optionals are
  absent from lookup, `exists` is the only presence-observing leaf. D9
  teardown is one function (stop → release → persist → transition → emit)
  and the sole releaseResources caller; crash sweep parks interrupted
  in-flight items; teardown-window terminal recovery replays persisted
  envelopes/traces. Live-profile semaphore capacity at acquire, canonical
  sorted all-or-nothing. Crash-idempotent human gates persist
  intervention + rewritten trace before transitioning. Claude re-ran full
  go-build/go-test + `go test -race ./internal/workflow/...`
  independently: 86 packages ok, engine race 36.5s, zero failures.
  Report at `reports/P2.2-report.md`.
- **Carried forward to P2.4 authoring (from P2.2 review):** (a) genuine
  seam gap — question-answer flow has no payload path (`Resume(itemID)`
  starts a fresh attempt; spec §7 wants the answer delivered as the next
  turn in the SAME provider session) — P2.4 must add an additive engine
  extension (answer-carrying Resume or a dedicated Answer command);
  (b) Runner.Stop must tolerate unknown RunKeys — the crash sweep calls
  Stop for runs the post-restart runner never started; (c) Resume and
  ResolveHumanGate error when global concurrency is full — the app layer
  must surface/retry, not drop; (d) Cancel only acts on running items —
  M3 needs a queued-item removal affordance; (e) engine.Enqueue creates
  the store row itself — bound methods pass params, not pre-created rows;
  (f) Settings gains WorkflowQueueActive/WorkflowConcurrency in P2.4.
  Degraded-but-correct (optional): loop feedback is rebuilt without the
  human note after a crash inside the human-decision window.
- **P2.4 BLOCKED at start (valid) → Scope amendment 1 → resumed.** Codex
  stopped clean at the def fence: phase prompts are sibling-relative file
  references, def validates them but exports no body loader, and the packet
  forbade both forking the confinement logic and editing def. Adjudication
  exposed the deeper hole — the frozen snapshot was freezing prompt PATHS,
  so a mid-run definition edit would mutate a running item's prompts (D8
  violation). Ruling: def gains exported `InlinePrompts(resolved)` (existing
  confinement + size rules; authored-form → runtime-form transition,
  documented as the only one); DefinitionSource inlines at item start so
  snapshots freeze bodies. Amendment committed (`d0763eaa`), lane
  fast-forwarded, session resumed at xhigh. Zero implementation work lost
  (blocked before any edit).
- **P2.5 packet authored** (`d4c446e3`) while P2.4 runs: watchdog / transient
  retry / budget as three buttons on the one teardown; runner-owned timers
  with new OutcomeStalled/OutcomeTransientExhausted mappings; engine-side
  budget check at attempt start behind a SpendSource seam composing
  usagecost estimates (the carried P2.1 finding); profile gains
  `reliability.backoff`; def gains per-phase `watchdog:` override; usage
  rows for phase threads get `work_item_id` stamped (budget prerequisite
  found while authoring — without it every budget sum reads zero).
  Dispatch gated on P2.4 merge.
- **P2.4 reviewed + merged.** No rider needed — first zero-rider app-layer
  packet. `InlinePrompts` amendment-exact (copies, existing confinement,
  element-naming errors). Engine Answer additive, mirroring
  resolveHumanGate's validation order; PriorThreadID forwarded/cleared like
  feedback; SetQueue concurrency atomic and bounded. Runner lock discipline
  verified (no order cycle; per-attempt sendMu serializes send vs Stop with
  recheck-after-acquire; single outcome delivery; unknown-key Stop no-op);
  its wire signals (fatal/expect_turn_complete meta,
  EventSessionStatus error/disconnected) all confirmed real. DefinitionSource
  dry-run-validates BEFORE inlining; workflow threads excluded from every
  user-facing listing (plain ListThreads feeds only harness internals);
  schema-less workflow spawn is a hard error; workflow threads skip generic
  auto-reconnect (engine owns continuation). Existing-test edits strengthen
  assertions only; codex added the crash-window answer-preservation test
  unprompted. Claude re-ran the FULL gate set independently (go-build,
  go-test, workflow -race 44.6s, frontend check/build, e2e 5/5): green.
  Report at `reports/P2.4-report.md`.
- **Carried forward from P2.4 review:** (a) a settings-UI workflow update
  resets a live process-N bound (UpdateSettings forwards maxStarts=0) — M3
  queue UI should either surface or re-apply the bound; (b) M3's take-over
  flow must route through the runner's schema registration — a free-form
  send to a parked workflow thread whose session died errors loudly today
  (correct but a UX dead-end until the takeover/finalize flow exists);
  (c) tool-driver and fan-out/join phases: runner rejects with a clear
  error → agent-error park; needs its own packet post-M2.
- **P2.5 reviewed + merged.** No rider — second consecutive zero-rider
  packet. Three triggers press the ONE teardown: budget checked at phase
  entry AND re-checked after resource waits; OutcomeStalled /
  OutcomeTransientExhausted map to existing typed reasons; SpendSource is a
  required engine seam. Runner: single timer slot per attempt with
  deadline-recheck-on-fire; watchdog armed on codex EventTurnStart / claude
  EventInit-or-successful-send (claude has no per-turn start signal);
  backoff-mode event suppression closes the stale-terminal window;
  process-death backoff starts only AFTER the app unregisters the dead
  session; Stop aborts backoff via the same detach path. Classifier is
  provider-scoped and closed (claude rate_limit, 529/ECONNRESET-precursor +
  server_error pairing, network_error terminal; codex tagged-union
  variants); ambiguous parks immediately. Attribution: triage gains a
  func-seam resolver (boundary intact, no workflow imports); aggregate
  wire-USD isolated from estimated rows; composition prices
  cost_source='none' groups via usagecost with LOUD failure on unknown
  models. Codex self-found and fixed six real races/holes pre-gate. Claude
  re-ran the full gate set independently plus -race over triage+store
  (392s/394s): green. Report at `reports/P2.5-report.md`.
- **Known nuance (accepted, from P2.5 review):** SpendSource returns one
  complete composition, so a TOKEN-budgeted item parks setup-failed if a
  cost_source='none' model has no usagecost rate — loud-over-silent wins,
  but adding a new model to the rate table unparks such items. Revisit only
  if it bites in practice.
- **P2.6 dispatched** into `~/repos/ao-lanes/p26` (branch
  `m2/p26-workspace-isolation`, base `cbcf8b63` = P2.5 merge). Lane
  frontend-bootstrapped; full pre-dispatch baseline green (go-build,
  go-test, frontend check, `make e2e` 5/5). Banner verified sol/**xhigh**,
  session `019f5635-3455-7483-9849-9a35a0805bb4`, no usage-limit DOA.
  Review inputs banked: artifact path deliberately deviates from D13's
  illustrative `runs/<run-id>/` (must appear in codex's report as a noted
  deviation); cleanup policy is plumbing-only — execution must NOT be
  implemented. Log: session scratchpad `p26-codex.log`.
- **P2.6 reviewed + merged** (`98855a55`) with one reviewer rider. Scope
  exact (38 files, no forbidden zones); gaming audit clean (existing-test
  edits were the additive stepMode param plus one strengthened assertion).
  Load-bearing assumption VERIFIED: v22 already reserved step_mode +
  worktree_path/branch/base_branch — the packet's v24 authorization rested
  on a wrong premise; codex answered with an honest no-op `SELECT 1` marker
  migration, and the rider dropped it (migration history records schema
  changes, not feature adoption) folding the column assertions into the
  v22 test. Line-level cores all sound: async Runner.Start futures (entered
  ack, keyed loop settlement, stale guards on item/phase/attempt/state —
  every cancel/complete race traced clean, futures always settle exactly
  once); provisioning state machine per packet (reuse requires all three
  fields + registered branch match, interrupted recovery on unique prefix,
  rollback via `git worktree remove --force` verified branch-retaining);
  ErrSetupFailed scoped to provisioning only; step-mode parks persist the
  genuine trace, approve executes exactly the recorded decision with
  completion persisted before intervention for crash safety, reject names
  alternatives verbatim; artifact confinement double-layered (EvalSymlinks
  containment + os.Root staged rename), capture failures never touch the
  outcome; D13 path deviation noted in the report as required; cleanup
  execution confirmed ABSENT (grep-proof). Bonus adjacent fix accepted:
  recoverTerminal rebuilds route feedback on advance/loop recovery, fixing
  a pre-existing P2.2-era crash-window feedback loss. Claude re-ran all six
  gates independently (build, go-test, workflow -race, frontend check,
  frontend build, e2e 5/5): green; rider re-verified with build + store
  suite. Report at `reports/P2.6-report.md`.
- **Carried forward from P2.6 review (M3):** workflow bound methods that
  trigger phase starts (WorkflowEnqueueItem, WorkflowSetQueue, resume,
  answer, gate-resolve) block until the provisioning of the starts they
  triggered settles — minutes-long RPCs for writing workflows with setup
  hooks. The future machinery makes fire-and-forget a trivial flip
  (stop attaching starts to command responses); decide the UI contract in
  M3. Also accepted interpretation: artifacts capture at the producing
  phase's successful completion (re-capture on loops), not at workflow
  end — self-consistent and loop-safe.
- **P2.7 dispatched** into `~/repos/ao-lanes/p27` (branch
  `m2/p27-harness-workflows`, base `a684bfa1` = post-P2.6 main). Lane
  frontend-bootstrapped; full pre-dispatch baseline green (go-build,
  go-test, frontend check, `make e2e` 5/5). Banner verified sol/**xhigh**,
  session `019f566e-6df8-7e71-9f2d-843f7b228a5d`, no usage-limit DOA.
  Review inputs banked: seeding must flow through production paths only
  (unreachable target = validation error, never raw rows); mock interrupt
  abort fixed once in the ao-mockprovider engine with parser-verified
  terminal frames for BOTH adapters; five zero-sleep Playwright specs;
  `make e2e` is the heart gate. Log: session scratchpad `p27-codex.log`.
- **P2.7 reviewed + merged** (`3faceb5f`). ZERO riders — third zero-rider
  packet of M2. All three banked review emphases verified line-level:
  (1) production-paths seeding — zero raw work-item/phase writes (grep +
  read), items via WorkflowEnqueueItem only, definitions/profile/prompts
  written at the exact production config-dir layout and validated through
  the production workflowDefinitionSource.Resolve, driven targets through
  the real scheduler with subscribe-before-activate event waiting;
  (2) interrupt frames — single engine-owned abort in ao-mockprovider
  (turn-keyed gates, abort checks at every step boundary, ack-before-
  terminal ordering by goroutine handoff), and BOTH binary tests push the
  emitted frames through the real app parsers (claude.Parser with
  MarkInterruptAcked; codex.ClassifyNotification) asserting
  Aborted/StopReason:"interrupted" normalization — the strongest check
  short of a live CLI; (3) all five Playwright specs are event-await-only
  (no sleep/poll), including same-mockId question continuation and 100ms
  watchdog. Reset drains queued items via start-then-cancel with a
  must-make-progress loop that tolerates setup-failure parks. Deliberate
  behavior change accepted: afterTurns:"silent" now stays owned until
  interrupt (a hung turn, not an empty completion) — packet scope.
  Sensible adjudications logged in ASSUMPTIONS (exhausted transient
  maxStarts refused loudly; queued targets require inactive caller
  queue). Claude re-ran all six gates independently: green, e2e 10/10.
  Report at `reports/P2.7-report.md`.
- **M2 CLOSE-OUT: complete.** All seven packets merged (P2.1–P2.7).
  Integrated-main verification green: make go-build, make go-test,
  frontend check, frontend build, make e2e (10/10), and full
  make test-race (root 739s within the 1800s budget, triage 393s).
  The engine now runs items end-to-end against real providers with
  reliability controls and workspace isolation, fully covered by the
  deterministic harness: enqueue → worktree provisioning + setup hooks →
  schema-enforced phase turns → gates (auto/human/step) → question parks
  answerable on-session → watchdog/transient-retry/budget parks →
  artifacts → crash sweep. Next: M3 boundary items — P0.3 rev2 brief,
  UI packets (carry-forwards logged above), post-M2 tool-driver +
  fan-out/join packet.
- **P0.3 rev2 authored** (`a7f37cc5`) at the M3 boundary, superseding the
  parked rev1. Scope per the mid-campaign user rulings: in-process
  notifications service (async authorization — startup never blocks on the
  180s macOS grant), Windows launcher↔backend bridge over the EXISTING
  transport (backend notifyOS publishes `notification:send`; the launcher —
  which already holds {port, token} from the bootstrap line — subscribes
  via a minimal WS client, presents, and posts activations back through a
  new LocalOnly `NotificationActivated` RPC; backend emits
  `notification:activated` to the SPA on both paths), bounded frontend
  pending-activation queue (cap 8) draining post-hydration with a vitest
  test, HarnessNotify + e2e per rev1. rev1 partial kept as reference only
  (predates slugs/M1/M2). New gates dry-run green on base: GOOS=windows
  full-repo cross-build; frontend vitest (362 files / 5236 tests).
- **Wails fork fix executed (Claude-owned rev2 prerequisite).** Linux
  `NotificationClosed` reason 2 no longer synthesizes a
  DefaultActionIdentifier response (dismiss ≠ click); map cleanup retained
  for all close reasons. Verified: build + vet + package tests green.
  Pushed to the pinned branch (`ao-webview2-dpi-hardening` @ `b7d0bcc96`)
  and cherry-picked cleanly onto an upstream-candidate branch
  (`fix/linux-notification-dismiss-vs-click` off wailsapp/wails master —
  bug confirmed still present at alpha2.117). **NO upstream PR opened;
  standing constraint holds: PR only with explicit user approval.**
  agent-overflow pin bumped (`a63891ac`), go-build + go-test green.
- **P0.3 rev2 dispatched** into `~/repos/ao-lanes/p03r2` (branch
  `m3/p03r2-os-notifications`, base `653514c6` = post-pin-bump main).
  Lane frontend-bootstrapped; full pre-dispatch baseline green including
  the packet's two new gates (GOOS=windows full cross-build; frontend
  vitest 362/5236) plus go-build, go-test, frontend check, e2e 10/10.
  Banner verified sol/**xhigh**, session
  `019f56ae-e129-7612-be10-c9c677757ed9`, no usage-limit DOA. Review
  inputs banked: dismissal must never emit `notification:activated`
  (fork fix consumed, not re-implemented); launcher WS client must be
  unit-tested against a stub transport server; `NotificationActivated`
  classified LocalOnly; pending-activation queue cap 8 with vitest;
  OS-presentation surfaces stated as explicitly unverified. Log: session
  scratchpad `p03r2-codex.log`.
- **P0.3 rev2 verdict: ACCEPT, merged `6cb823f4`.** All seven gates re-run
  independently over the post-rider tree: go-build, go-test, GOOS=windows
  full cross-build, frontend check, vitest (363 files / 5243 tests),
  frontend build (zero dynamic-import warnings), e2e 11/11 including the
  new notifications spec. Every banked review input verified in code:
  dismissal cannot emit `notification:activated` (default-action-only
  filter on both native paths; the desktop test asserts against the
  literal macOS dismiss identifier); launcher WS client unit-tested
  against a stub transport server (token auth + raw/URL-escaped
  redaction, subscribe shape, replay checkpoints across reconnects,
  activation RPC round-trip, sequence-gap checkpoint preservation);
  `NotificationActivated` classified LocalOnly; pending-activation queue
  cap 8 with vitest; OS presentation stated explicitly unverified. Scope
  clean: the `internal/transport/` breadth is the minimal principled
  mechanism the bridge needs — bounded opt-in `subscribe` frame,
  per-subscriber channel filters (no subscribe → all visible channels,
  SPA contract untouched), replay-completion marker (replay and live pump
  share writeMu, so strict-order consumers need a buffer boundary),
  loopback-only visibility for both notification channels. go.mod delta
  is the known indirect go-toast dep (accepted in rev1 review). All seven
  report assumptions accepted; notably assumption 2 (headless+transport
  publishes into the ring instead of erroring when no launcher subscriber
  exists) is the better design — ring replay delivers to a
  late-connecting launcher, so a zero-subscriber moment is not a failure
  — and assumption 5 (no activation RPC retry after a write attempt)
  preserves one-click-≤-one-event. Riders (Claude): (1) new
  `internal/notify` package deduplicating the wire contract codex had
  copied into `internal/wsllauncher` (the launcher can't import main; a
  shared internal package is the right fix — validator drift across the
  process boundary would silently drop notifications); bindings
  regenerated; (2) pure `notificationActivationQueue.ts` factory split
  out of `eventsNotification.ts`, replacing assumption 7's dynamic-import
  isolation with static imports + call-time closures — the first vitest
  gate run surfaced the real constraint (mock proxies throw on eval-time
  access to exports a partial vi.mock factory omits, via
  TerminalView.test.ts), and closures defer the access to activation
  time while killing both INEFFECTIVE_DYNAMIC_IMPORT warnings; (3)
  e2e/AGENTS.md documents notifications.spec.ts. Report at
  `reports/P0.3-rev2-report.md`. Remaining user-assisted items: real
  Windows toast click-through (incl. cold `-Embedding` launch), real
  macOS presentation/authorization, real Linux desktop click/dismiss.
  Lane pruned.
- **P3.1 authored (`c3a34919`) + dispatched** into `~/repos/ao-lanes/p31`
  (branch `m3/p31-thread-modes`, base `c3a34919`). Brief binds: one
  append-only migration (threads mode CHECK + `work_items.triage_thread_id`);
  `internal/threadmode` owns the hidden-modes set consumed by all backend
  listing exclusions; take-over = send-path entry (interrupt → detach →
  schema-less free-form turns) parking at needs-human(taken-over) with a
  new typed reason; `WorkflowCompleteTakeover` finalize turn re-attaches
  the phase schema and routes the envelope through the normal gate path
  (failed finalize re-parks taken-over, not agent-error); discard+re-run
  asserts existing `WorkflowResumeItem` from the taken-over park;
  `WorkflowOpenTriageThread(itemID)` open-or-return with no-internals
  seeding + immediate kickoff; `WorkflowOpenTriageAgent(projectID)`
  singleton shell (D4.9 tools deferred to M4); P2.4 carry-forward (a)
  fixed (settings updates may not clear a live process-N bound).
  Forbidden: provider/transport/triage internals, disposition, P3.3
  surface, frontend src. Gates add workflow+threadmode+store -race and
  GOOS=windows cross-build; full lane baseline green pre-dispatch
  (8/8 including e2e 11/11). Run1 died on arrival at the codex usage
  limit (window reset 12:05 PM); run2 banner-verified sol/**xhigh**,
  session `019f5714-98e5-77f2-8d0a-cc289a6bf374`, no usage-limit errors.
  Log: session scratchpad `p31-codex-run2.log`.
- **P3.1 BLOCKED (valid, packet authoring gap) → Scope Amendment 1 →
  resumed.** Codex blocked pre-edit, baselines all green: the brief's
  forbidden zone allowed only `methods_gen.go` regeneration under
  `internal/transport/`, but every new bound App method must be
  classified in `internalmethods.go` and the exhaustive classification
  test enforces it — an honest implementation path did not exist. Codex
  refused to misclassify or weaken the test (exactly right). Amendment
  ruling: all three new RPCs join the existing Workflow* LocalOnly block
  (they drive provider sessions + local worktree state); existing
  transport test fixtures (privilegedApp stubs) may be extended exactly
  as the classification tests require; test expectations stay untouched.
  Session resumed in place (context is the asset — specs read, baseline
  done): same id `019f5714-98e5-77f2-8d0a-cc289a6bf374`, xhigh restated,
  log `p31-codex-resume.log`.

- **Campaign moved off `main` onto `workflows-system` (user request,
  2026-07-12).** The user pushed trunk changes from another machine
  (PTY normalization `6e8f8dc4`, codex v2 collab cleanup `3c22d149`,
  macOS packaging `b726e877`) and asked that main be reset and all
  workflow work continue on a branch. Local `main` (49 campaign
  commits) was branched as `workflows-system`, `main` hard-reset to
  `origin/main`, and origin merged into the branch (`9d59a1b2`).
  **Migration collision found and resolved:** trunk's new v21
  (`trim_codex_v2_encrypted_collab_prompts`) collided with the
  campaign's unpublished v21–v23. Trunk numbering wins (renumbering the
  published side would skip DDL on the user's other machine's DB):
  project_slugs 21→22, workflow_persistence 22→23, thread_workflow_mode
  23→24; tests + schema docs renumbered; full `make go-test` green on
  the merged branch. All future packet merges target `workflows-system`;
  pushing/PRing the branch is the user's call.
- **P3.1 reviewed + merged `3ee937f8` with three riders.** Report
  (24k, high quality) verified; diff split five ways (store / engine /
  take-over core / triage / modes+transport) and line-level reviewed.
  Highlights confirmed: v25 migration derivation is drift-proof and
  preserves both pre-existing work_items indexes; `takeoverFinalize`
  lives only on the tracked runtime item so every park evicts it
  (resume-is-fresh is structural); intervention-persist failure still
  tears down resources (test-proven); `StopForTakeover` restores the
  attempt + backoff timer on failed yield; interrupt path takes no
  thread lock (live-interrupt integration test would deadlock
  otherwise); triage seeding is injection-hardened (ASCII-quoted,
  delimiter-escaped, rune-bounded) and provably excludes gate traces;
  transport delta is exactly Scope Amendment 1's allowance. Gaming
  audit: all existing-test changes additive. Riders: (1) lane migration
  renumbered v24→v25 atop the trunk merge; (2) reverted a cosmetic
  `''`→`”` comment mangle in migrate_test.go; (3) fixed a real stuck
  state — a failed Claude schema-restart during finalize left
  `takeovers[thread].transitioning=true` forever, rejecting all further
  steering sends until app restart; Start now cancels the transition on
  error (regression `TestFailedFinalizeStartClearsTakeoverTransition`).
  Accepted as documented assumptions: Complete fails fast at full
  concurrency; 15s bounded engine-loop wait for takeover yield.
  Independent full gate battery re-run post-merge on `workflows-system`.

- **P3.2 authored (combined P3.2+P3.3 per the approved larger-chunk
  cadence) + dispatched** into `~/repos/ao-lanes/p32` (branch
  `m3/p32-disposition-uidata`, base `dc591c52` on `workflows-system`).
  Scope: disposition D3.3 (WorkflowMergeItem / WorkflowCreateItemPR /
  WorkflowDiscardItem via internal/git + forge layer; refusal parks
  needs-human(disposition); receipts JSON on the item; auto-pr/auto-merge
  policy shares the manual core; D6 cleanup gating); migration v26 (ADD
  COLUMN disposition + digest, rebuild fallback if CHECK-on-ADD fails);
  D4.3 digests (synchronous deterministic template at park — never
  blocks — async textgen upgrade via threadtitle/commitmsg precedent);
  notification senders on the app emit seam through the P0.3 pipe
  (per-item park/fail + coalesced drain summary; done-awaiting-
  disposition rides the summary only; notify.Target gains workflow-item /
  workflow-triage-agent kinds, additive, launcher-opaque); D12 notes
  RPCs over automations.notes; WorkflowListDefinitions intake listing
  (dry-run validity + first error + profile cross-check + predicted
  queue position); WorkflowRemoveQueuedItem (queued-only, loses politely
  to a racing start); fire-and-forget enqueue (P2.6 carry-forward —
  synchronous validation stays, only triggered starts detach; resume/
  answer/gate-resolve contracts unchanged). All new RPCs pre-classified
  LocalOnly in the brief (P3.1 BLOCKED lesson baked in). Lane
  bootstrapped (pnpm frontend+e2e, frontend dist built for the go:embed,
  lane make go-build green). Banner verified sol/**xhigh**, session
  `019f576c-444e-70b3-9bd5-0afadd50c1de`. Review emphases banked: merge
  refusal paths (dirty base / conflict / missing branch) must be
  test-proven and receipt-less; no forced git flags anywhere (grep
  --force / checkout -f); digest template must not leak internals
  (variables/gate traces); Target extension round-trip through
  TargetToMap/FromMap + launcher opacity; fire-and-forget must not break
  engine Sync() test semantics; drain-summary tally reset on restart is
  an accepted documented assumption. Log: session scratchpad
  `p32-codex.log`.

- **P3.2 BLOCKED (valid, packet authoring gap) → Scope Amendment 1 →
  resumed.** Codex implemented the fire-and-forget contract, found
  `TestWorkflowHookFailureAndTimeoutParkSetupFailed` asserting the OLD
  inline-provisioning-error contract of `WorkflowSetQueue`, and stopped
  rather than weaken a test, add a shim, or revert decision 11 — exactly
  right (all seven pre-edit baselines had passed; a harness-reset race it
  found on the way was fixed via the preserved `Engine.Sync()` contract,
  no expectations touched). Amendment ruling: tests asserting the
  obsolete synchronous contract may be UPDATED to assert the new one at
  equal-or-stronger rigor (park to needs-human(setup-failed) + event
  surfacing both proven; synchronous validation errors still inline);
  one named contract change, anti-gaming rules otherwise unchanged.
  Session resumed in place (context is the asset): same id
  `019f576c-444e-70b3-9bd5-0afadd50c1de`, xhigh restated, log
  `p32-codex-resume.log`.

- **P3.2 verdict: MERGED `6d329261` (workflows-system).** Review split
  five ways (git / disposition / notifications+digest / data+engine /
  store+transport), line-level on the correctness cores. Git merge
  machinery CLEAN: revision-expression rejection (`main~0` refused),
  double clean-tree checks, exact-branch verification post-checkout,
  non-mutating conflict preflight, no force flags anywhere, refusal
  tests assert non-mutation (HEAD + branch + untracked file unchanged).
  Disposition STRONG: receipt-or-park exhaustive; manual/auto race
  returns the existing receipt as success; cleanup:auto only after
  durable receipt; auto worker serialized + drained at shutdown. The
  no-retry-merge-from-disposition-park question resolved against
  UI-SPEC §5.3 — spec defines no such path (triage thread → discard /
  re-enqueue); accepted gap, not a rider. Notifications/digest CLEAN:
  template digest persisted synchronously BEFORE event publication;
  bounded async upgrade (2 slots, staleness = state+reason+digest
  bytes); heavy work all off the engine loop. Scope Amendment 1 gaming
  audit: NOT GAMED — strictly stronger (park + both events with typed
  cause + inline-validation probe retained; engine test covers all
  three detached surfaces + preserved Sync()). Store/transport: v26
  additive ADD COLUMN + JSON CHECK proven both ways; predicted queue
  position is a global rank matching the engine's drain order; all 7
  RPCs LocalOnly-classified. ONE rider: reverted a re-introduced
  `''`→`”` smart-quote mangle in migrate_test.go — same line as the
  P3.1 rider; codex's editor smart-quotes comments in files it touches;
  **standing review check: sweep every codex diff for curly quotes in
  Go files**. Independent 7-gate battery green on the merged branch
  (report relocated to `reports/P3.2-report.md` with rider addendum).
  Lane p32 pruned.

- **origin/main merged again** (`8b4233e2`): user's
  `e8d658b5 fix(codex): reconcile completed v2 agents` — clean merge,
  go-build + go-test re-verified. (Recovery note: a `git stash` /
  `stash pop` pair around the merge popped a PRE-EXISTING user stash
  (`settle-flicker` on main) because the stash step had nothing to
  save; the pop conflict was reverted file-for-file and the user's
  stash entry is intact. Protocol fixed: never pop without confirming
  the entry is ours.)

- **P3.4+P3.5 authored (combined per the approved larger-chunk
  cadence):** `P3.4-5-workflows-pane-frontend.md` — the complete
  workflows pane (skeleton + read-only levels per UI-SPEC §§2–5/§11,
  then actions/sweep/intake per §5.3/§5.4/§7/§9). Backend preamble
  bounded to four items the UI can't render without: P1 usage on
  WorkflowGetItem, P2 WorkflowListItemCosts grouped query (NOT
  LocalOnly — remote view-only reads it), P3 WorkflowListDefinitions →
  WorkflowDefinitionCatalog wrapper (baseBranch + hoisted
  predictedQueuePosition + per-listing typed `inputs` for §7.1 seeds;
  API-shape ruling: zero consumers yet, P3.2 shape tests updated at
  equal rigor), P4 WorkflowOpenStudioThread create-or-resume (the
  §3.1 "+ New workflow" / §4 Edit affordances had NO backend — gap
  found during authoring). Key bindings: queue toggle uses the
  settings path (WorkflowSetQueue would clobber process-N); one
  R1 classifier module table-tested like threadStatusPill; diff
  evidence via GetBranchBaseDiff + utils/diff.ts (ReviewPane is
  P3.6); chat confirm card ships prefill-capable but unmounted if no
  clean timeline seam exists (M4 owns the producer); exclusions
  (sidebar/footer/pickers/deep-links/remote gating) are P3.6.

- **P3.4+P3.5 dispatched** into `~/repos/ao-lanes/p345` (branch
  `m3/p345-workflows-pane`, base `c5fb1bc6` on `workflows-system` —
  includes the `8b4233e2` origin/main merge). Lane bootstrapped (pnpm
  frontend+e2e, frontend dist built for the go:embed, lane
  `make go-build` green). Banner verified sol/**xhigh**, session
  `019f57c7-df07-70c3-b29f-0af4eb40af07`. Review emphases banked:
  backend preamble stays inside its four named items (P1–P4) — any
  drift beyond them is a scope violation, not initiative; the P3.2
  listing-shape test update must keep every store/validation assertion
  at equal rigor (only the wrapper shape changes); R1 classifier must
  be table-tested incl. done-awaiting-disposition neutrality; R2 grep
  the diff for snapshot/envelope/gateTrace renders and the word
  "variables" in copy; queue toggle must NOT call WorkflowSetQueue
  (process-N clobber); singleton pane reuse must be test-proven;
  sweep order/wrap/skip semantics per §5.4; curly-quote sweep on Go
  files (P3.1/P3.2 recurrence). Log: session scratchpad
  `p345-codex.log`.

- **P3.4+P3.5 BLOCKED (valid, packet authoring gap) → Scope Amendment 1
  → resumed.** Codex ran all eight baselines green, touched zero
  implementation files, and stopped on a real contradiction I authored:
  preamble P4 mandates `WorkflowOpenStudioThread` be LocalOnly, but the
  forbidden-zones allowlist says "NOTHING else" and omits
  `internal/transport/internalmethods.go`. Amendment ruling (appended to
  repo + lane packet copies, committed `5af736c0`): internalmethods.go
  amendable for exactly that one addition; P4 may live in a new
  `app_workflow_studio.go` + test; extending `app_bindings_test.go`'s
  registered-methods list is allowed (pre-empting two likely re-bounces
  spotted during the ruling). Session resumed in place, xhigh restated,
  log `p345-codex-resume.log`. Third packet running with a BLOCKED on a
  Claude-side allowlist gap (P3.1 transport, P3.2 obsolete-contract
  test, now this) — authoring lesson: when a preamble names a
  cross-cutting requirement (classification, registration test), walk
  the allowlist against each requirement's file touch-set before
  dispatch.

- **P3.4+P3.5 second BLOCKED (valid) → Scope Amendment 2 → resumed.**
  Codex found the transport classification gate requires every new
  bound method in exactly one of LocalOnlyMethods / `wireSafeMethods`
  (`methods_gen_test.go`), and P2's NOT-LocalOnly directive pointed at
  a test file no ruling permitted. Adjudication REVERSED P2 instead of
  granting the requested wireSafeMethods edit: P2's premise was wrong —
  every existing workflow read (WorkflowListItems / WorkflowGetItem /
  WorkflowListDefinitions) is LocalOnly today, and a lone wire-safe
  cost map serves no remote renderer when the item lists it annotates
  are refused. WorkflowListItemCosts goes LocalOnly with its siblings
  (Amendment 1's internalmethods.go allowance widens to exactly two
  additions); methods_gen_test.go untouched. **Recorded for the P3.6
  brief: §12 remote view-only requires re-classifying the workflow
  read set as one coherent decision** — today's all-LocalOnly state
  means remote renders no workflows, acceptable until P3.6. Committed
  `2132c5cf`; resume log `p345-codex-resume2.log`, xhigh restated.

- **P3.4+P3.5 third BLOCKED (valid, mid-implementation) → Scope
  Amendment 3 → resumed.** Codex had finished the preamble (gates 1–4
  green) and the frontend work plan, then caught in its own post-task
  review that `WorkflowListItems` summaries blank `disposition` —
  making resolved done runs indistinguishable from
  done-awaiting-disposition, which would wrongly resurrect merged/
  discarded runs into sweep counts after every reload. It correctly
  refused all three dishonest outs (contract change outside allowlist,
  unbounded detail hydration, ship-it-wrong-as-assumption). Ruling
  adopted its smallest-resolution proposal, upgraded from a boolean to
  the full receipt: `workItemSummaryColumns` selects the real
  `disposition` column (compact schema-checked JSON, needed for §4.1
  history receipts anyway); digest stays detail-only; blanking-contract
  tests updatable at equal rigor; frontend treats done+empty as
  awaiting disposition. Committed `0a7617ed`; resume log
  `p345-codex-resume3.log`, xhigh restated. Notably the run is
  self-auditing against the packet's binding decisions before its gate
  battery — the 907k-token session found this, not my review.
