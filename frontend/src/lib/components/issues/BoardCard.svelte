<script lang="ts">
  // One card of the Board screen: the epic or parent, the title, labels,
  // then type, priority and key, the estimate and the assignee, and how
  // long the card has sat in its status. Display hides any of them but the
  // title. A click opens its ticket, and its menu (right-click, or a long
  // press under compact) lists what can be done with it; the open ticket's
  // card is marked. The assignee is initials (ReviewAvatar); no avatar
  // image is fetched.
  import PriorityIcon from './PriorityIcon.svelte';
  import TypeIcon from './TypeIcon.svelte';
  import ReviewAvatar from '../review/ReviewAvatar.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import { daysInStatus, formatEstimate } from './boardFormat';

  interface Props {
    row: RowView;
    hidden: ReadonlySet<CardField>;
    /** The board's estimate unit. */
    unit: string;
    /** Now, to the minute. */
    now: number;
    /** Its ticket is the one open. */
    open: boolean;
    onOpen: (row: RowView) => void;
    onMenu: (row: RowView, event: MouseEvent) => void;
  }

  let { row, hidden, unit, now, open, onOpen, onMenu }: Props = $props();

  const card = $derived(row.card);
  const estimate = $derived(formatEstimate(row.estimate, unit));
  const days = $derived(daysInStatus(card.status_since, now));
  const parentText = $derived(card.parent ? card.parent.title || card.parent.key || '' : '');
  const assigneeName = $derived(card.assignee ? card.assignee.name || 'Unknown user' : '');
</script>

<article
  class="relative mb-1.5 flex flex-col gap-1.5 rounded-[var(--radius-field)] border bg-surface-1 px-3 py-2 text-sm {open ? 'border-accent' : 'border-border-subtle hover:border-border'}"
  data-testid="issues-card"
  data-issue={card.id}
  data-open={open ? '' : undefined}
  oncontextmenu={(event) => { event.preventDefault(); onMenu(row, event); }}
>
  <!-- The whole card opens its ticket; its text stays plain under the button. -->
  <button
    type="button"
    class="absolute inset-0 cursor-pointer rounded-[var(--radius-field)] focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:select-none"
    aria-label="Open {card.key}: {card.title}"
    aria-current={open ? 'true' : undefined}
    data-testid="issues-card-open"
    onclick={() => onOpen(row)}
  ></button>
  {#if parentText && !hidden.has('parent')}
    <span class="max-w-full self-start truncate rounded-sm bg-accent/10 px-1.5 text-[0.6875rem] text-accent" title={card.parent?.key ? `${card.parent.key} ${parentText}` : parentText} data-testid="issues-card-parent">{parentText}</span>
  {/if}
  <span class="leading-snug text-fg [overflow-wrap:anywhere]" data-testid="issues-card-title">{card.title}</span>
  {#if card.labels?.length && !hidden.has('labels')}
    <div class="flex flex-wrap gap-1" data-testid="issues-card-labels">
      {#each card.labels as label (label.id)}
        <span class="rounded-sm bg-surface-2 px-1.5 text-[0.6875rem] text-fg-muted">{label.name || label.id}</span>
      {/each}
    </div>
  {/if}
  <div class="flex min-w-0 items-center gap-1.5 text-[0.75rem] text-fg-muted">
    {#if !hidden.has('type')}
      <TypeIcon type={card.type} testId="issues-card-type" />
    {/if}
    {#if !hidden.has('priority')}
      <PriorityIcon priority={card.priority} testId="issues-card-priority" />
    {/if}
    {#if !hidden.has('key')}
      <span class="truncate tabular-nums" data-testid="issues-card-key">{card.key}</span>
    {/if}
    <span class="flex-1"></span>
    {#if estimate !== null && !hidden.has('estimate')}
      <span class="shrink-0 rounded-sm bg-surface-2 px-1.5 tabular-nums text-fg" title={unit === 'time' ? 'Estimate' : 'Points'} data-testid="issues-card-estimate">{estimate}</span>
    {/if}
    {#if card.assignee && !hidden.has('assignee')}
      <span class="inline-flex" data-testid="issues-card-assignee">
        <ReviewAvatar login={card.assignee.id} name={card.assignee.name} title={assigneeName} size={20} />
      </span>
    {/if}
  </div>
  {#if days > 0 && !hidden.has('days')}
    <span class="text-[0.6875rem] text-fg-muted" data-testid="issues-card-days">{days} {days === 1 ? 'day' : 'days'} in {card.status.name || 'this status'}</span>
  {/if}
</article>
