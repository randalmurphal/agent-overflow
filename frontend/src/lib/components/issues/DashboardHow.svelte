<script lang="ts">
  // How a widget's number is counted (docs/specs/issues.md, "Dashboards"),
  // as the board's computer words it from the metric's definition: the
  // metric's own words, what it adds up and for whom, what the range does
  // to it, the rules it reads, the tables it reads and its query. Shown in
  // the widget's place, so it reads the same under compact. A count of
  // threads reads the board's computer's (`computer`), which the Ticket
  // screen's list, of every attached computer, can exceed: it says so. A
  // count of logged time says which days the page reads
  // (trackerDashboard's reads, trackerdash worklogs.go).
  import type { How } from '../../../../bindings/agent-overflow/internal/trackerdash/models';

  interface Props {
    how: How;
    /** The name of the computer that counts the board. */
    computer: string;
  }

  let { how, computer }: Props = $props();
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
  {#if how.reads.includes('worklogs')}
    <p data-testid="dashboard-how-worklogs">It counts your own logged time. While it shows, this page reads from the tracker the days of the range, and of the range before it compares with, that were not read yet. A day more than 180 days ago is not read, so a number that counts one is not known.</p>
  {/if}
  {#if how.reads.includes('threads')}
    <p data-testid="dashboard-how-threads">It counts the threads on {computer} linked to these tickets; a ticket's Work lists those of every attached computer.</p>
  {/if}
  <details class="mt-1">
    <summary class="cursor-pointer select-none text-fg-muted hover:text-fg">The query</summary>
    <pre class="mt-1 max-h-48 overflow-auto rounded-[var(--radius-control)] bg-surface-1 p-2 font-mono text-[0.6875rem] text-fg" data-testid="dashboard-how-sql">{how.sql}</pre>
  </details>
</div>
