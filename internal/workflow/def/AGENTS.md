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

## Fan-out authoring

- A phase declares its units either statically (`fan_out:` list) or dynamically
  (`over:` an array-typed variable, `as:` an element binding, `unit:` one
  template). The two forms are mutually exclusive, both require a `join:`, and
  both require `shape: fan-out`.
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
- A unit may declare its own `outputs:`, validated by the same name grammar,
  schema vocabulary, and reserved-tool-output rules a phase's are. A unit that
  declares none gets the control-only envelope: `status`, `question`, `reason`,
  and `outputs` present but empty — the run still has to learn done/question/
  stuck from it.
- A **join** declares none at all: its envelope IS the phase's, so the only
  contract it can answer is the phase's `outputs:`, and a second declaration
  would name outputs the gate never reads. That also means what produces a
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
| `resolve.go` | Ordered scoped-directory resolution. |
| `schema.go` | JSON-Schema fragments and embedded authoring schema. |
| `grants.go` | The closed `ao` grant set and the phase-level grant checks. |
| `envelope.go` | Generated control schema and payload post-validation. |
| `tool.go` | The tool driver's implicit outputs and the merged `PhaseOutputs` contract. |
| `interpolate.go` | Single-pass prompt interpolation and template checks. |
| `predicate.go` | The one predicate shape check shared by validation and evaluation, plus the standalone `ValidatePredicateShape` / `EvaluatePredicate` entry points. |
| `fanout.go` | Unit expansion, unit-scoped declarations, and the implicit `provider:<name>` resource + its default capacity. |
| `calls.go` | Call-phase accessors, the child-outputs surface, and the call-aware workspace need. |
| `validate*.go` | Whole-definition structural, graph, variable, and binding checks. `validate_fanout.go` holds the fan-out structural rules and the width report; `validate_calls.go` holds the call shape, the call-graph traversal (cycles, child validity), and the argument checks. |

Tests use deterministic fixtures under `testdata/` and must not inspect shared
application configuration.
