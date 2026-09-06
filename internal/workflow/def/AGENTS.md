# internal/workflow/def/

Pure, engine-free workflow definition support: the YAML authoring shape, resolver, embedded
authoring schema, interpolation, generated phase-envelope schemas, post-validation, and
whole-graph dry-run validation.

Rules only. The reasoning lives in `docs/specs/workflows-system.md` (section numbers) and
`docs/specs/workflows-system-decisions.md` (D-numbers).

## Boundaries

- Keep provider, engine, persistence, transport, scheduling, and profile loading out. File I/O
  is limited to workflow YAML and sibling prompt files the caller supplies, bounded by
  `MaxDefinitionBytes` (1 MiB) and `MaxPromptBytes` (4 MiB) in `files.go`.
- Workflow input names and phase IDs share one namespace. Phase output references are
  `phase.output`, and nested paths may continue after that.
- **A phase's output contract is whatever `PhaseOutputs` returns**, never `phase.Outputs`. A
  `driver: tool` phase always contributes `passed` and `exit-code` on top of its authored
  outputs, and authoring them is a finding.
- Validation returns typed findings and collects all independent errors. `Reports` is the
  separate, non-failing channel: a report says what a run will do, never what is wrong with the
  definition, so `Valid()` ignores it. **Predicates have one shape definition and one
  evaluator**, both in `predicate.go`, so a predicate cannot validate under one rule set and
  evaluate under another. `ValidatePredicateShape` / `EvaluatePredicate` are the exports for
  predicates outside a gate.
- **Frozen snapshots are decoded and never re-validated.** Every decoder must accept the older
  shape and every resolution helper must re-apply its own ceiling, because a run started before
  a rule existed still reaches the evaluator.

## Reserved names

- Reserved names are facts the ENGINE holds that an author could only restate.
  `reservedInputName` / `reservedDeclaration` / `bindReservedDeclarations` (`depth.go`) are the
  single list; adding one means adding it there and nowhere else.
- **The reservation is a refusal, not a silent shadow** (`input.reserved`, `phase.reserved`,
  `namespace.collision`), and every declaration site is covered including the PHASE ID: the
  engine binds reserved names LAST, so a colliding phase's whole output object would vanish.
- **`call-depth`** is how deep this run sits in its call tree, 0 for a directly started run;
  the bare `depth` stays available (`TestBareDepthStaysAvailableToAuthors`). **`budget`**
  (`budget.go`) is declared `optional:`, which no other reserved read is, so an unbudgeted run
  renders `(not provided)`. Its schema declares no properties, and it is refused in a gate
  predicate (`predicate.ref`): have the phase decide and declare that as an output.
- **`units`** is bound last by `JoinDeclarations(phase)`, so a phase input cannot shadow the
  results a join exists to consolidate. The grammar has no indexing, so `{{units}}` and
  `{{history.<phase>}}` as whole values are the supported forms.
- **`history.<phaseID>`** binds that phase's earlier attempts as a series, oldest first, for
  any phase including itself, because `<phase>.<output>` resolves to the highest COMPLETED
  attempt alone and leaves a looped phase blind to every round before the last.
- History is a **prompt surface, not a routing one**: `resolveReference` never resolves it, so
  a predicate, feedback reference, or workflow output naming it is the ordinary
  unresolved-reference finding. `history` is reserved at both ends of the namespace. Its
  declaration is `schema: {type: array}` plus an optional `window: N`; any other keyword is
  `history.schema` and `optional:` is `history.optional`. `window:` is legal here alone,
  defaults to 10, and over `MaxHistoryWindow` (50) is a finding rather than a silent trim.
  `MaxHistoryBytes` (32 KiB) lives here too.

## Gate routes

- A loop route's `max:` is a `LoopBound`: an authored count or a reference into the run's
  variable context, told apart by node type alone; a human route's `reject.max` is the same
  bound. The JSON decoder also accepts the legacy plain-integer shape, and a seeded bound's
  producer must be non-optional and number-typed.
