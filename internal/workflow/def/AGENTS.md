# internal/workflow/def/

Pure, engine-free workflow definition support. This package owns the YAML
authoring shape, resolver, embedded authoring schema, interpolation, generated
phase-envelope schemas, post-validation, and whole-graph dry-run validation.

## Boundaries

- Keep provider, engine, persistence, transport, scheduling, and profile loading
  out of this package.
- File I/O is limited to workflow YAML and sibling prompt files supplied by the
  caller.
- Workflow input names and phase IDs share one namespace. Phase output
  references use `phase.output`; nested object paths may continue after that.
- A phase's output contract is whatever `PhaseOutputs` returns, never
  `phase.Outputs` read directly. A `driver: tool` phase always contributes
  `passed` (boolean) and `exit-code` (number) on top of its authored outputs;
  authoring them is a validation finding, and the merge means a snapshot frozen
  before that rule still resolves one consistent set. Envelope-schema
  generation, envelope post-validation, and variable resolution all go through
  that single function so no path can disagree about what a phase produces.
- Validation returns typed findings and collects all independent errors.
  `Reports` is the separate, non-failing channel: a report describes what a run
  will do (a fan-out wider than the provider capacity it will get), never what
  is wrong with the definition, so `Valid()` ignores it.
- Predicates have one shape definition and one evaluator, both in
  `predicate.go`. `predicateShapeIssues` is what the static validator
  (`validatePredicate`) and the runtime evaluator both ask "is this predicate
  well formed", so a predicate can never validate under one set of rules and
  evaluate under another. `ValidatePredicateShape` and `EvaluatePredicate` are
  the standalone exports for predicates that live outside a workflow gate —
  an automation's run-if condition (`internal/workflow/scheduler`) is checked
  and evaluated through them rather than through a second evaluator. Exporting
  them keeps this package's purity intact: it learns nothing about automations,
  and callers get gate semantics for free.
- `Bindings` is intentionally narrow; profile loading implements it elsewhere.
  It answers `Capacity(name)` rather than a bare "is it declared", because the
  dry-run's width report needs the number the engine will actually schedule
  against. `DeclaredMaxFanOutWidth()` has no "is it bound" half for the opposite
  reason: the ceiling always exists, so 0 means undeclared and
  `EffectiveMaxFanOutWidth` is the single place that resolves it.

## Loop bounds

- A loop route's `max:` is a `LoopBound`: an authored count (`max: 2`) or a
  reference into the run's variable context (`max: fix-budget`), told apart by
  node type alone because a scalar in that position can mean nothing else. The
  reference form is what lets a campaign seed its own budget at run start
  instead of editing the YAML for every run.
- **Frozen snapshots are decoded and never re-validated**, so every run
  persisted before the reference form existed carries `"max": 2` as a plain
  JSON integer. The JSON decoder accepts both shapes and each re-encodes as
  what it was authored as; `IsZero` backs `json:",omitzero"`, so a route that
  declares no bound still persists without the key.
- Validation splits the way every other reference does. `loopBoundShapeFindings`
  (`validate_graph.go`) checks what a bound is on its own — a literal of at
  least one, a non-blank reference — and `validateLoopBoundRef`
  (`validate_vars.go`) resolves the reference where the dominator graph lives. A
  seeded bound obeys a feedback reference's rules plus two of its own: its
  producer may not be optional and must be number-typed, because a bound that is
  absent or non-numeric when the gate evaluates parks the run instead of routing
  it, which is exactly what the dry-run promises will not happen.
- **Resolution happens once, at evaluation, in `EvaluateGate`.** It reads the
  reference through `LookupVariable` and converts it through the same numeric
  conversion predicate comparison uses, so a seeded budget and a routing
  condition can never disagree about what a variable holds. The result must be a
  whole count of at least one; anything else is an error (the engine parks it
  `wiring-error`), never a coerced zero that would end the loop or a coerced one
  that would invent a budget nobody wrote. `RouteDecision.Max` therefore carries
  the RESOLVED number, and because the decision is persisted in the gate trace,
  the trace states the budget the run actually got rather than the name it came
  from.
- A human route's `reject.max` is the same bound and resolves under the same
  rules, in the engine (`resolveHumanGate`) against that attempt's variables. A
  failure there fails the human's action and leaves the run parked; it is never
  read as an exhausted budget.

## Fan-out authoring

- A phase declares its units either statically (`fan_out:` list) or dynamically
  (`over:` an array-typed variable, `as:` an element binding, `unit:` one
  template). The two forms are mutually exclusive, both require a `join:`, and
  both require `shape: fan-out`.
- **A unit binds to exactly one of three things: `prompt:` (agent),
  `command:` (tool), or `call:` (another workflow).** `EffectiveDriver()`
  answers `(Driver, bool)` rather than a bare driver precisely so a call unit —
  which runs no driver at all — cannot be read as an agent unit by a caller
  that forgot the third case; it answers `EffectiveShape() == ShapeCall`
  instead, reusing the phase-level shape rather than a parallel discriminator.
  Every combination other than exactly one binding is a finding, and on a call
  unit so are provider/model/prompt, command, access, and `outputs:` — the
  child workflow's phases carry all of it, and a unit's outputs *are* the
  child's declared outputs, so a declaration here is either a duplicate or a
  lie. `args:` and `max_depth:` require `call:` for the mirror-image reason.
  See "Call authoring" for what a unit's call edge shares with a phase's.
