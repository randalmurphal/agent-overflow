<script lang="ts">
  // One card of the Board screen: the epic or parent, the title, labels,
  // then type, priority and key, the estimate and the assignee, and how
  // long the card has sat in its status. Display hides any of them but the
  // title. A card is not a control yet: the ticket screen opens from it in
  // a later change. The assignee is initials (ReviewAvatar); no avatar
  // image is fetched.
  import Bookmark from '@lucide/svelte/icons/bookmark';
  import Bug from '@lucide/svelte/icons/bug';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronUp from '@lucide/svelte/icons/chevron-up';
  import ChevronsDown from '@lucide/svelte/icons/chevrons-down';
  import ChevronsUp from '@lucide/svelte/icons/chevrons-up';
  import CornerDownRight from '@lucide/svelte/icons/corner-down-right';
  import Equal from '@lucide/svelte/icons/equal';
  import Square from '@lucide/svelte/icons/square';
  import SquareCheck from '@lucide/svelte/icons/square-check';
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import Zap from '@lucide/svelte/icons/zap';
  import Icon from '../primitives/Icon.svelte';
  import ReviewAvatar from '../review/ReviewAvatar.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import { daysInStatus, formatEstimate, priorityMark, typeMark, type PriorityMark, type TypeMark } from './boardFormat';

  interface Props {
    row: RowView;
    hidden: ReadonlySet<CardField>;
    /** The board's estimate unit. */
    unit: string;
    /** Now, to the minute. */
    now: number;
  }

  let { row, hidden, unit, now }: Props = $props();

  const TYPE_ICONS = {
    epic: { icon: Zap, tone: 'text-accent' },
    subtask: { icon: CornerDownRight, tone: 'text-fg-muted' },
    bug: { icon: Bug, tone: 'text-error' },
    story: { icon: Bookmark, tone: 'text-success' },
    task: { icon: SquareCheck, tone: 'text-info' },
    other: { icon: Square, tone: 'text-fg-muted' },
  } satisfies Record<TypeMark, unknown>;

  const PRIORITY_ICONS = {
    urgent: { icon: TriangleAlert, tone: 'text-error' },
    highest: { icon: ChevronsUp, tone: 'text-error' },
    high: { icon: ChevronUp, tone: 'text-warning' },
    medium: { icon: Equal, tone: 'text-warning' },
    low: { icon: ChevronDown, tone: 'text-info' },
    lowest: { icon: ChevronsDown, tone: 'text-info' },
    other: { icon: Equal, tone: 'text-fg-muted' },
  } satisfies Record<Exclude<PriorityMark, 'none'>, unknown>;

  const card = $derived(row.card);
  const type = $derived(TYPE_ICONS[typeMark(card.type)]);
  const priority = $derived(priorityMark(card.priority));
  const estimate = $derived(formatEstimate(row.estimate, unit));
  const days = $derived(daysInStatus(card.status_since, now));
  const parentText = $derived(card.parent ? card.parent.title || card.parent.key || '' : '');
  const assigneeName = $derived(card.assignee ? card.assignee.name || 'Unknown user' : '');
</script>

<article
  class="mb-1.5 flex flex-col gap-1.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-3 py-2 text-sm"
  data-testid="issues-card"
  data-issue={card.id}
>
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
      <span class="inline-flex {type.tone}" title={card.type?.name ?? 'No type'} data-testid="issues-card-type">
        <Icon icon={type.icon} size={13} strokeWidth={2} />
      </span>
    {/if}
    {#if priority !== 'none' && !hidden.has('priority')}
      {@const mark = PRIORITY_ICONS[priority]}
      <span class="inline-flex {mark.tone}" title="{card.priority?.name} priority" data-testid="issues-card-priority">
        <Icon icon={mark.icon} size={13} strokeWidth={2} />
      </span>
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
