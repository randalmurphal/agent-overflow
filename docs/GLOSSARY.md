# Glossary

Coined and repurposed vocabulary in this repo. Use it when a doc or a review
uses a term without defining it. Every entry names its authoritative source;
if this file and the source drift, the source wins. Definitions here are
one-liners, not specs.

When you coin a term that will appear in more than one doc, add it here in
the same change.

## Terms with more than one meaning

Read this section first. These words mean different things in different
subsystems, and an unqualified use is a real trap.

| Term | Meanings |
|---|---|
| **wave** | (a) campaign wave: one traversal of a campaign spine's graph, one child run along the tail-self-call edge (`docs/architecture/workflow-campaigns.md`). (b) project workstream phase: a chunk of a planned effort, e.g. "wave 2 of the cold-thread-loading work" (`docs/architecture/thread-replica-sync.md`). |
| **lane** | (a) campaign task lane: the second starter workflow a campaign spine calls per task (`workflow-campaigns.md`). (b) run-map fan lane: one branch column of a fan-out phase in the UI (`docs/architecture/workflow-run-map.md` §2). |
| **spine** | (a) campaign spine: the self-calling workflow definition (`workflow-campaigns.md`). (b) run-map spine: the vertical CSS connector line, `.run-map-spine` (`docs/architecture/workflow-run-map.md` §2). (c) dispatch spine: `internal/triage/router.go` as the switch every concern hangs off (`docs/architecture/conventions.md`). |
| **run** | (a) workflow run: one execution of a workflow definition, which IS a work-item row (`docs/specs/workflows-system.md` §3a). (b) activity run: a collapsed stretch of consecutive activity rows in the chat timeline, with its own `runId` (`docs/architecture/activity-runs.md`). |
| **item** | (a) work item: a workflow run (`workflows-system.md` §2). (b) timeline item: a `store.Item` row with `item_index` (`docs/architecture/invariants.md` §1). Both docs say "the item" with no qualifier. |
| **ghost** | (a) run-map ghost: a not-yet-reached future phase, rendered as a bare line with no box (`docs/architecture/workflow-run-map.md` §2). (b) Codex ghost row: a persisted background tool_call orphaned by a dead subprocess, flipped to `errored`/`lost` at session start (`internal/codexghost/AGENTS.md`). (c) ghost-text: the Claude TUI's inline completion hint (`docs/architecture/claude-tui-provider.md`). |
| **profile** | (a) project profile: the workflow bindings `profile.yaml` (`workflows-system.md` §8). (b) instance profile: `--profile harness\|soak`, the value every per-instance isolation axis derives from (`docs/architecture/soak-rig.md`). (c) chat model profile: the provider/model/effort/fast-mode tuple on a thread (`internal/chatmodel/AGENTS.md`). |
| **envelope** | (a) control envelope: a phase's structured result (`workflows-system.md` §3). (b) input envelope: the persisted `input_envelope` a held start renders from at release (`internal/workflow/engine/AGENTS.md`). (c) budget envelope: the root item's token/USD/wall-clock ceiling enforced across the tree (`workflows-system.md` §3a). (d) replica envelope: a `SyncThreadWindow` response carrying an attested stamp (`docs/architecture/thread-replica-sync.md`). |
| **sweep** | (a) needs-attention sweep: the human `j`/`k` triage pass over parked runs (`workflows-system.md` §7). (b) crash sweep: startup reconciliation failing rows still claiming `running` (`internal/workflow/engine/AGENTS.md`). (c) retention sweep: age-based log pruning, also `orphanreaper.Sweep` (`internal/AGENTS.md`). |
| **lease** | (a) scroll lease: depth-counted auto-scroll suspension (`docs/architecture/scroll-rearchitecture-inventories.md`). (b) input lease: the take-control arbitration token on a Claude-TUI terminal (`docs/architecture/claude-tui-provider.md`). (c) expansion lease: an activity-run row's keep-me-mounted claim while expanded (`activity-runs.md`). |
| **element** | (a) workflow element: any phase, fan-out unit, or join that runs a turn or command (`internal/workflow/runner/AGENTS.md`). (b) array element: the `over:`/`as:` fan-out binding value (`workflows-system.md` §3). Plus the DOM sense throughout the frontend. |
| **oracle** | (a) standing oracle: an in-app diagnostic observer that must stay silent to prove a bug class is gone (`docs/architecture/settle-flicker-analysis.md`). (b) port-scan oracle: the dev-server probe's yes/no answer about a live listener (`app_dev_server_probe.go`). |
| **snapshot** | (a) workflow frozen definition: the resolved graph + bindings pinned into a run record at start (`workflows-system.md`). (b) bundle DB snapshot: the `VACUUM INTO` copy a replay bundle restores (`agent-harness.md` § Record / replay). (c) semantic viewport snapshot: the harness bridge's mounted-rows answer (`agent-harness.md` § Frontend bridge). (d) scroll snapshot: a pane's saved scroll state for cache restore (`threadScrollSnapshots.ts`). |
| **triage** | (a) the Go package: provider-event classification (`internal/triage/AGENTS.md`). (b) thread mode: the `workflow-triage` value of `threads.mode` (`docs/architecture/schema.md`). (c) trace triage: the ui-trace bug-report investigation workflow (`.claude/skills/trace-triage/`). |
| **projection** | (a) wire projection: the bounded copy of a timeline item a client receives, with a marker naming what was removed (`internal/itemwire/AGENTS.md`). (b) event projection: `internal/app` mapping an application service's events onto the wire channels. (c) live projection: a read-model-ish view a service exposes to the frontend (discussion/worktree activity projections, `internal/AGENTS.md`). Only (a) removes anything. |
| **elision** | Always the wire projection's leaf drop from `meta.input` — structure-preserving, marker-carrying, never a string truncation (`internal/itemwire`). Not to be confused with the render-time ellipsis the tool-card previews apply, which is a display cap on a value that arrived whole. |
| **chokepoint** | Not conflicting, but it names at least four different single-writer points: `writeScrollTop` (scroll), `persistItem` (Go store+emit), `apply()` (entity stores), `replaceTimelineItems` (pane items). An unqualified "the chokepoint" is ambiguous outside its file. |

