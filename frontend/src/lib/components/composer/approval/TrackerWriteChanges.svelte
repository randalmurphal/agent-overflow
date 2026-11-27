<script lang="ts" module>
  import type { ApprovalRequest } from '../../../types/events';

  /** Names the one prompt whose changes the list drew (`drawn`). */
  export function drawnKey(approval: ApprovalRequest): string {
    return `${approval.requestId}\u0000${approval.occurrence ?? ''}`;
  }
</script>

<script lang="ts">
  // What a tracker write will send, for the person to approve
  // (internal/trackertools Approval): each ticket with its title, the
  // connection it goes through when two of one site are in it, and its
  // change lines (a time post's are its entries: day, time and whole
  // description), the comment each gets, and the notes, lines that name
  // no ticket (a time post's open questions). A line that
  // shows a value in part (a new text, a value cut) names the value, which
  // the approval carries whole; Show all opens it there, with nothing
  // fetched. The actions are the tool approval panel's.
  //
  // `drawn` is drawnKey of the prompt once the list holds every ticket,
  // change and note its input names and is in the page, and '' while it
  // is not: the app approves a tracker write only from a screen that drew
  // it (ApprovalResponse.shown).
  import Button from '../../primitives/Button.svelte';

  let { approval, drawn = $bindable('') }: { approval: ApprovalRequest; drawn?: string } = $props();

  type Change = { line: string; value: string };
  type Ticket = { key: string; account: string; title: string; changes: Change[] };

  const record = (v: unknown): Record<string, unknown> | null =>
    v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
  const text = (r: Record<string, unknown>, k: string): string => (typeof r[k] === 'string' ? (r[k] as string) : '');

  const input = $derived(record(approval.input) ?? {});
  // The tickets, or null when the input holds one this list cannot draw.
  const parsed: Ticket[] | null = $derived.by(() => {
    const raw = input.tickets;
    if (!Array.isArray(raw)) return null;
    const out: Ticket[] = [];
    for (const entry of raw) {
      const r = record(entry);
      if (!r || !text(r, 'key') || (r.changes !== undefined && !Array.isArray(r.changes))) return null;
      const changes: Change[] = [];
      for (const c of (r.changes as unknown[] | undefined) ?? []) {
        const cr = record(c);
        if (!cr || !text(cr, 'line')) return null;
        changes.push({ line: text(cr, 'line'), value: text(cr, 'value') });
      }
      out.push({ key: text(r, 'key'), account: text(r, 'account'), title: text(r, 'title'), changes });
    }
    return out;
  });
  const tickets: Ticket[] = $derived(parsed ?? []);
  // Each value of the call's set, whole, by its name.
  const values: Map<string, string> = $derived.by(() => {
    const out = new Map<string, string>();
    if (!Array.isArray(input.values)) return out;
    for (const v of input.values) {
      const r = record(v);
      if (r && text(r, 'name')) out.set(text(r, 'name'), text(r, 'value'));
    }
    return out;
  });
  const comment = $derived(text(input, 'comment'));
  const notes: string[] = $derived(Array.isArray(input.notes) ? input.notes.filter((n): n is string => typeof n === 'string' && n !== '') : []);
  const complete = $derived(
    parsed !== null &&
      (input.notes === undefined || (Array.isArray(input.notes) && notes.length === input.notes.length)) &&
      (tickets.length > 0 || notes.length > 0 || comment !== ''),
  );

  let root: HTMLDivElement | undefined = $state();
  // An effect runs once the list above is in the page.
  $effect(() => {
    drawn = complete && root ? drawnKey(approval) : '';
  });

  // The lines whose whole value is open, by ticket and line.
  let open = $state<Set<string>>(new Set());
  function toggle(id: string): void {
    const next = new Set(open);
    if (!next.delete(id)) next.add(id);
    open = next;
  }
</script>

<div bind:this={root} class="mt-2 max-h-60 overflow-y-auto rounded border border-border bg-surface-1 px-2.5 py-1.5" data-testid="tracker-write-approval">
  <ul class="space-y-1">
    {#each tickets as ticket (`${ticket.key}\n${ticket.account}`)}
      <li class="min-w-0 text-xs" data-testid="tracker-write-approval-ticket">
        <div class="flex min-w-0 items-baseline gap-1.5">
          <span class="shrink-0 font-mono font-semibold text-fg">{ticket.key}</span>
          {#if ticket.account}<span class="shrink-0 text-fg-subtle" data-testid="tracker-write-approval-account">{ticket.account}</span>{/if}
          {#if ticket.title}<span class="min-w-0 truncate text-fg-muted">{ticket.title}</span>{/if}
        </div>
        {#each ticket.changes as change, i (i)}
          {@const id = `${ticket.key}\n${ticket.account}\n${i}`}
          {@const whole = change.value ? values.get(change.value) : undefined}
          <div class="ml-3 flex min-w-0 items-baseline gap-1.5 text-fg-muted">
            <span class="min-w-0 break-words" data-testid="tracker-write-approval-change">{change.line}</span>
            {#if whole !== undefined}
              <Button variant="ghost" size="xs" testId="tracker-write-approval-show-all" onclick={() => toggle(id)}>
                {open.has(id) ? 'Show less' : 'Show all'}
              </Button>
            {/if}
          </div>
          {#if whole !== undefined && open.has(id)}
            <p class="ml-3 mt-0.5 max-h-40 overflow-y-auto whitespace-pre-wrap break-words rounded border border-border-subtle px-1.5 py-1 text-fg" data-testid="tracker-write-approval-value">{whole}</p>
          {/if}
        {/each}
      </li>
    {/each}
  </ul>
  {#if notes.length > 0}
    <ul class="mt-1.5 space-y-0.5 border-t border-border pt-1.5 text-xs text-fg-muted">
      {#each notes as note, i (i)}
        <li class="break-words" data-testid="tracker-write-approval-note">{note}</li>
      {/each}
    </ul>
  {/if}
  {#if comment}
    <div class="mt-1.5 border-t border-border pt-1.5" data-testid="tracker-write-approval-comment">
      <span class="mb-0.5 block text-[0.625rem] text-fg-subtle">Comment on each ticket</span>
      <p class="whitespace-pre-wrap break-words text-xs text-fg">{comment}</p>
    </div>
  {/if}
</div>
