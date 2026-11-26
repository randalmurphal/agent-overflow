<script lang="ts">
  // How a widget's number is counted (docs/specs/issues.md, "Dashboards"),
  // as the board's computer words it from the metric's definition: the
  // metric's own words, what it adds up and for whom, what the range does
  // to it, the rules it reads, the tables it reads and its query. Shown in
  // the widget's place, so it reads the same under compact.
  import type { How } from '../../../../bindings/agent-overflow/internal/trackerdash/models';

  interface Props {
    how: How;
  }

  let { how }: Props = $props();
</script>

<div class="flex flex-col gap-1.5 text-[0.75rem] text-fg-muted" data-testid="dashboard-how">
  {#each how.words as line, i (i)}
    <p class="text-fg">{line}</p>
  {/each}
  <p>{how.counts}</p>
  <p>{how.range}</p>
  <p>Rules it reads: {how.rules}.</p>
  {#if how.reads.length > 0}
    <p>It reads {how.reads.join(', ')}.</p>
  {/if}
  <details class="mt-1">
    <summary class="cursor-pointer select-none text-fg-muted hover:text-fg">The query</summary>
    <pre class="mt-1 max-h-48 overflow-auto rounded-[var(--radius-control)] bg-surface-1 p-2 font-mono text-[0.6875rem] text-fg" data-testid="dashboard-how-sql">{how.sql}</pre>
  </details>
</div>