## Project, workspace, worktree

The one distinction this repo redefines outright. Source: root `AGENTS.md`
Core Principle 7.

| Term | Definition |
|---|---|
| **project** | The git repo (the main root, via `--git-common-dir` semantics). A linked worktree resolves to the repository it was cut from, not to itself. |
| **workspace** | Where the provider operates: the project root or a separate worktree. Threads track both. Git status, MCP listing, and the workspace-change lock are workspace-keyed, never thread-keyed. |
| **sub-worktree** | The per-unit checkout cut from a work item's branch for a writing fan-out unit; the branch name encodes item/phase/attempt/unit/try so a retry never inherits a failed try's tree (`workflows-system.md` §9). |
| **worktree setup** | The per-project recipe (copy globs + argv commands) run at worktree creation. Project app settings, not the workflow profile, so chat and workflow worktrees share it (`internal/worktreesetup/AGENTS.md`). |

## Workflows engine

Source: `docs/specs/workflows-system.md` unless noted.

| Term | Definition |
|---|---|
| **work item** | A goal bound to a project, driven through a workflow. States: `running → needs-human → done / failed / cancelled`. There is deliberately no `queued` state; contention is a phase waiting on a resource, never an item in a line. |
| **phase** | A configurable unit of work with a driver (`agent` = provider+model, or `tool` = a profile-bound argv command), an `access:` declaration, inputs, and outputs. Not "an agent". |
| **unit** | One parallel branch of a fan-out phase, bound to exactly one of `prompt:`, `command:`, or `call:`. A writing unit gets its own sub-worktree; the unit IS the isolation boundary. |
| **join** | The unit that runs after all units rest; its envelope is the phase's envelope. Synthesis join (reads N attempts at one goal) vs merge join (git-merges N disjoint branches). A join may never be a `call:`. |
| **control envelope** | The system-owned structured result every phase emits: flat, generated schema, closed status set `done \| question \| stuck`, declared outputs nested under `outputs`. Engine post-validation is the sole success authority. |
| **narrative** | The agent's free prose about what it did, written to a file at a system-dictated path, never an envelope field. |
| **gate** | The ordered predicate set after a phase that reads its outputs and routes. `passed: false` from a tool phase is not a phase failure; the gate decides what a red check means. |
| **seed** | The initial variable context an item starts with. `amend --seed k=v` durably changes it on a resting run. |
| **grant** | The set of `ao` CLI subcommands a phase's injected session credential authorizes. Structure is declared; agency is granted. |
| **park** | A run stopping at `needs-human` with a typed reason (`gate`, `question`, `stuck`, `interrupted`, `budget-exhausted`, ...). Never a silent dead end. |
| **resting state** | Any of done / failed / cancelled / needs-human; the transition set that fires a wake and a notification. |
| **wake** | The compact message a resting root run injects into its bound thread via the queued-user-message path. Deduplicated by content signature, never a time window (`internal/workflow/wake/AGENTS.md`). |
| **bound thread** | The chat thread a root run records so its resting states wake that conversation. Child runs never bind and never notify. |
| **disposition** | The in-app landing of a done item's branch: merge / PR / discard (`manual \| auto-pr \| auto-merge`). The only place the project's base branch is touched. |
| **project profile** | Per-project `profile.yaml` supplying the concrete bindings a portable workflow references by name. Workflow declares; project binds (§8). |
| **snapshot / frozen definition** | The resolved graph + bindings pinned into the run record at start; the run executes from that, never live config. |
| **step mode** | Per-item intake option making every gate behave as a human gate. The trust-building mode for a new workflow. |
| **soft-stop** | Deferred pause: the current wave finishes, nothing in flight is interrupted, the run parks `needs-human(checkpoint)`. |
| **guidance** | `run guide <id> "<text>"`: a pending-guidance slot delivered at the run's next fresh phase entry only, never mid-turn. |
| **takeover** | Sending a message into a phase's thread interrupts the turn, detaches the attempt, and parks `needs-human(taken-over)`. The send is the takeover; there is no button. |
| **campaign memory** | Append-only NDJSON notes keyed by the run tree's root, closed kind vocabulary `pattern \| warning \| learning \| handoff` (`internal/workflow/memory/AGENTS.md`). |
| **`history.<phase>`** | Reserved phase input giving prior attempts of a phase oldest-first. Prompt surface only; gates cannot reference it. |
| **`accounts_for_units`** | A join opt-in requiring `merged[]` ∪ `blocked[]` to exactly equal the unit set; the engine refuses a `done` envelope otherwise. |
| **teardown** | The single engine path that releases resource holders and the only caller of `Runner.Stop`. Every exit shape uses it (`internal/workflow/engine/AGENTS.md`). |
| **`provider:<name>` resource** | The implicit semaphore every agent-driver element acquires; the bound on concurrent CLI sessions. A fan-out phase takes none (it would deadlock against its own units). |