- **A unit declares its own `resources:`, and they are unit-scoped.** The
  phase's are acquired once at entry and held for the whole attempt; a unit's
  are taken per running unit, by any unit that runs work — a `command:` unit
  gating on a container slot as readily as an agent one, which is the case the
  field exists for, since no provider bound paces a command. A call unit
  declares none: it runs no work of its own, and the child workflow's phases
  acquire what they need on the same project bounds, so a declaration here would
  hold a slot for something else's work. Every entry is held to the same
  `binding.capacity` rule a phase's is.
- A fan-out phase **runs no work of its own**, so every field that would
  configure some — `driver`, `provider`/`model`/`prompt`,
  `check`/`command`/`commands`, `access` — is a finding, exactly as it is on a
  call phase. The units and the join carry all of it: `startPhaseWork` expands
  the phase instead of starting a runner, `phaseResources` skips its provider
  bound because each unit takes its own, and `PhaseProducesToolEnvelope` answers
  from the join. A phase-level `access` is refused for the same reason a
  phase-level `provider` is — it reads as "my units may write" and reaches no
  unit; a writing unit declares its own and gets its own sub-worktree. What a
  fan-out phase *does* declare is `inputs:` (what its units and join may
  reference), `outputs:` (the contract its join answers), `resources:` (held
  once for the whole attempt), `watchdog:` (each unit's turn is watched by it),
  and `grants:` (every unit's session is scoped from the phase's frozen set).
- `ExpandUnits(phase, vars)` is the one expansion, shared by the engine's phase
  entry and its recovery paths. Static units expand to themselves; dynamic ones
  are stamped `<template-or-phase-id>-<index>` with the whole element bound
  under `as` for interpolation. It is a pure function of the frozen phase and
  the attempt's variable context, so re-expanding a persisted attempt
  reproduces exactly the same ids and bindings — which is what makes a parked
  fan-out recoverable without persisting the expansion itself.
- Unit count is a runtime fact: the same definition can expand to zero units on
  one run and twenty on the next. Validation therefore checks the *shape* of a
  dynamic fan-out (the `over` variable resolves, is in scope, and is
  array-typed; `as` is a valid identifier that collides with nothing) and never
  the width — only a static list has a width to check here.
- **The project's fan-out ceiling is the one width rule that does apply
  statically** (D29). `EffectiveMaxFanOutWidth(bindings)` resolves the profile's
  `max_fan_out_width`, defaulting to `DefaultMaxFanOutWidth` (32) when the
  profile declares none *or when no profile resolved at all* — the run-start
  path loads `profile.Default()` for a project with no `profile.yaml` and
  enforces exactly that number, so skipping on nil bindings would let a
  definition validate clean and be refused at its first expansion.
  A static list over it is a blocking `fan-out.max-width` **Finding**, not a
  Report: this width never runs, so it is wrong with the definition rather than
  a description of how it will behave. The `fan-out.width` capacity Report is a
  different statement and both survive — inside the ceiling but over capacity is
  pacing, over the ceiling is a refusal. A dynamic width is refused by the
  engine at expansion, where the number first exists.
- `UnitDefinition(phase, unitID, join)` resolves the frozen definition behind
  one persisted unit id without the variable context an expansion needs, for
  the recovery paths that hold a row rather than an attempt. A dynamic phase
  only answers for ids its own template could have stamped
  (`<prefix>-<index>`); anything else — the join's id, a typo — is not found,
  because fabricating a unit from the template for an arbitrary id would run a
  real turn on a wrong contract.

## Call authoring

- **Two kinds of edge, one traversal.** A `shape: call` phase and a call-bound
  fan-out unit are both call edges: `validateCallEdge` resolves the target,
  applies the cycle bound with *that edge's* `max_depth`, and validates the
  child once per dry-run, for both. `CallTargets` (and so
  `PropagatedWorkspaceNeed`) walks unit edges too — a fan-out of writing
  sub-workflows needs a worktree to cut sub-worktrees from. What differs is
  only where the arguments resolve: a phase's against the workflow's
  phase-output graph with a dominance check, a unit's against
  `ResolveUnitDeclarations` (phase inputs plus the `as:` element binding),
  which is exactly what a unit *prompt* may reference, so the two cannot
  disagree about scope. A unit edge has nothing unit-local to dominate, so
  there is no dominance check on it.
- A `shape: call` phase declares a **static** workflow id (`call:`), an argument
  map (`args:` — child input name to a reference in the caller), an optional
  `max_depth:`, and its gate. Nothing else: every field that configures work of
  the phase's own (driver, provider/model/prompt, check/command/commands,
  resources, capabilities/mcp, access, watchdog, inputs, outputs, fan-out) is a
  finding rather than a silently ignored declaration, because the child's phases
  are what carry all of it. The target is never a variable — a static id is what
  lets the dry-run validate the whole call graph before anything runs.
