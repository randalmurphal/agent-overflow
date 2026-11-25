<script lang="ts">
  // The editor of one of a Board card's fields, at its mark (or at the card
  // when Display hides the mark), loaded on the first change from a card.
  // The editor is the ticket screen's own (`ticketEditors.ts#cardEditor`):
  // a pick list (TicketPicker) for the type, priority and assignee, the
  // estimate typed (TicketTypedValue). Each edit is saved at once through
  // the ticket's write store; its pending state and outcome show on the
  // card and on the ticket screen alike. Closing it changes nothing.
  import Popover from '../primitives/Popover.svelte';
  import TicketPicker from './TicketPicker.svelte';
  import TicketTypedValue from './TicketTypedValue.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import { cardEditor, cardFieldName, pointsLabel, type CardEditField } from './ticketEditors';

  interface Props {
    ctx: TicketWriteContext;
    row: RowView;
    field: CardEditField;
    /** The board's estimate unit. */
    unit: string;
    /** The tracker board an estimate is set on. */
    board: string;
    /** The tracker has issue types. */
    typed: boolean;
    anchor: HTMLElement;
    onClose: () => void;
  }

  let { ctx, row, field, unit, board, typed, anchor, onClose }: Props = $props();

  const editor = $derived(cardEditor(ctx, row.card, field, { board, unit, estimate: row.estimate }, typed));
  const label = $derived(field === 'points' ? pointsLabel(unit) : cardFieldName(field));
  let invalid = $state('');
</script>

{#if editor?.kind === 'pick'}
  <TicketPicker {anchor} open {label} load={editor.load} checked={editor.checked} clearLabel={editor.clearLabel} multi={editor.multi} onPick={editor.onPick} {onClose} />
{:else if editor?.kind === 'type'}
  <Popover {anchor} open {onClose} placement="bottom-start" ariaLabel={label}>
    <div class="flex w-48 flex-col gap-1 p-1.5" data-testid="issues-card-typed">
      <TicketTypedValue {label} input={editor.input} text={editor.text} onSave={editor.onSave} onDone={onClose} bind:invalid />
      {#if invalid}
        <p class="text-xs text-error" role="alert" data-testid="ticket-field-invalid">{invalid}</p>
      {/if}
    </div>
  </Popover>
{/if}