## Campaigns and the run map

| Term | Definition |
|---|---|
| **campaign** | One root run of a recursive spine whose last phase calls itself; the tree pauses/stops/discards as a unit. Loop until dry, not for N waves (`docs/architecture/workflow-campaigns.md`). |
| **wave chain** | The root → child → grandchild chain along tail self-calls; wave ordinal = position in the chain. Everything else renders as composition (`docs/architecture/workflow-run-map.md` §3). |
| **lap** | The run-map's unit of the wave chain: one wave's segment. A lap can hold two waves when a tail call was retried (`frontend/src/lib/utils/workflowRunMapTypes.ts`). |
| **composition** | A called run that is not a tail self-call, rendered as a chain inside its parent's node, recursively. Collapses by default at every depth. |
| **frontier** | The leaves with status `running` or parked causes, needs-human first. The frontier path decides folding, never depth (`docs/architecture/workflow-run-map.md` §5). |
| **settled (wave)** | Not live, or has tail children. A wave that called the next lap is still `running` in the engine, so folding on run state alone would keep every ancestor expanded. |
| **fan** | A fan-out phase's parallel branch columns that fork and rejoin in the run map. A folded lane carries `chain: []`; collapsed means not built, everywhere. |
| **R1 / two hues** | The overlay's color law: amber = a human is blocked, red = failed, everything else neutral. One decider module; never inline a hue (`frontend/AGENTS.md`). |
| **R2 / no internals** | No envelopes, JSON, schemas, gate traces, or the word "variables" on any human surface. |

## Frontend scroll and timeline

Source: `docs/architecture/frontend-scroll.md`, `docs/architecture/scroll-contracts.md`, `frontend/src/lib/utils/scroll/`.