- A call phase's downstream surface is the **child workflow's declared
  `outputs:`**, typed by the child phases that produce them (`CallPhaseOutputs`).
  Validation builds an *effective* workflow whose call phases carry those
  outputs and runs every existing variable, gate, and workflow-output check
  against it, so a parent consumer type-checks against the child's real
  contract with no separate code path. The caller's definition is never mutated.
- `Validate` takes a `CallResolver` because a definition with call edges cannot
  be dry-run without resolution: its arguments, its child's validity, and its
  cycles are all facts about definitions this package will not read from disk
  itself. A nil resolver is legal and reports `call.unresolved` per edge rather
  than calling an unchecked graph valid.
- Each reachable workflow is validated exactly once per dry-run (memoized), a
  child that fails its own validation is reported on the *edge* that calls it
  (`call.child-invalid`, quoting a bounded prefix of its findings), and cycles
  are collected across the traversal and reported on the graph's own result:
  the edge that closes a cycle can sit levels down, so `call.unbounded-cycle`
  always lands on what the caller validated, naming the cycle and the edge that
  must declare `max_depth`.
- `PropagatedWorkspaceNeed(workflow, calls)` is the call-aware workspace answer;
  `DeriveWorkspaceNeed` stays the pure single-definition one. A workflow that
  calls a writing workflow needs a worktree, because the child executes in the
  caller's workspace and never provisions one — the root is the only place a
  worktree can be cut.

## Discovery is flat, and says what it skipped

A workflow is `<id>.yaml` sitting directly in a source directory beside its
`<id>-*.md` prompts. `Resolve` skips every subdirectory, so a hand-authored
`<id>/workflow.yaml` produces no row, no error, and no clue — it is simply not
there, and the id "was not found".

`SkippedDirs(sources)` is the reportable half of that: the directories a source
holds that contain at least one YAML file, which is the one signal separating
"someone tried to author a workflow in here" from an unrelated directory. It is a
separate read rather than a second `Resolve` return value because the engine
resolves on every run start and has nothing to do with the answer, while the two
CLI surfaces that render it (`workflow list`, and `workflow validate --id` when
resolution fails) pay one extra directory listing only when a human is asking.
It stays pure — no stderr, no log — so the caller decides how loud a skipped
directory is.

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
  creation instead (`createWorkflowThread`, repo root), which is also where the
  `threads.reasoning_effort` CHECK constraint is satisfied.
- The vocabulary is declared here rather than imported because this package
  stays free of `internal/provider`. The two lists are held together by
  `TestWorkflowEffortTiersMatchTheProviderReasoningEfforts` in the root package,
  which compares them in both directions and in order.

## Phase grants

- A phase may declare `grants:`, the first-party `ao` capabilities its agent is
  allowed to exercise (spec §5). The set is CLOSED — `start-run`, `schedule`,
  `update-notes`, `introspect` — and lives in `grants.go`. An unknown name is a
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
  exactly what freezing exists to prevent. `frozenPhaseGrants` (repo root,
  `app_ao_session.go`) reads them back and drops any name this build does not
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
- `SplitEnvelopeNarrative` is the seam the app lifts that field out at, and
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

## Files

| File | Responsibility |
|---|---|
| `types.go` | YAML and validation result types. |
| `parse.go` | Strict single-document YAML parsing. |
| `resolve.go` | Ordered scoped-directory resolution, plus `SkippedDirs` (below). |
| `schema.go` | JSON-Schema fragments and embedded authoring schema. |
| `grants.go` | The closed `ao` grant set and the phase-level grant checks. |
| `effort.go` | The closed reasoning-tier vocabulary and the tier-name check. |
| `envelope.go` | Generated control schema and payload post-validation. |
| `tool.go` | The tool driver's implicit outputs and the merged `PhaseOutputs` contract. |
| `interpolate.go` | Single-pass prompt interpolation and template checks. |
| `predicate.go` | The one predicate shape check shared by validation and evaluation, plus the standalone `ValidatePredicateShape` / `EvaluatePredicate` entry points. |
| `loopbound.go` | A loop route's `max:`: both authored forms, their YAML/JSON (un)marshaling including legacy integer snapshots, the shape check, and runtime resolution. |
| `fanout.go` | Unit expansion, unit-scoped declarations, and the implicit `provider:<name>` resource + its default capacity. |
| `calls.go` | Call-phase accessors, the child-outputs surface, and the call-aware workspace need. |
| `validate*.go` | Whole-definition structural, graph, variable, and binding checks. `validate_fanout.go` holds the fan-out structural rules and the width report; `validate_calls.go` holds the call shape, the call-graph traversal (cycles, child validity), and the argument checks. |

Tests use deterministic fixtures under `testdata/` and must not inspect shared
application configuration.
