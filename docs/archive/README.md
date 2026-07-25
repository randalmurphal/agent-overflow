# Archive

Frozen documents from earlier phases of the project. **Not authoritative.**
Kept for history and context. Do not update these files.

## Contents

- `ARCHITECTURE.md` — original architecture brief. Superseded by the
  top-level `AGENTS.md` and the files under `docs/architecture/`.
- `IMPLEMENTATION.md`, `IMPLEMENTATION-PARITY.md` — implementation specs
  used by the ralph loops.
- `ralph/` — the ralph-loop artifacts that produced the initial codebase:
  `PROMPT-*.md`, `progress-*.md`, and the `ralph.sh` runner. These describe
  the *process* used to build the project, not its current behavior.
- `workflows-system-gap-analysis.md` — the pre-implementation punch-list for
  the workflows spec. Its "definitely add" items were folded into
  `docs/specs/workflows-system.md` (§12 is the teardown keystone it produced);
  archived at the rev-2 campaign close.
- `workflows-refit.md` — a 2026-07-14 discussion draft for a backlog-reconciler
  product layer on top of the workflows system. Never implemented, and its
  integration points (the chat enqueue tool, queue semantics) were removed by
  workflows rev 2 — read it as a problem statement, not a plan.

## If You're Looking For

- Current architecture → `/AGENTS.md` and `/docs/architecture/`.
- Current area rules → `AGENTS.md` next to the code you're editing.
- External references → `/docs/references/`.

If you find information in here that is load-bearing for today's code and
is missing from the live docs, treat that as a doc gap: fix it in the
live docs, not here.