| Term | Definition |
|---|---|
| **stick / escape / re-stick** | The sticky-bottom intent model. Any upward user input escapes synchronously; re-stick only via input-backed scroll near bottom, explicit `forceStick`, or wheel-down while clamped. Intent is event-sourced, never geometry-inferred. |
| **spring chase** | The velocity-spring animation the viewport uses to follow a moving bottom. All autonomous growth while pinned takes this one route. |
| **glide** | A spring chase in flight, as the user sees it. |
| **warm gate** | Quiescence gate over content deliveries: growth stays sync-pinned until the content observer has been quiet or a failsafe trips. Stops a thread restore from chasing its own mount cascade. |
| **write caller** | Every programmatic `scrollTop` write names its origin from a closed union, classified `program` (bounded continuous motion) vs `placement` (one-shot). |
| **activity run** | One maximal stretch of consecutive rail rows rendered as one timeline row that scrolls in place and collapses to a line. The one nested scroller running the pane's physics (`docs/architecture/activity-runs.md`). |
| **rail** | The continuous left border marking activity-row membership. Rail continuity and run continuity are the same property. |
| **reveal gate** | The position in the item window past which nothing renders yet, driven by the per-item smoother. |
| **smoother** | `PerItemSmoother`: word-aligned reveal controller holding `received` (wire accumulator) vs `revealed` (animated cursor). Nothing is skipped or rushed; bursty streams make the backlog self-correcting. |
| **subagent fold** | Settled subagent children evicted from pane memory into a per-anchor registry and re-hydrated from SQLite on card expansion. An id is folded XOR loaded. |
| **size priors** | Per-thread, per-row measured-size persistence feeding the virtualizer's estimate resolver. |
| **engine compensation** | The windowing engine never writes `scrollTop`; geometry changes surface as observations routed to the scroll controller. |
| **the Print Doctrine** | The transcript renders like print: exactly two motion owners inside the timeline scroller (scroll glide, streaming line-slide). Everything else is still ink, enforced by tripwire tests. |
| **chip** | The scroll-to-bottom affordance. Its 70px visibility band is chip-visibility only, deliberately not the re-stick tolerance. |

## Frontend state

Source: `frontend/AGENTS.md`.

| Term | Definition |
|---|---|
| **entityStore vs keyedSignalRegistry** | The two keyed-store primitives, chosen by "is there something to RELEASE?": `entityStore` when the key is backed by an acquirable backend resource; `keyedSignalRegistry` when the key is push-fed. |
| **entity keying** | State is keyed by its entity (app / project / workspace / PR / thread / pane), never by its consumer. |
| **replica** | IndexedDB copy of recently-viewed thread windows that paints before the sync RPC returns, validated by a per-thread `rev`/`epoch` stamp pair. A paint accelerator, not a source of truth (`docs/architecture/thread-replica-sync.md`). |
| **companion pane** | A secondary pane (review / plan / browser / agent) opened beside a chat pane. |
| **overlay** | A sibling of `<PaneHost>` (workflows, settings): never a pane kind, never replaces the pane strip. |
| **front burner** | The first manual sidebar pin block. `pin_group` NULL/0 maps here; it keeps the accent pin and sorts by the normal status/activity/id comparator within the block. |
| **back burner** | The second manual sidebar pin block. `pin_group = 1`; it uses the muted pin token and the same normal comparator, separated from front burner only when both blocks exist. |

## Providers, accounts, triage

