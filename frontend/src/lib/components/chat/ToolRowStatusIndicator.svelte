<script lang="ts">
  // The single status-indicator wrapper for every tool-row header
  // (GenericToolCallRow, AgentRow, AdvisorRow, CommandOutput,
  // AskUserQuestionCard, SubagentGroup, CollabToolRow,
  // TerminalInteractionRow, ToolResultCard). Centralises the
  // `data-status` / `data-state` hooks so any future attribute drift
  // (status taxonomy, styling) updates in one place instead of many
  // near-identical templates — and so the line-box fix below can't be
  // reintroduced by a one-off wrapper elsewhere.
  //
  // Accessibility lives on the inner `Indicator` dot, not here: the dot
  // carries `role="status"` (the live region a screen reader announces)
  // plus the state-derived `aria-label`. This wrapper deliberately does
  // NOT repeat that label — a second aria-label on this generic span
  // would announce the same dot twice. Don't re-add it.
  //
  // The wrapper MUST establish a flex context (`inline-flex`), never a
  // plain inline box. The running dot is a 6px `inline-block`; inside a
  // plain inline span it sits in a line box sized by the inherited 24px
  // line-height, so a running row renders ~7px taller than its settled
  // height. On completion the dot is removed and the row snaps shorter —
  // a bottom-pinned scroll surface compensates the shrink instantly,
  // which reads as a small downward shift of everything above the row.
  // `inline-flex` makes the wrapper track the dot's real height, so
  // running and settled rows are the same height. See
  // components/chat/CLAUDE.md "Row Contract" (stable outer shell across
  // completion).
  import Indicator from './Indicator.svelte';
  import type { IndicatorState } from './rowState';
  import type { Item } from '../../types/models';

  let {
    item,
    state,
    testId,
  }: {
    item: Pick<Item, 'status'>;
    state: IndicatorState;
    // Optional: rows that assert on the status element pass an id;
    // rows that only need the dot omit it (no `data-testid` is emitted).
    testId?: string;
  } = $props();
</script>

{#if state}
  <span
    class="inline-flex items-center"
    data-testid={testId}
    data-status={item.status}
    data-state={state}
  >
    <Indicator {state} />
  </span>
{/if}
