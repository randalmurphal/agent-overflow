<script lang="ts">
  // What a widget that counts your logged time says of its reads, on the
  // Dashboards screen and on the board's summary row alike: the days not
  // read yet once the board's computer stopped reading them, with why the
  // last read failed, and a known number's failed read again; each with
  // Read again (trackerapp/worklog_reads.go). A list that counts days
  // older than the logged time kept says so, with nothing to read again.
  // Nothing while the computer reads, or for a widget whose answer says
  // why it cannot count.
  import type { WidgetValue } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import type { LoggedTimeRead } from '../../stores/trackerDashboard.svelte';
  import { LOGGED_OLD_TEXT, notReadWords } from '../../stores/trackerDashboardModel';

  interface Props {
    value: WidgetValue | undefined;
    /** The widget's metric reads your logged time (How this is counted). */
    counts: boolean;
    logged: LoggedTimeRead | undefined;
  }

  let { value, counts, logged }: Props = $props();
  const unread = $derived(value?.unread?.length ?? 0);
  const old = $derived(!!value && !value.error && !value.note && counts && !!value.old);
  const said = $derived.by(() => {
    if (old) return `Not known: it counts days ${LOGGED_OLD_TEXT}.`;
    if (!value || value.error || value.note || !counts || !logged || logged.reading) return '';
    if (unread > 0) return `Not known: ${notReadWords(unread)} of your logged time.${logged.failure ? ` ${logged.failure}` : ''}`;
    return logged.failure ? `Your logged time was not read again: ${logged.failure}` : '';
  });
</script>

{#if said}
  <p class="flex flex-wrap items-baseline gap-x-2 text-[0.6875rem] text-fg-muted" data-testid="dashboard-widget-unread">
    <span>{said}</span>
    {#if !old}<button type="button" class="text-accent hover:underline" data-testid="dashboard-widget-unread-retry" onclick={() => logged?.retry()}>Read again</button>{/if}
  </p>
{/if}
