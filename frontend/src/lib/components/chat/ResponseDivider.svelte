<!--
  The line that separates a turn's activity from the prose that follows it.

  Two visual modes share a fixed wrapper height (`h-[1.625rem]` = 26px =
  pill chrome: text-[0.625rem] × leading-tight ≈ 12px + py-1 8px + 2× 1px
  border). Labeled mode renders `line | gap | pill | gap | line`, unlabeled
  mode renders one continuous full-width line. The pill's `leading-tight`
  keeps its content inside the fixed wrapper across font-loading variance.
  Fixed `h-` (not the codebase's usual `min-h-` slot convention) is
  deliberate: both branches MUST be the exact same height so promoting an
  intermediate divider to "final" on turn settle never shifts row geometry,
  which satisfies the "no late transcript adornments on completion" rule in
  `frontend/AGENTS.md`. Re-derive 1.625rem if the pill classes change.
-->
<script lang="ts">
  let { final, label, duration = '' }: {
    /** The row closes its turn: show the labeled pill. */
    final: boolean;
    label: string;
    duration?: string;
  } = $props();
</script>

<div data-testid="response-divider" data-final-response={final ? 'true' : 'false'}>
  <div class="my-3 flex h-[1.625rem] items-center gap-3">
    <span class="timeline-hairline flex-1" aria-hidden="true"></span>
    {#if final}
      <span
        class="rounded-full border border-border bg-surface-1 px-2.5 py-1 text-[0.625rem] uppercase leading-tight tracking-[0.14em] text-text-secondary"
      >
        {label}{#if duration}{' '}<span class="normal-case tabular-nums tracking-normal">{duration}</span>{/if}
      </span>
      <span class="timeline-hairline flex-1" aria-hidden="true"></span>
    {/if}
  </div>
</div>