- **Resolution happens once, at evaluation, in `EvaluateGate`**, through the same numeric
  conversion predicate comparison uses. It must be a whole count of at least one, never a
  coerced zero or one, and `RouteDecision.Max` carries the RESOLVED number into the gate trace.
- **`notify: true` asks for a progress wake** and is a decoration, not a route kind. It is
  refused on a `human:` or `park:` route (`gate.notify`) because parking already wakes the
  bound thread, and on a route to `done` or `failed` it is a Report (`gate.notify-terminal`).
  **A loop route may declare `session: continue` and `prompt: <file.md>`**, which make the
  re-entry authorable. `fresh` is the default and stays the default, for anti-anchoring.
- **Both are refused outside a `loop:` route** (`gate.session`, `gate.prompt`) and both require
  a loop target that runs one session of its own, so a tool phase, a call phase, and a fan-out
  are findings. `prompt:` is template-checked against the TARGET phase's inputs and never
  sticky.
- **`decisionForRoute` enforces `notify:` and `session:`, it does not merely check them**, so
  the trace records what was owed rather than what was authored.

## Workflow inputs and outputs

- A phase input whose key is exactly a declared workflow input name and which declares no
  schema of its own **inherits that workflow input's schema** (`inherit.go`), as a
  resolution-time copy applied by `Parse`. An explicit schema wins and is not checked against
  the workflow input's, because narrowing is the reason to restate one; only the schema is
  inherited.
- A workflow's `outputs:` are its deliverables, synthesized at completion and read by whatever
  called it. A required one with no value is a hard failure at that moment (D44), enforced in
  `internal/workflow/engine`. **`optional: true` means the deliverable is genuinely absent on
  some completion path.** An absent optional output is OMITTED from the synthesized envelope,
  not nulled, matching an absent optional call argument (D45).
- **`workflow.output-unreachable`** (`validate_outputs.go`) is the dry-run half: a required
  output whose producer is not on every path reaching `done` is a finding naming the output,
  the phase, and ONE completion path that misses it. An optional output is never reported, and
  a `human:` route's `approve: done` counts as a completion path.

## Fan-out authoring

- A phase declares units statically (`fan_out:` list) or dynamically (`over:` an array-typed
  variable, `as:` an element binding, `unit:` one template). The forms are mutually exclusive,
  and both require a `join:` and `shape: fan-out`.
- **A unit binds to exactly one of `prompt:` (agent), `command:` (tool), or `call:` (another
  workflow).** `EffectiveDriver()` answers `(Driver, bool)` so a call unit cannot be read as an
  agent unit. On a call unit, provider/model/prompt, command, access, and `outputs:` are
  findings, and `args:` / `max_depth:` require `call:`.
- **A unit's `resources:` are unit-scoped**, taken per running unit by any unit that runs work,
  a `command:` unit included. A call unit declares none, and every entry is held to the same
  `binding.capacity` rule a phase's is. **A fan-out phase runs no work of its own**, so
  `driver`, provider/model/prompt, check/command/commands, and `access` are findings. What it
  does declare is `inputs:`, `outputs:` (the contract its join answers), `resources:`,
  `watchdog:`, and `grants:`.
- `ExpandUnits(phase, vars)` is the one expansion, shared by phase entry and recovery, and is a
  pure function of the frozen phase and the attempt's variable context. That is what makes a
  parked fan-out recoverable without persisting the expansion.
- Unit count is a runtime fact, so validation checks the SHAPE of a dynamic fan-out and never
  its width. **The project's fan-out ceiling is the one width rule that applies statically**
  (D29): `EffectiveMaxFanOutWidth(bindings)` defaults to 32 when no profile declares one or
  none resolved, and a static list over it is a blocking `fan-out.max-width` Finding.

## Call authoring

- **Two kinds of edge, one traversal.** A `shape: call` phase and a call-bound fan-out unit are
  both call edges: `validateCallEdge` resolves the target, applies the cycle bound with that
  edge's `max_depth`, and validates the child once per dry-run for both. What differs is only
  where arguments resolve, and a unit edge has nothing unit-local to dominate.
