<script lang="ts">
  // The card menu's Move to… page: every column but the card's own, those
  // a move of the ticket reaches (one row per move when several reach a
  // column) and the others disabled, saying so. It reads the ticket's
  // moves once, when it opens (TrackerMoves, one tracker request on Jira).
  import MenuItem from '../primitives/MenuItem.svelte';
  import type { ColumnView, MoveView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { columnsReached } from '../../stores/trackerBoardModel';
  import { ticketMoves, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import { errString } from '../../utils/errors';

  interface Props {
    ctx: TicketWriteContext;
    columns: readonly ColumnView[];
    /** The card's own column. */
    from: number;
    onPick: (column: number, move: MoveView) => void;
  }

  let { ctx, columns, from, onPick }: Props = $props();

  type Read = { readonly state: 'reading' } | { readonly state: 'read'; readonly reached: Map<number, MoveView[]> } | { readonly state: 'failed'; readonly error: unknown };
  let read = $state.raw<Read>({ state: 'reading' });
  // svelte-ignore state_referenced_locally
  ticketMoves(ctx).then(
    (moves) => { read = { state: 'read', reached: columnsReached(columns, moves) }; },
    (error: unknown) => { read = { state: 'failed', error }; },
  );
</script>

{#if read.state === 'reading'}
  <MenuItem label="Reading where {ctx.label} can move…" disabled testId="issues-card-move-reading" />
{:else if read.state === 'failed'}
  <MenuItem label="Could not read where {ctx.label} can move" description={errString(read.error)} disabled wrap testId="issues-card-move-failed" />
{:else}
  {@const reached = read.reached}
  {#each columns as col, i (i)}
    {#if i !== from}
      {@const moves = reached.get(i) ?? []}
      {#if moves.length === 0}
        <MenuItem label={col.name} description="No move reaches it" disabled testId="issues-card-move-column" />
      {:else}
        {#each moves as move (move.id)}
          <MenuItem
            label={moves.length > 1 ? `${col.name}: ${move.name}` : col.name}
            description={move.fields?.length ? 'Asks for fields' : undefined}
            testId="issues-card-move-column"
            onSelect={() => onPick(i, move)}
          />
        {/each}
      {/if}
    {/if}
  {/each}
{/if}
