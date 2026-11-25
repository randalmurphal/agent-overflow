<script lang="ts">
  // A Jira ticket's planning poker (docs/specs/issues.md, "Ticket"), the
  // body of its folding section, loaded when the section first opens. While
  // this body is on screen in the ticket's scroller it tells the ticket's
  // surface so (`setPokerShown`), and the ticket store follows the votes
  // only then (stores/trackerTicket.svelte.ts). It shows each voter with
  // the value the read gives, "voted" where the read gives none, the
  // person's own vote marked, whether the votes are revealed and when they
  // were read. A vote is a card of the deck or a typed number; Reveal shows
  // every vote; Clear votes, which removes everyone's, takes a second
  // press. Once revealed, each value voted offers to set the estimate on
  // the ticket's board row, most votes first. A poker failure shows here
  // and nowhere else.
  import type { TicketPoker, TrackerTicketAttachment } from '../../stores/trackerTicket.svelte';
  import { pokerResult } from '../../stores/trackerTicketModel';
  import {
    clearTicketPokerVotes,
    revealTicketPoker,
    ticketWrite,
    voteTicketPoker,
    type TicketWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';

  interface Props {
    attachment: TrackerTicketAttachment;
    ctx: TicketWriteContext;
    poker: TicketPoker | null;
    /** Sets the ticket's estimate on its board row; null when no board
     * the app mirrors shows the ticket. */
    onSetEstimate: ((value: number) => void) | null;
    /** The board row estimates in hours, not points. */
    hours: boolean;
  }

  let { attachment, ctx, poker, onSetEstimate, hours }: Props = $props();

  /** The deck's cards; any other number from 0 to 1000 is typed. */
  const DECK = [0, 1, 2, 3, 5, 8, 13, 21] as const;
  const MAX_VOTE = 1000;

  const view = $derived(poker?.view ?? null);
  const readAt = $derived(view?.read_at ?? 0);
  const pending = $derived(ticketWrite(ctx.key, 'poker')?.pending ?? false);
  const settingEstimate = $derived(ticketWrite(ctx.key, 'points')?.pending ?? false);
  const mine = $derived(view?.votes?.find((v) => v.mine) ?? null);
  const result = $derived(view?.revealed ? pokerResult(view.votes ?? []) : []);
  const failure = $derived(poker?.error ? errString(poker.error) : view?.failure?.message ?? '');

  // On screen: inside the ticket's scroller viewport. The section's fold
  // mounts and unmounts this body, which is the other half.
  function watchOnScreen(node: HTMLElement): () => void {
    const root = node.closest<HTMLElement>('[data-pane-scroller]');
    if (typeof IntersectionObserver === 'undefined') {
      attachment.setPokerShown(true);
      return () => attachment.setPokerShown(false);
    }
    const observer = new IntersectionObserver((entries) => {
      const entry = entries.at(-1);
      if (entry) attachment.setPokerShown(entry.isIntersecting);
    }, { root });
    observer.observe(node);
    return () => {
      observer.disconnect();
      attachment.setPokerShown(false);
    };
  }

  let typed = $state('');
  const typedValue = $derived(typed.trim() === '' ? null : Number(typed));
  const typedValid = $derived(typedValue !== null && Number.isFinite(typedValue) && typedValue >= 0 && typedValue <= MAX_VOTE);

  function vote(value: number): void {
    clearing = false;
    void voteTicketPoker(ctx, value);
  }

  function voteTyped(event: SubmitEvent): void {
    event.preventDefault();
    if (!typedValid || typedValue === null) return;
    vote(typedValue);
    typed = '';
  }

  let clearing = $state(false);
  let deck: HTMLElement | undefined = $state();
  let clearButton: HTMLButtonElement | undefined = $state();

  /** The menu's Vote: the caret on the first card. */
  export function focusVote(): void {
    const first = deck?.querySelector<HTMLButtonElement>('[data-testid="ticket-poker-card"]');
    first?.scrollIntoView({ block: 'nearest' });
    first?.focus();
  }

  /** The menu's Clear votes: its first press; the second is the
   * person's, here. */
  export function askClear(): void {
    clearing = true;
    clearButton?.scrollIntoView({ block: 'nearest' });
    clearButton?.focus();
  }

  function clearVotes(): void {
    if (!clearing) {
      clearing = true;
      return;
    }
    clearing = false;
    void clearTicketPokerVotes(ctx);
  }
</script>

<div class="flex flex-col gap-2" data-testid="ticket-poker" {@attach watchOnScreen}>
  {#if failure}
    <p class="text-xs text-error" role="alert" data-testid="ticket-poker-failure">Planning poker: {failure}</p>
  {/if}
  {#if !view}
    <p class="text-xs text-fg-muted" data-testid="ticket-poker-reading">Reading the votes…</p>
  {:else}
    <ul class="flex flex-col gap-0.5" data-testid="ticket-poker-votes">
      {#each view.votes ?? [] as v (v.voter.id)}
        <li class="flex items-center gap-2 text-sm" data-testid="ticket-poker-vote" data-mine={v.mine ? '' : undefined}>
          <span class="min-w-0 flex-1 truncate text-fg">{v.voter.name || 'Unknown user'}{v.mine ? ' (you)' : ''}</span>
          <span class="shrink-0 tabular-nums {typeof v.value === 'number' ? 'text-fg' : 'text-fg-muted'}">{typeof v.value === 'number' ? v.value : 'voted'}</span>
        </li>
      {:else}
        <li class="text-sm text-fg-muted">No votes yet.</li>
      {/each}
    </ul>
    <p class="text-xs text-fg-muted" data-testid="ticket-poker-state">
      {view.revealed ? 'Revealed.' : 'Not revealed.'}
      {#if readAt}Read {relativeTime(readAt, 'locale', ctx.ref.backend)}.{/if}
    </p>
  {/if}
  <div bind:this={deck} class="flex flex-wrap items-center gap-1" role="group" aria-label="Your vote">
    {#each DECK as card (card)}
      <button
        type="button"
        class="min-w-8 rounded-[var(--radius-control)] border border-border-subtle px-2 py-0.5 text-sm tabular-nums hover:bg-surface-1 disabled:opacity-60 compact:min-h-9 {mine?.value === card ? 'border-accent bg-accent/10 text-accent' : 'text-fg'}"
        aria-pressed={mine?.value === card}
        disabled={pending}
        data-testid="ticket-poker-card"
        onclick={() => vote(card)}
      >{card}</button>
    {/each}
    <form class="flex items-center gap-1" onsubmit={voteTyped}>
      <input
        class="w-16 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-1.5 py-0.5 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        inputmode="decimal"
        placeholder="Other"
        aria-label="Another vote, from 0 to {MAX_VOTE}"
        aria-invalid={typed !== '' && !typedValid}
        disabled={pending}
        data-testid="ticket-poker-typed"
        bind:value={typed}
      />
      <button type="submit" class="text-xs text-accent hover:underline disabled:opacity-60" disabled={pending || !typedValid} data-testid="ticket-poker-typed-vote">Vote</button>
    </form>
  </div>
  {#if typed !== '' && !typedValid}
    <p class="text-xs text-error" data-testid="ticket-poker-typed-invalid">A vote is a number from 0 to {MAX_VOTE}.</p>
  {/if}
  <div class="flex flex-wrap items-center gap-2">
    <button type="button" class="text-xs text-accent hover:underline disabled:opacity-60" disabled={pending} data-testid="ticket-poker-reveal" onclick={() => { clearing = false; void revealTicketPoker(ctx); }}>Reveal</button>
    <button bind:this={clearButton} type="button" class="text-xs hover:underline disabled:opacity-60 {clearing ? 'font-medium text-error' : 'text-accent'}" disabled={pending} data-testid="ticket-poker-clear" onclick={clearVotes}>{clearing ? 'Clear everyone\'s votes' : 'Clear votes'}</button>
    {#if clearing}
      <button type="button" class="text-xs text-fg-muted hover:underline" data-testid="ticket-poker-clear-cancel" onclick={() => { clearing = false; }}>Keep them</button>
    {/if}
  </div>
  <TicketWriteNote ticketKey={ctx.key} target="poker" fieldName={(id) => id} />
  {#if result.length > 0}
    <div class="flex flex-wrap items-center gap-1.5" data-testid="ticket-poker-result">
      {#if onSetEstimate}
        {@const set = onSetEstimate}
        {#each result as r (r.value)}
          <button type="button" class="rounded-[var(--radius-control)] border border-border-subtle px-2 py-0.5 text-xs text-fg hover:bg-surface-1 disabled:opacity-60 compact:min-h-9" disabled={settingEstimate} data-testid="ticket-poker-set-estimate" onclick={() => set(r.value)}>
            Set estimate to {r.value}{hours ? (r.value === 1 ? ' hour' : ' hours') : ''} <span class="text-fg-muted">({r.count} {r.count === 1 ? 'vote' : 'votes'})</span>
          </button>
        {/each}
      {:else}
        <p class="text-xs text-fg-muted" data-testid="ticket-poker-no-row">Open the ticket's board in Issues to set its estimate.</p>
      {/if}
    </div>
  {/if}
</div>