- A call phase declares a **static** workflow id, `args:`, an optional `max_depth:`, and its
  gate; every field that would configure work of its own is a finding. A static target is what
  lets the dry-run validate the whole call graph before anything runs. A call phase's
  downstream surface is the **child workflow's declared `outputs:`**. Validation builds an
  EFFECTIVE workflow carrying those outputs and runs every existing check against it, never
  mutating the caller. `Validate` takes a `CallResolver`, and a nil one reports
  `call.unresolved` per edge rather than calling an unchecked graph valid.
- `PropagatedWorkspaceNeed(workflow, calls)` is the call-aware workspace answer. A workflow
  that calls a writing workflow needs a worktree, because the child never provisions one (§9).

## Grants and reasoning effort

- `grants:` is the CLOSED set of first-party `ao` capabilities a phase's agent may exercise
  (§5): `start-run`, `schedule`, `update-notes`, `introspect`. An unknown name is a finding.
  **`report-back` is deliberately not in the v1 set** even though §5 lists it. Grants require
  an agent session, so a tool phase is a finding; a fan-out phase answers with its units and
  its join, and a call phase grants nothing. Grants freeze with the snapshot, and
  `frozenPhaseGrants` (`internal/app`) drops any name this build does not recognize.
- `effort:` is legal exactly where `provider:`/`model:` are, so it is a finding on a tool
  phase, a call phase, a fan-out phase, and a call unit.
