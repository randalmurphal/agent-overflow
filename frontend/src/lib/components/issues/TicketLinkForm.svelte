<script lang="ts">
  // The form that links the ticket to another (docs/specs/issues.md,
  // "Ticket"): how the ticket relates, one phrase of each of the
  // connection's link types ("blocks", "is blocked by"), and the other
  // ticket, found by key or words (TicketFinder). The phrase decides the
  // side the ticket takes (`linkRequest`). Link sends once through the
  // write store's `new-link` target; every input takes no change while it
  // is out, so what is linked is what the form shows. The form closes once
  // the link was made and says why when it was not. The caller wraps it in
  // a Popover.
  import { untrack } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import TicketFinder, { type FoundTicket } from './TicketFinder.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { linkRelations } from '../../stores/trackerTicketModel';
  import {
    linkTicket,
    loadTicketLinkTypes,
    readOnFormRefresh,
    ticketLinkTypes,
    ticketWrite,
    type TicketWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import { errString } from '../../utils/errors';

  interface Props {
    ctx: TicketWriteContext;
    /** The form is done: the link was made, or the person closed it. */
    onDone: () => void;
  }

  let { ctx, onDone }: Props = $props();

  // Opening the form, Retry and the ticket's Refresh read the types when
  // what is held is old (the store's form read rules).
  function readTypes(): void {
    void loadTicketLinkTypes(ctx.ref.backend, ctx.ref.connection);
  }
  $effect(() => {
    untrack(readTypes);
  });
  readOnFormRefresh(() => ctx.ref, readTypes);
  const types = $derived(ticketLinkTypes(ctx.ref.backend, ctx.ref.connection));
  const relations = $derived(types?.value ? linkRelations(types.value) : []);
  const typesError = $derived(types?.error ? errString(types.error) : '');

  let relationIndex = $state(0);
  const relation = $derived(relations[relationIndex] ?? null);
  let other = $state<FoundTicket | null>(null);
  let finding = $state(false);
  let findAnchor = $state<HTMLElement | undefined>();
  let problem = $state('');

  const pending = $derived(ticketWrite(ctx.key, 'new-link')?.pending ?? false);

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (pending) return;
    problem = '';
    if (!relation) {
      problem = 'Pick how the tickets relate.';
      return;
    }
    if (!other) {
      problem = 'Pick the ticket to link to.';
      return;
    }
    const outcome = await linkTicket(ctx, relation, other.issue);
    if (outcome?.kind === 'applied') onDone();
  }

  const inputClass = 'w-full rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60';
</script>

<form class="flex w-[min(20rem,calc(100vw-2rem))] flex-col gap-2 p-3" data-testid="ticket-link-form" onsubmit={submit}>
  <h3 class="text-sm font-semibold text-fg">Link {ctx.label} to a ticket</h3>
  {#if typesError}
    <div class="flex flex-col items-start gap-1 text-xs">
      <span class="text-error" role="alert" data-testid="ticket-link-form-types-error">Could not read how tickets can relate: {typesError}</span>
      <button type="button" class="text-accent hover:underline" data-testid="ticket-link-form-types-retry" onclick={readTypes}>Retry</button>
    </div>
  {:else if !types?.value}
    <span class="text-xs text-fg-muted" data-testid="ticket-link-form-types-loading">Reading how tickets can relate…</span>
  {:else}
    <label class="flex flex-col gap-1 text-xs text-fg-muted">
      {ctx.label}
      <select class={inputClass} value={relationIndex} onchange={(event) => { relationIndex = event.currentTarget.selectedIndex; }} disabled={pending} data-testid="ticket-link-form-relation">
        {#each relations as r, i (`${r.type}:${r.outward}`)}
          <option value={i}>{r.phrase}</option>
        {/each}
      </select>
    </label>
  {/if}
  <div class="flex min-w-0 items-baseline gap-1.5 text-sm" data-testid="ticket-link-form-other">
    {#if other}
      <span class="shrink-0 font-medium text-fg">{other.key || `ID ${other.issue}`}</span>
      <span class="min-w-0 truncate text-fg-muted">{other.title}</span>
    {/if}
    <button
      type="button"
      class="shrink-0 text-xs text-accent hover:underline disabled:opacity-60"
      disabled={pending}
      bind:this={findAnchor}
      data-testid="ticket-link-form-find"
      onclick={() => { finding = true; }}
    >{other ? 'Another ticket' : 'Pick a ticket'}</button>
  </div>
  <TicketFinder
    anchor={findAnchor}
    open={finding}
    sources={[{ backend: ctx.ref.backend, connection: ctx.ref.connection, name: '' }]}
    onPick={(t) => { other = t; problem = ''; }}
    onClose={() => { finding = false; }}
  />
  {#if problem}
    <p class="text-xs text-error" role="alert" data-testid="ticket-link-form-problem">{problem}</p>
  {/if}
  <TicketWriteNote ticketKey={ctx.key} target="new-link" fieldName={(id) => id} />
  <div class="flex items-center gap-1.5">
    <Button type="submit" size="sm" variant="primary" disabled={pending || !types?.value} testId="ticket-link-form-save">{pending ? 'Linking…' : 'Link'}</Button>
    <Button size="sm" variant="ghost" testId="ticket-link-form-cancel" onclick={onDone}>Cancel</Button>
  </div>
</form>
