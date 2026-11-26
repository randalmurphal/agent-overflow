<script lang="ts">
  // The marks of a ticket's type, priority, estimate and assignee as a
  // Board card and a Backlog row draw them, in the order `fields` names
  // (docs/specs/issues.md, "Board" and "Backlog and sprint planning"). On a
  // pointer screen each opens that field's editor at the mark
  // (`CardEditor`), waiting while its write is out; under compact they only
  // show, and the menu changes them. Display hides any of them. The marks'
  // elements are bound into `els`, where the editor anchors.
  import PriorityIcon from './PriorityIcon.svelte';
  import TypeIcon from './TypeIcon.svelte';
  import ReviewAvatar from '../review/ReviewAvatar.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import { fieldTarget, type TicketWrite } from '../../stores/trackerTicketWrites.svelte';
  import { formatEstimate } from './boardFormat';
  import type { CardEditField } from './ticketEditors';

  interface Props {
    row: RowView;
    fields: readonly CardEditField[];
    hidden: ReadonlySet<CardField>;
    /** The board's estimate unit. */
    unit: string;
    /** The tracker has issue types (not Linear). */
    typed: boolean;
    /** The marks open their editors (not under compact). */
    editable: boolean;
    /** The ticket's writes (`ticketWrites`). */
    writes: ReadonlyMap<string, TicketWrite>;
    els: Partial<Record<CardEditField, HTMLElement>>;
    onEdit: (field: CardEditField) => void;
  }

  let { row, fields, hidden, unit, typed, editable, writes, els = $bindable(), onEdit }: Props = $props();

  const card = $derived(row.card);
  const estimate = $derived(formatEstimate(row.estimate, unit));
  const assigneeName = $derived(card.assignee ? card.assignee.name || 'Unknown user' : '');
  const estimated = $derived(unit !== 'none' && unit !== '');

  const TARGETS: Readonly<Record<CardEditField, string>> = {
    type: fieldTarget('type'), priority: fieldTarget('priority'), points: 'points', assignee: fieldTarget('assignee'),
  };
  const pending = (field: CardEditField) => writes.get(TARGETS[field])?.pending ?? false;
  const DISPLAYED: Readonly<Record<CardEditField, CardField>> = { type: 'type', priority: 'priority', points: 'estimate', assignee: 'assignee' };

  function edit(field: CardEditField): void {
    if (!pending(field)) onEdit(field);
  }

  const markClass = 'relative z-[1] inline-flex shrink-0 items-center rounded-sm focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50';
</script>

{#each fields as field (field)}
  {#if !hidden.has(DISPLAYED[field])}
    {#if field === 'type'}
      {#if editable && typed}
        <button bind:this={els.type} type="button" class={markClass} aria-label="Type: {card.type?.name ?? 'None'}" disabled={pending('type')} data-card-mark data-testid="issues-card-type-edit" onclick={() => edit('type')}>
          <TypeIcon type={card.type} testId="issues-card-type" />
        </button>
      {:else}
        <TypeIcon type={card.type} testId="issues-card-type" />
      {/if}
    {:else if field === 'priority'}
      {#if editable}
        <button bind:this={els.priority} type="button" class="{markClass} {card.priority ? '' : 'h-3.5 w-3.5 opacity-0 group-hover:opacity-100 focus-visible:opacity-100'}" aria-label="Priority: {card.priority?.name ?? 'None'}" disabled={pending('priority')} data-card-mark data-testid="issues-card-priority-edit" onclick={() => edit('priority')}>
          {#if card.priority}<PriorityIcon priority={card.priority} testId="issues-card-priority" />{:else}<span class="h-0.5 w-2.5 rounded-full bg-fg-muted"></span>{/if}
        </button>
      {:else}
        <PriorityIcon priority={card.priority} testId="issues-card-priority" />
      {/if}
    {:else if field === 'points'}
      {#if estimated && editable}
        <button bind:this={els.points} type="button" class="{markClass} rounded-sm bg-surface-2 px-1.5 tabular-nums text-fg {estimate === null ? 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100' : ''}" aria-label="{unit === 'time' ? 'Estimate' : 'Points'}: {estimate ?? 'None'}" disabled={pending('points')} data-card-mark data-testid="issues-card-estimate-edit" onclick={() => edit('points')}>
          {#if estimate !== null}<span data-testid="issues-card-estimate">{estimate}</span>{:else}–{/if}
        </button>
      {:else if estimated && estimate !== null}
        <span class="shrink-0 rounded-sm bg-surface-2 px-1.5 tabular-nums text-fg" title={unit === 'time' ? 'Estimate' : 'Points'} data-testid="issues-card-estimate">{estimate}</span>
      {/if}
    {:else if field === 'assignee'}
      {#if editable}
        <button bind:this={els.assignee} type="button" class="{markClass} rounded-full {card.assignee ? '' : 'h-5 w-5 justify-center border border-dashed border-border opacity-0 group-hover:opacity-100 focus-visible:opacity-100'}" aria-label="Assignee: {card.assignee ? assigneeName : 'Unassigned'}" disabled={pending('assignee')} data-card-mark data-testid="issues-card-assignee-edit" onclick={() => edit('assignee')}>
          {#if card.assignee}
            <span class="inline-flex" data-testid="issues-card-assignee"><ReviewAvatar login={card.assignee.id} name={card.assignee.name} title={assigneeName} size={20} /></span>
          {/if}
        </button>
      {:else if card.assignee}
        <span class="inline-flex" data-testid="issues-card-assignee">
          <ReviewAvatar login={card.assignee.id} name={card.assignee.name} title={assigneeName} size={20} />
        </span>
      {/if}
    {/if}
  {/if}
{/each}
