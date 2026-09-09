# Workflow definitions

This package owns workflow YAML parsing, validation, reference resolution,
interpolation, gate evaluation, fan-out and call authoring, envelope contracts,
and schema generation. Keep provider execution, engine state, persistence,
scheduling, transport, and profile loading outside it.

## Definition and snapshot rules

- Definition loading reads flat `<id>.yaml` files and sibling prompt files. It
  performs no recursive workflow discovery.
- Validation returns typed findings and collects independent errors. Do not
  replace author-facing validation with first-error parsing.
- Input names and phase IDs share one namespace. Reserved names are rejected,
  not silently shadowed.
- `PhaseOutputs` is the authoritative phase output contract. Callers must not
  infer it directly from `phase.Outputs` because shape-specific outputs differ.
- Frozen run snapshots are decoded without applying current validation rules.
  Decoders must remain compatible with snapshots created by older versions.
- A running workflow uses its frozen snapshot. Calls intentionally resolve the
  called definition at the call edge.

## References and routes

- `history.<phaseID>` exposes bounded prior attempts to prompts but is not a
  routing reference.
- `call-depth` is zero for a directly started run and increases at each call
  edge. `units` is reserved for join bindings and is bound last.
- Resolve loop bounds once during `EvaluateGate`, using the same numeric
  coercion rules as ordinary references.
- `notify`, `session`, and prompt decorations are valid only where their route
  contract allows them. Validation and `decisionForRoute` must enforce the same
  rules.
- Workflow outputs are synthesized deliverables. Validation must prove required
  outputs are reachable; runtime synthesis still reports missing values.

## Fan-out and calls

- A fan-out unit selects exactly one execution form: agent prompt, tool command,
  or workflow call.
- `ExpandUnits` is the single deterministic expansion used by normal entry and
  recovery. Enforce dynamic width at runtime because input data determines it.
- Unit resources are acquired per running unit. Joins have their own execution
  and resource contract.
- A call phase and a call-bound unit use the same call graph rules. Validate
  static edges and preserve runtime checks for depth and dynamically resolved
  context.
- Use `PropagatedWorkspaceNeed` for call-aware workspace requirements.

## Agent and envelope contracts

- `effort` applies only to agent turns. This package validates the vocabulary;
  the application validates availability for the selected provider and model.
- Grants apply only to elements that hold an agent session and are frozen with
  the definition. Keep the supported grant vocabulary closed.
- `EnvelopeContract` is shared by schema generation and post-validation. Keep
  those two paths aligned.
- Generated schemas require every control key. Post-validation enforces branch
  meaning, including output declarations, question and reason fields, narrative,
  and workflow-memory entries.
- `memory` is product data: it carries structured workflow-memory notes from an
  agent envelope. Preserve its closed schema and validation in
  `envelope_memory.go`.
- Tool and agent units may have unit output declarations. Call-unit outputs come
  from the called workflow. A join envelope is the phase envelope.
- `accounts_for_units` is an envelope completeness assertion for a join, not a
  merge implementation.

See `docs/specs/workflows-system.md` for the authoring model and
`docs/specs/workflows-system-decisions.md` for settled behavior.
