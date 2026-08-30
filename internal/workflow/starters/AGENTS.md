# internal/workflow/starters/

Embedded product-quality workflow definition sets used only as sources for
`agent-overflow workflow new`. **The workflow engine must never load this package as a hidden
built-in definition tier.**

Each starter directory holds one `workflow.yaml` plus its siblings, and **the directory name is
the workflow's id**, because that is the name a sibling starter's `call:` edge uses.

Each YAML documents its own pattern in its leading comment and its `description:` fields. That
is the source of truth for what a starter demonstrates; do not restate it here.

- Keep the leading binding inventory in the YAML synchronized with every check, command, and
  capacity the definition uses. Tests enforce the exact match and validate the complete copied
  set through `workflow/def`.
- `patterns_test.go` pins the shapes the campaign-shaped starters exist to DEMONSTRATE, since a
  valid definition is not the same as the composed pattern. Flattening a ratchet or dropping the
  ledger forwarding fails there rather than silently shipping a starter that teaches nothing.
- A sibling that is not a prompt (the campaign's reference merge script) is copied and
  namespaced like every other one. Only `.md` names are rewritten inside the YAML, since
  `prompt:` is the one field that references a sibling. See `internal/aocli/AGENTS.md`.
- `merge_script_test.go` is the one test that EXECUTES a starter sibling. The merge script is
  content, but the `accounts_for_units:` contract it demonstrates is enforced by the engine, so
  each case runs the real script and puts its real envelope through
  `def.JoinEnvelope(...).Validate` over the ids `def.UnitIDsFromResults` reads from the same
  units array. The set the join is judged against is derived exactly as the engine derives it.
  The test needs no git repository and skips when `python3` is not on PATH.

## Starters that call starters

A starter may name another one on a `call:` edge, and may call itself for a next wave (D37).

- **The set is validated as a set.** `TestEmbeddedStartersAreCompleteAndValid` materializes
  every starter first and validates each against a `CallResolver` over all of them, because a
  dry-run with no resolver reports `call.unresolved` per edge rather than checking the graph.
- **Only self-calls follow `--id`.** `workflow new` renames the definition it is creating,
  including its calls to itself (`rewriteSelfCalls` in `internal/aocli/workflow_new.go`). A call
  to a DIFFERENT starter keeps the documented id, because the scaffolder does not know what the
  user called that one. A starter with a companion says so in its leading comment, and
  scaffolding it alone produces a `call.target` finding naming what is missing.
