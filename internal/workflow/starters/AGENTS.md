# internal/workflow/starters/

Embedded product-quality workflow definition sets used only as sources for
`agent-overflow workflow new`. The workflow engine must never load this package
as a hidden built-in definition tier.

Each starter directory contains one `workflow.yaml` plus its sibling prompt
Markdown files, and the directory name is the workflow's id. Keep the leading
binding inventory in the YAML synchronized with every check, command, and
capacity used by the definition; tests enforce the exact match and validate the
complete copied set through `workflow/def`.

## Starters that call starters

A starter may name another one on a `call:` edge — `port-campaign` calls
`port-one-task` for every implement unit, and calls itself for the next wave
(D37). Two rules follow:

- **The set is validated as a set.** `TestEmbeddedStartersAreCompleteAndValid`
  materializes every starter first and validates each against a `CallResolver`
  over all of them, because a dry-run with no resolver reports
  `call.unresolved` per edge rather than checking the graph. A starter's
  directory name must therefore equal its workflow id, since that is the name a
  sibling's edge uses.
- **Only self-calls follow `--id`.** `workflow new` renames the definition it
  is creating, including its calls to itself (`rewriteSelfCalls` in
  `internal/aocli/workflow_new.go`). A call to a *different* starter keeps the
  documented id, because the scaffolder does not know what the user called that
  one — so a starter with a companion says so in its leading comment, and
  scaffolding it alone produces a `call.target` finding naming what is missing.