| Term | Definition |
|---|---|
| **husk** | The blank credential file claude ≥ 2.1.219 leaves after a failed token refresh, tokens blanked in place. A slot holding one must never overwrite a real credential (`internal/provideraccounts/AGENTS.md`). |
| **slot** | One saved account's opaque provider-native credential file, stored under the provider's own home with its native filename and shape. |
| **canonical credential / home** | The single real credential file the provider reads per request; normal provider processes always run from the canonical native home. |
| **the account tuple** | Claude splits a login across `.credentials.json` (tokens) and `~/.claude.json` `oauthAccount` (identity). AO never writes a provider identity, only retires one, and retires before the credential write. |
| **ghost-row flip** | On Codex session start, persisted `is_background=1 AND status='running'` tool_call rows from a dead subprocess transition to `errored`/`lost` (`internal/codexghost/AGENTS.md`). |
| **triage (package)** | Classifies each normalized `ProviderEvent` and decides what ships to the frontend vs what writes to SQLite. No derived state (`internal/triage/AGENTS.md`). |
| **the sentinel** | The `default` branch of `Router.Handle` returning `ErrUnhandledEventKind`, existing so a coverage test can loop `AllEventKinds` and fail loudly on a new kind (`docs/architecture/triage-routing.md`). |
| **stash** | `pending_background_task_terminals`: a queued completion held until the agent observes it, drained by `task_notification` (a timing trigger, never a status source) (`docs/architecture/invariants.md` §21). |
| **turn** | The unit `Stop` interrupts. Turn activity is wire-pushed, never derived from items; `turn_index` is monotonic per thread (`invariants.md` §3, §10, §22). |
| **external-queue turn** | A Codex turn that starts on a thread AO owns without AO sending `turn/start`. The app-server's queue service dispatched a row `codex queue --thread` wrote into `state_5.sqlite`. Adopted, never refused, and stamped `Meta.origin = "external-queue"` so the injected prompt is not rendered as something the user typed (`docs/references/codex-wire.md` §"Externally queued turns"). |
| **peer session-turn** | A turn on a provider session AO is connected to that some OTHER client's action produced. The external-queue turn is the one AO currently sees; the term names the class, so a second producer is distinguishable rather than folded into "not ours". |
| **history cut** | Truncating a provider thread's own history at a turn boundary, the provider-side half of edit-and-resend. Codex has three, all turn-granular; AO uses two: the in-place `thread/revert { beforeTurnId }` (keeps the thread id) and `thread/fork { lastTurnId }` (mints a new one). The two anchors describe the same boundary from opposite sides, which is why they are resolved separately (`internal/provider/codex/AGENTS.md` §"History truncation"). |
| **item_index** | Server-assigned, immutable after first upsert, in intended-appearance order, not wire-arrival order. The timeline ordering contract rests on this (`invariants.md` §1). |

## Test rigs and process

| Term | Definition |
|---|---|
| **harness** | `--harness` boot mode: real backend + real SPA, isolated data dir, both provider binaries pointed at `ao-mockprovider`; headless by default, `--window` opens the real webview on it (`docs/architecture/agent-harness.md`). |
| **soak rig** | The soak PRESET (`--autopilot`) on an isolated launcher-shell instance: left running for hours with harness-grade mocking to reproduce renderer hangs. Nothing asserts; ask `make soak-check` (or `ao-harness health`) later. `make soak` = `make harness-wsl` + autopilot on Windows; `make soak-window` is the native-window equivalent (`docs/architecture/soak-rig.md`). |
| **harness instance** | One booted harness/soak backend, identified by the first 8 hex of the SHA-256 of its canonical data root; announced via `harness-instance.json` (with token) and a token-free registry row under the user cache dir (`internal/harness/instanceinfo`). |
| **harness bridge** | The in-page half of the harness (`frontend/src/lib/harness/`), armed by the `harness` bootstrap flag: answers `HarnessUIQuery` with semantic viewport snapshots, element/globals probes, and perf-meter control (`agent-harness.md` § Frontend bridge and perf). |
| **semantic viewport snapshot** | The text answer to "what is on screen": per pane, the mounted timeline rows with geometry and `inViewport` flags, overlays, scroll position. The harness's replacement for screenshots; `ao-harness ui snapshot|diff` consumes it. |
| **bench workload** | A scripted, terminating load run (`ao-harness bench`): reset, seed, arm the perf meters, drive, report. A report doubles as a baseline; drift exits 3 only when `--baseline` is passed. |
| **health rollup** | `ao-harness health`: one ok/warn/red line per concern (liveness, new errors, oracle triggers, RSS, DB size, mocks, replay, perf), file concerns cursor-tracked since last check. The generalized `make soak-check`. |
| **the four pins** | `providerBinaryOverride`, `credentialHomeOverride`, `fileKeychainOverride`, `backgroundFetchDisabled`: the isolation set every mocked boot mode must take together. Three of four is the shape that burned a real login. |
| **spike** | A small isolated experiment outside the project to confirm external tool behavior, then ported in (`docs/references/spike-policy.md`). |
| **packet** | A scoped, independently-reviewable unit of delegated implementation work, each leaving the gates green with independent review before merge (coined during the workflows delegation effort). |
| **test seam** | Production-code hook that stays nil/no-op in production and lets tests synchronize deterministically instead of sleeping. `SetEventHook` is the reference (`docs/architecture/conventions.md`). |
| **prefix verbs** | `handleFoo` / `persistFoo` / `parseFoo` / `emitFoo` / `buildFoo` each carry a contract. `buildFoo` means pure, no I/O (`conventions.md`). |
| **oracle (standing)** | An in-app diagnostic observer that must remain silent; it asserts a bug class has not returned. |