- **Validation checks the tier NAME; the app checks the tier against the model.** `effort.go`
  owns the closed vocabulary (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`,
  `ultra`), declared rather than imported to keep this package free of `internal/provider`.
  Which tiers a given model advertises is deliberately not validated, because the catalog is
  provider-owned and partly live.

## Envelopes

## Reasoning effort

- `effort:` pins the reasoning tier of one model turn. It is legal **exactly
  where `provider:`/`model:` are** — an agent-driver phase running its own turn,
  and a prompt-bound fan-out unit or join. On a tool phase it is
  `phase.effort`, and a stray `provider:`/`model:`/`prompt:` on a tool phase is
  refused the same way (`phase.tool`) — a tool phase runs a command, not a model
  turn, so any of the four would be a dead line the author never learns was
  ignored. On a call phase, a fan-out phase, and a call unit `effort:` joins the
  existing `provider/model/effort/prompt` refusal group, because those elements
  run no turn of their own. A command unit gets the same "a unit declares a
  command, provider/model/effort/prompt, or call, not more than one" finding a
  stray `provider:` gets: it is one rule, not a parallel one.
- **Validation checks the tier NAME; the app checks the tier against the
  model.** `effort.go` owns the closed vocabulary (`none`, `minimal`, `low`,
  `medium`, `high`, `xhigh`, `max`, `ultra`) and an unknown name is a finding —
  a typo must not read as "run at the model's default". Which of those tiers a
  *given* model advertises is deliberately NOT validated: the catalog is
  provider-owned and partly live (Codex's comes off the app-server, Claude's is
  probe-enriched), so a static rule would make a definition's validity depend on
  data the author cannot see in the YAML and cannot pin. An authored tier the
  model does not advertise is coerced onto that model's own default at thread
  creation instead (`createWorkflowThread`, `internal/app`), which is also where the
  `threads.reasoning_effort` CHECK constraint is satisfied.
- The vocabulary is declared here rather than imported because this package
  stays free of `internal/provider`. The two lists are held together by
  `TestWorkflowEffortTiersMatchTheProviderReasoningEfforts` in `internal/app`,
  which compares them in both directions and in order.

## Phase grants

- A phase may declare `grants:`, the first-party `ao` capabilities its agent is
  allowed to exercise (spec §5). The set is CLOSED — `start-run`, `schedule`,
  `update-notes`, `introspect`, `resolve`, `remote-commands` — and lives in `grants.go`. An unknown name is a
  finding rather than an ignored line, because a typo would otherwise read as
  "this phase deliberately has no authority".
- Grants require an agent session, which `phaseHoldsAgentSession` is the one
  predicate for. A `driver: tool` phase runs a command, not a session that could
  hold the credentials, so `grants:` on one is a finding. A fan-out phase has no
  driver at all, so it answers with its units and its join — grants stay legal
  there (the app scopes every unit's token from the *phase's* frozen set) and
  are a finding only when nothing under the phase runs an agent. A call phase
  grants nothing either: the child workflow's own phases declare what they may
  do.
- Grants are frozen into the run snapshot with everything else. A phase
  re-entered after the definition was edited keeps the authority the run
  started with — widening a running phase's authority by editing a file is
  exactly what freezing exists to prevent. `frozenPhaseGrants`
  (`internal/app/app_session_runtime.go`) reads them back and drops any name this build does not
  recognize, so an old snapshot cannot hand out authority the code cannot
  enforce.
- **`report-back` is deliberately NOT in the v1 set** even though spec §5 lists
  it. The other four map onto CLI commands that already exist; `report-back`
  does not yet have a defined destination or payload contract, and shipping a
  grant name that authorizes nothing would be a promise the enforcement layer
  cannot keep. It stays out until it is ratified with a contract; §5 is
  unchanged and the orchestrator owns surfacing the gap.

## Unit and join envelopes

- `EnvelopeContract` is the one thing schema generation and post-validation are
  written against, so a unit cannot drift from the rules a phase is held to.
  `PhaseEnvelope(phase)` and `UnitEnvelope(unit)` differ only in the
  declarations they carry and the element name diagnostics blame.
  `EnvelopeSchema` / `ValidateEnvelope` remain the phase-level shorthands.
- The control fields are `status`, `outputs`, `question`, `reason`, and
  `narrative`. **`narrative` is the one field with no branch rule**: a done, a
  question, and a stuck element all did work worth an account, so refusing it
  anywhere would burn the element's single envelope retry on the one field that
  is never a mistake. It exists because Codex applies a turn's `outputSchema` to
  EVERY assistant message in that turn — an element under a schema cannot emit
  prose there at all, so "send your narrative as the message before your
  envelope" was an instruction only Claude could follow. Outputs nest under
  `outputs`, so an author may still declare an output literally named
  `narrative` and the two never meet — no reserved-name check is needed or
  wanted.
- **The generated schema requires every control key; post-validation requires
  only `status`.** Strict mode has no optional, only required-and-nullable
  (`internal/providerschema`), so the schema lists all five and a provider under
  it emits all five, answering null where it has nothing to say. Post-validation
  reads back what that null MEANT rather than the null itself: an absent
  `question`, `reason`, or `narrative` is that null, and an absent `outputs` is
  an empty one — a `done` element still owes every declared output, so an
  envelope that omits the key is told which deliverables are missing, never that
  a container is. `status` is the one literal requirement, because it is the
  discriminator and a document without it is not an envelope. The keys a schema
  forces onto a provider are not a debt a hand-written envelope owes: a tool
  command's envelope, and every envelope frozen before a field existed, carry no
  null boilerplate and are judged identically to one that does.
- **`memory` is the second field with no branch rule**, and the second one the
  app lifts out. It carries an array of `{kind, text, files}` campaign-memory
  notes; a `read-only` element records through it because it cannot reach the
  `agent-overflow memory add` verb at all (see `internal/workflow/runner`), and
  a `write` element is told to leave it null. Post-validation accepts it from
  either — one contract, one rule set — so a write element that answers it has
  its notes recorded rather than dropped. What a note IS lives in
  `internal/workflow/memory` (the closed kind vocabulary, the text/file bounds,
  `ValidateDraft`); `envelope_memory.go` adds only the schema fragment and the
  two things a schema cannot express: the per-envelope count bound
  (`MaxEnvelopeMemoryNotes`, 20) and the refusal of an author-supplied
  `provenance` / `at` / `wave`, reported as `property is not allowed` rather
  than ignored, because a field an element is told is merely ignored keeps
  getting sent.
- The field has to be in the GENERATED schema, not merely tolerated by
  post-validation: the top-level object is closed, so a provider under it
  physically cannot emit a property the schema does not declare. It is
  required-and-nullable like every other control field, since strict mode has no
  optional.
- `SplitEnvelopeNarrative` is the seam the app lifts that field out at,
  `SplitEnvelopeMemory` is its twin for the notes, and
  `EnvelopeAccount` is what narrative recovery reads an envelope-SHAPED document
  with (a top-level `status`, weaker than the document-identity test recovery
  applies to the accepted envelope). Both live here because this package owns
  what an envelope is; neither validates, and both return their input untouched
  when it is not one.
- A unit may declare its own `outputs:`, validated by the same name grammar,
  schema vocabulary, and reserved-tool-output rules a phase's are. A unit that
  declares none gets the control-only envelope: `status`, `question`, `reason`,
  and an `outputs` with nothing to answer — empty, null, and absent are one
  answer there, since no declaration is being withheld. The run still has to
  learn done/question/stuck from it.
- A **call unit** declares none either, and for the opposite reason: its
  envelope is the *child workflow's* declared `outputs:`, synthesized from the
  child run rather than authored, so any declaration here duplicates or
  contradicts a contract this definition does not own.
- A **join** declares none at all: its envelope IS the phase's, so the only
  contract it can answer is the phase's `outputs:`, and a second declaration
  would name outputs the gate never reads. A join may not be a `call:` for the
  same reason it declares no outputs — the phase's continuations
  (`Answer`, `CompleteTakeover`, a resume in place) are continuations of the
  join's own session, and a child run is not one. That also means what produces a
  phase's envelope follows the join — `PhaseProducesToolEnvelope` reports true
  for a fan-out whose join is a command, so `PhaseOutputs` merges `passed` /
  `exit-code` exactly as it does for a `driver: tool` phase and the synthesized
  tool envelope validates against its own phase contract.
- A fan-out phase runs no turn of its own, so **any** `driver:` on the phase is
  a finding (see "Fan-out authoring"): the binding would never run, and "what
  produces this phase's envelope" would be ambiguous between the phase and its
  join.
- `JoinDeclarations(phase)` binds the reserved `units` name last so a phase
  input can never shadow the results a join exists to consolidate. The
  reference grammar has no indexing, so `{{units}}` — the whole array rendered
  as JSON — is the supported form in a join prompt or command.

## The merge-join contract: `accounts_for_units:`

- A `join:` may declare `accounts_for_units: true`, holding it to naming every unit of its
  fan-out exactly once. Since the join's envelope IS the phase's, a unit it does not mention
  does not exist downstream.
- **It is a contract, never a merge driver.** The engine merges nothing; how lanes are
  reconciled stays authorable content, and what the flag buys is that a join which loses one is
  REFUSED instead of believed. Two halves that must agree. Statically (`join_accounting.go`)
  the PHASE must declare non-optional `merged` and `blocked`, blamed on the phase because a
  join declares no outputs. At run time `JoinEnvelope(phase, unitIDs)` carries the obligation
  and post-validation checks a `done` envelope against that exact set. `accounts` and
  `accounted` are separate fields because a join over ZERO units still owes two empty lists.
- The refusal is ordinary envelope-validation feedback, never a park. It applies to `done`
  alone. A whole unreadable list is not reported, since the declared output rules already do,
  but a malformed ENTRY is reported where it sits.

## Non-goals and discovery

- `non_goals:` is the author's standing "do not drift here" list, def-owned where a goal is
  run-owned. Bounds are findings, never silent trims (`goals.go`): at most `MaxNonGoals` (12)
  entries of at most `MaxNonGoalRunes` (500) RUNES each.
- **Discovery is flat.** A workflow is `<id>.yaml` sitting directly in a source directory
  beside its `<id>-*.md` prompts, and `Resolve` skips every subdirectory, so a hand-authored
  `<id>/workflow.yaml` produces no row, no error, and no clue. `SkippedDirs(sources)` is the
  reportable half, read only when a human asks.


## References

- `docs/specs/workflows-system.md` for §5 grants, §8 call scoping, and §9 workspace.
- `docs/specs/workflows-system-decisions.md` for D29, D44, D45, and incident D-C1.
- `internal/workflow/engine/AGENTS.md` for what the engine composes and enforces at runtime.
