<script lang="ts">
  // Renders a horizontal "Response" separator above the assistant message
  // that closed a completed turn. Driven by `pane.latestSettledTurn`; the
  // owning <MessageTimeline> decides WHEN to mount this (before the leaf
  // whose id matches turn.assistantMessageId) and passes the settled-turn
  // projection in. Pure presentation — no side effects, no reactivity
  // beyond the incoming prop.
  //
  // Spec: docs/architecture/turn-lifecycle.md §UI components driven by
  // this state — "Separator rendered before the item whose id matches
  // assistantMessageId. Label 'Response • Worked for Xs · Yk tokens'."

  import type { SettledTurn } from '../../stores/thread.svelte';
  import { formatElapsedSeconds, formatTurnTokens } from '../../utils/format';

  interface Props {
    turn: SettledTurn;
  }

  let { turn }: Props = $props();

  // Elapsed is measured in whole seconds. Clamp negative deltas (backend
  // clock skew) to zero; completedAt < startedAt is never a legitimate
  // state but we prefer a quiet "0s" over a negative render artifact.
  let elapsedSeconds = $derived(
    Math.max(0, Math.floor((turn.completedAt - turn.startedAt) / 1000)),
  );

  // Sum input + output for the "Xk tokens" suffix. tokenUsage can be
  // null (provider didn't report usage) OR present with a zero sum
  // (rare, but e.g. a zero-token interrupt). Both collapse to "no
  // meaningful count" and suppress the tokens section.
  let totalTokens = $derived.by<number>(() => {
    const u = turn.tokenUsage;
    if (!u) return 0;
    const input = Number.isFinite(u.inputTokens) ? u.inputTokens : 0;
    const output = Number.isFinite(u.outputTokens) ? u.outputTokens : 0;
    return input + output;
  });

  // Base label precedence matches the task spec:
  //   1. aborted  -> "Interrupted"
  //   2. non-empty errorMessage -> "Error"
  //   3. otherwise -> "Response"
  // Aborted wins over error because the user-initiated stop is the
  // "reason" we want surfaced; a best-effort error string captured
  // alongside an interrupt is still a user cancellation first.
  let baseLabel = $derived.by<string>(() => {
    if (turn.aborted) return 'Interrupted';
    if (turn.errorMessage && turn.errorMessage.length > 0) return 'Error';
    return 'Response';
  });

  let label = $derived.by<string>(() => {
    let out = baseLabel;
    if (elapsedSeconds > 0) {
      out += ` • Worked for ${formatElapsedSeconds(elapsedSeconds)}`;
    }
    if (totalTokens > 0) {
      out += ` · ${formatTurnTokens(totalTokens)}`;
    }
    return out;
  });

  let showErrorLine = $derived(
    !turn.aborted && turn.errorMessage && turn.errorMessage.length > 0,
  );
</script>

<div data-testid="completion-divider" data-turn-id={turn.turnId}>
  <div class="my-3 flex items-center gap-3">
    <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
    <span
      class="rounded-full border border-border bg-surface-1 px-2.5 py-1 text-[10px] uppercase tracking-[0.14em] text-text-secondary"
      data-testid="completion-divider-label"
    >{label}</span>
    <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
  </div>
  {#if showErrorLine}
    <div
      class="mb-3 text-center text-xs text-error"
      data-testid="completion-divider-error"
      role="status"
    >{turn.errorMessage}</div>
  {/if}
</div>
