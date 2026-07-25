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
- `Bindings` is intentionally narrow; profile loading implements it elsewhere.
  It answers `Capacity(name)` rather than a bare "is it declared", because the
  dry-run's width report needs the number the engine will actually schedule
  against.

## Fan-out authoring

- A phase declares its units either statically (`fan_out:` list) or dynamically
  (`over:` an array-typed variable, `as:` an element binding, `unit:` one
  template). The two forms are mutually exclusive, both require a `join:`, and
  both require `shape: fan-out`.
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
  the width — only a static list has a width to report on.

## Files

| File | Responsibility |
|---|---|
| `types.go` | YAML and validation result types. |
| `parse.go` | Strict single-document YAML parsing. |
| `resolve.go` | Ordered scoped-directory resolution. |
| `schema.go` | JSON-Schema fragments and embedded authoring schema. |
| `envelope.go` | Generated control schema and payload post-validation. |
| `tool.go` | The tool driver's implicit outputs and the merged `PhaseOutputs` contract. |
| `interpolate.go` | Single-pass prompt interpolation and template checks. |
| `fanout.go` | Unit expansion, unit-scoped declarations, and the implicit `provider:<name>` resource + its default capacity. |
| `validate*.go` | Whole-definition structural, graph, variable, and binding checks. `validate_fanout.go` holds the fan-out structural rules and the width report. |

Tests use deterministic fixtures under `testdata/` and must not inspect shared
application configuration.
