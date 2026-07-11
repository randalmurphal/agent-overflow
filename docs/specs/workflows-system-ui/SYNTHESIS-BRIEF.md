# Synthesis Brief — one mockup from the design-round verdict

Read first: `BRIEF.md` (canonical dataset + the seven moments still apply), then
`docs/specs/workflows-system-decisions.md` **D11 and D12** (the verdict — this brief
implements it), then skim the three prior concepts (`concept-a.html`, `concept-b.html`,
`concept-c.html`) to MINE their interaction details — not their visual density.

The user's critique, verbatim, is the aesthetic contract:

> "all of these UIs suck in their own ways, too busy, too many badges, my attention
> isnt clearly being drawn to what I need to give attention to from the normal view.
> Should be minimalistic but with enough information to show what it needs to show to
> be able to dig in further where it matters. And it should use the existing
> behaviors/look from normal threads where it makes sense."

## The synthesized IA (locked — do not re-litigate)

1. **Reframe:** this is a *background jobs* surface. Most of the user's work happens
   in normal threads; workflows are scheduled/triggered automations and custom tasks
   (research runs, Jira backlog triage). Design it as a calm secondary surface that
   only speaks up when something needs the human.
2. **Sidebar:** a separate, collapsed per-project section (Concept B's placement)
   whose rows LOOK LIKE the existing thread rows — study how AO's sidebar renders a
   thread row with a single status pill (frontend/src/lib/components/sidebar/, and
   the pill logic in frontend/src/lib/utils/threadStatusPill.ts) and match that
   restraint exactly. One signal per row. An item row expands (dropdown) to its
   phases; clicking the running phase opens it as a NORMAL thread pane where the
   user can watch/steer/finish that phase.
3. **Overview pane:** Concept A's board layout, but hosted as a persistent PANE in
   the pane row (a first-class multi-pane citizen alongside thread panes), not a
   full-page takeover. It's the one place with columns/filters.
4. **Inspection is a modal** (not a slide-over): opened from a board card or sidebar
   row. The modal absorbs the stepper: j/k (or ←/→) steps across needs-attention
   items; a/r/t act. Content: what-happened digest, diff summary + checks + cost,
   the question (if any) with quick answers, and the action row.
5. **NO INTERNALS:** no variables, no envelopes, no JSON, no gate traces anywhere.
   Phase detail for a human = narrative digest, diff, checks status, cost. Period.
6. **Hand-to-agent:** on done/stuck/failed items the primary affordance next to the
   state is "continue with agent" — spawns a pre-seeded thread (show the transition:
   modal → new thread pane with a context chip).
7. **PRs:** the item modal / done card exposes: open PR, view review comments, send
   comments to the agent — visually consistent with AO's existing review affordances.
8. **Job notes (D12):** the item modal has a small "notes" affordance for
   scheduled/triggered jobs (continuity across runs). Show it on the automation-born
   item (canonical item 6) only — one quiet line, expandable.

## Aesthetic rules (hard)

- Fewer badges than ANY of the three concepts. One status signal per row/card;
  color appears only where state demands attention (amber needs-you, red failed);
  running/queued/done are typographic, not chromatic.
- Match AO's actual tokens (app.css) and its existing sidebar/pane chrome so the
  section reads as native, not as a bolted-on dashboard.
- Whitespace and typography do the hierarchy work. If an element doesn't change what
  the user does next, cut it.
- The "normal view" test: with nothing needing attention, the workflows presence is
  nearly invisible — a quiet section header and calm rows.

## Deliverable

One file: `docs/specs/workflows-system-ui/synthesis.html` — same constraints as the
concept round (fully self-contained, inline CSS/JS, no external requests, fixed
header naming it "Synthesis", internal navigation across the moments, functional
keyboard modal-stepper, annotations toggle for rationale). Cover the seven BRIEF.md
moments plus the hand-to-agent transition and the PR affordances. Use the canonical
dataset unchanged.
