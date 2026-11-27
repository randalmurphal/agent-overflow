<script lang="ts">
  // The tickets behind one number of a dashboard (docs/specs/issues.md,
  // "Dashboards"): the number and its rows from one evaluation of their
  // own, so the two always agree, a page of 100 at a time. A page asked
  // for after the rows changed starts again at the first, and says so. A
  // row opens its ticket, which closes the drill; Escape, Back and Close
  // close it.
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import type { TicketRow } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Modal from '../primitives/Modal.svelte';
  import ErrorNotice from '../shared/ErrorNotice.svelte';
  import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
  import type { DrillState } from '../../stores/trackerDashboard.svelte';
  import { drillCountWords, FIRST_READ_TEXT, numWords, type Num } from '../../stores/trackerDashboardModel';
  import { formatAmount, unitWord, type DashboardUnit } from '../../utils/dashboardAmount';

  interface Props {
    drill: DrillState;
    onPage: (page: number) => void;
    onTicket: (row: TicketRow) => void;
    onClose: () => void;
  }

  let { drill, onPage, onTicket, onClose }: Props = $props();

  const answer = $derived(drill.answer);
  const unit = $derived((answer?.unit ?? 'tickets') as DashboardUnit);
  const valueWords = $derived(answer ? numWords(answer.value as Num, unit, answer.estimates) : null);
  const atFirst = $derived(drill.loading || !answer || answer.page === 0);
  const atLast = $derived(drill.loading || !answer || answer.page >= answer.pages - 1);
  // Captured once per failure, so Details and copy describe the one shown.
  let failure: CapturedError | null = $state(null);
  let failureOf: unknown = null;
  $effect(() => {
    const err = drill.error;
    if (err === failureOf) return;
    failureOf = err;
    failure = err ? captureError(err, { context: "Could not list this number's tickets" }) : null;
  });

  function rowValue(row: TicketRow): string {
    if (row.value === undefined || row.value === null || !answer) return '';
    const word = unitWord(row.value, unit, answer.estimates);
    return `${formatAmount(row.value, unit, answer.estimates)}${word ? ` ${word}` : ''}`;
  }
</script>

<Modal open={true} title={answer?.title ?? 'Tickets'} {onClose} width="lg" padding="none">
  <div class="flex min-h-0 flex-col" data-testid="dashboard-drill" aria-busy={drill.loading}>
    {#if failure}
      <ErrorNotice message="Could not list this number's tickets: {failure.report.reason}" captured={failure} testId="dashboard-drill-error" />
    {/if}
    {#if answer}
      <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1 border-b border-border-subtle px-5 py-3">
        <span class="text-xl font-semibold tabular-nums text-fg" data-testid="dashboard-drill-value">{valueWords?.value}</span>
        {#if valueWords?.notCounted}
          <span class="text-[0.75rem] text-fg-muted" data-testid="dashboard-drill-value-not-counted">{valueWords.notCounted}</span>
        {/if}
        <span class="text-[0.75rem] text-fg-muted" data-testid="dashboard-drill-count">{drillCountWords(answer.count, answer.tickets)}</span>
      </div>
      {#if answer.reading}
        <p class="border-b border-border-subtle bg-surface-1 px-5 py-1.5 text-[0.75rem] text-fg-muted" role="status" data-testid="dashboard-drill-reading">{FIRST_READ_TEXT}</p>
      {/if}
      {#if answer.restarted}
        <p class="border-b border-border-subtle bg-surface-1 px-5 py-1.5 text-[0.75rem] text-fg-muted" role="status" data-testid="dashboard-drill-restarted">The tickets changed while you paged, so this is the first page again, counted now.</p>
      {/if}
      <ul class="min-h-0 flex-1 overflow-y-auto" data-testid="dashboard-drill-rows">
        {#each answer.rows as row (row.row)}
          <li class="border-b border-border-subtle last:border-b-0">
            <button
              type="button"
              class="flex w-full min-w-0 items-baseline gap-2 px-5 py-1.5 text-left text-[0.75rem] hover:bg-surface-1 focus:outline-none focus-visible:bg-surface-1"
              data-testid="dashboard-drill-row"
              data-counted={row.counted}
              onclick={() => onTicket(row)}
            >
              <span class="shrink-0 font-mono text-[0.6875rem] text-fg-muted">{row.key}</span>
              <!-- Under compact the note goes under the title, which keeps its width. -->
              <span class="flex min-w-0 flex-1 items-baseline gap-2 compact:flex-col compact:items-start compact:gap-0">
                <span class="w-full min-w-0 flex-1 truncate {row.counted ? 'text-fg' : 'text-fg-muted'}" data-testid="dashboard-drill-title">{row.title}</span>
                {#if !row.counted}
                  <span class="max-w-[16rem] shrink-0 truncate text-[0.6875rem] text-fg-muted compact:max-w-full" data-testid="dashboard-drill-not-counted">Not counted{row.note ? `: ${row.note}` : ''}</span>
                {:else if row.note}
                  <span class="max-w-[12rem] shrink-0 truncate text-[0.6875rem] text-fg-muted compact:max-w-full" data-testid="dashboard-drill-note">{row.note}</span>
                {/if}
              </span>
              {#if row.period}<span class="max-w-[8rem] shrink-0 truncate text-[0.6875rem] text-fg-muted" data-testid="dashboard-drill-period">{row.period}</span>{/if}
              {#if row.counted && rowValue(row)}<span class="shrink-0 tabular-nums text-[0.6875rem] text-fg" data-testid="dashboard-drill-row-value">{rowValue(row)}</span>{/if}
              <span class="max-w-[8rem] shrink-0 truncate text-[0.6875rem] text-fg-muted">{row.status}</span>
            </button>
          </li>
        {:else}
          <li class="px-5 py-4 text-[0.75rem] text-fg-muted">No tickets.</li>
        {/each}
      </ul>
      {#if answer.pages > 1}
        <div class="flex items-center gap-2 border-t border-border-subtle px-5 py-2 text-[0.75rem] text-fg-muted">
          <Button variant="ghost" size="sm" disabled={atFirst} onclick={() => answer && onPage(answer.page - 1)} testId="dashboard-drill-prev">
            <Icon icon={ChevronLeft} size={13} strokeWidth={2} /> Previous
          </Button>
          <span class="flex-1 text-center" data-testid="dashboard-drill-page">Page {answer.page + 1} of {answer.pages}</span>
          <Button variant="ghost" size="sm" disabled={atLast} onclick={() => answer && onPage(answer.page + 1)} testId="dashboard-drill-next">
            Next <Icon icon={ChevronRight} size={13} strokeWidth={2} />
          </Button>
        </div>
      {/if}
    {:else if drill.loading}
      <p class="px-5 py-4 text-[0.75rem] text-fg-muted" data-testid="dashboard-drill-loading">Listing the tickets…</p>
    {/if}
  </div>
</Modal>
