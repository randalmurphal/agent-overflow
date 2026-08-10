# internal/workflow/starters/

Embedded product-quality workflow definition sets used only as sources for
`agent-overflow workflow new`. The workflow engine must never load this package
as a hidden built-in definition tier.

Each starter directory contains one `workflow.yaml` plus its siblings — prompt
Markdown files, and any other file the definition references — and the directory
name is the workflow's id. Keep the leading binding inventory in the YAML
synchronized with every check, command, and capacity used by the definition;
tests enforce the exact match and validate the complete copied set through
`workflow/def`. `patterns_test.go` pins the shapes the campaign-shaped starters
exist to DEMONSTRATE (a valid definition is not the same as the composed
pattern), so flattening a ratchet or dropping the ledger forwarding fails there
rather than silently shipping a starter that no longer teaches anything.

A sibling that is not a prompt — the campaign's reference merge script — is
copied and namespaced like every other one; only `.md` names are rewritten
inside the YAML, since `prompt:` is the one field that references a sibling. See
`internal/aocli/AGENTS.md`.

`merge_script_test.go` is the one test that EXECUTES a starter sibling. The
merge script is content, but the `accounts_for_units:` contract it demonstrates
is enforced by the engine, so a script that answers lists the engine refuses is
not a reference — it is a wave that dies at its merge. Each case runs the real
script over a units array and puts its real envelope through
`def.JoinEnvelope(...).Validate` over the ids `def.UnitIDsFromResults` reads from
that same array: the set the join is judged against is derived exactly as the
engine derives it, so the test cannot drift from the rule. It needs no git
repository (every case is decided before the script reaches a branch) and skips
when `python3` is not on PATH.

## The curator is content, not machinery

`port-one-task` ends a CLEAN lane with a `curate` phase whose whole job is to
distil what that lane learned into campaign memory. It is a STARTER, not an
engine tier, and the distinction is load-bearing: the engine injects campaign
memory into every element's prompt and lifts notes off every envelope whether or
not any workflow has such a phase. What the phase adds is the format contract —
"write for an agent with NO context; they see only your note text" plus the
explicit do-NOT list (no per-file play-by-play, no restating the diff, no status
updates, no praise) — which is a prompt-engineering judgement that belongs in
authorable content where a user can edit it, not compiled into the runner.

It runs read-only at `effort: low` on the done edge only, so it costs one cheap
turn per successful task and nothing at all on a lane a human is already looking
at.

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

## `converge-on-review`: the convergence pattern, composed

The review/fix loop that STOPS. It exists because the working pattern lived as
unenforced prose — "round three and later, only blocking or material findings
extend the loop; minors become residue" — and prose in a prompt is a request. The
starter puts the ratchet in the gate instead, composing four engine features:
`history.review` (the reviewer reads its own prior rounds, so it can see a loop
circling), `session: continue` on the fix edge only (the implementer remembers
what it tried; the reviewer must not remember arguing for its last verdict),
`prompt:` on the loop routes (later rounds ask a narrower question), and a
`memory`-field residue note for the minors that ship.

The ratchet is **route order plus two budgets**: route 1 matches every verdict
and burns `fix-rounds`, route 2 matches blocking/material only and burns
`converge-rounds`, route 3 then ships a minor-only verdict. An exhausted loop
route falls through to the next matching route, which is what makes minors stop
extending the loop without any element counting its own laps. Reordering the
routes, merging the two loop edges, or giving them one budget silently restores
the oscillating loop.

`notify: true` rides the CONVERGENCE loop route rather than a terminal one:
entering the narrowing rounds is the moment a watcher wants to hear about, and
the run continues through it. On a route to `done` the decoration is inert — the
run rests there and its resting wake is the fuller message — which the dry-run
reports as `gate.notify-terminal`.

## The acceptance-criteria ledger is content

`port-campaign` seeds `criteria` (typed, required, fixed at the start) and
forwards each wave's `coverage` answer through the self-call's `args:` exactly
as it forwards the wave number. Nothing in the engine knows the ledger exists,
and that is the design: a per-wave planner that re-forms its own opinion of
"done" every round is the drift the ledger replaces, and the fix needed no
engine change at all — only a typed input, a typed output, and one more
argument on an edge that already existed.
