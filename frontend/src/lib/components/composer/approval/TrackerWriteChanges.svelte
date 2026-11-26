<script lang="ts">
  // What a tracker write will send, for the person to approve
  // (internal/trackertools Approval): each ticket with its title and
  // change lines (a time post's are its entries: day, time and whole
  // description), the comment each gets, and the notes, lines that name
  // no ticket (a time post's open questions). A line that
  // shows a value in part (a new text, a value cut) names the value, which
  // the approval carries whole; Show all opens it there, with nothing
  // fetched. The actions are the tool approval panel's.
  import type { ApprovalRequest } from '../../../types/events';
  import Button from '../../primitives/Button.svelte';

  let { approval }: { approval: ApprovalRequest } = $props();

  type Change = { line: string; value: string };
  type Ticket = { key: string; title: string; changes: Change[] };

  const record = (v: unknown): Record<string, unknown> | null =>
    v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
  const text = (r: Record<string, unknown>, k: string): string => (typeof r[k] === 'string' ? (r[k] as string) : '');

  const input = $derived(record(approval.input) ?? {});
  const tickets: Ticket[] = $derived.by(() => {
    const raw = input.tickets;
    if (!Array.isArray(raw)) return [];
    return raw.flatMap((entry) => {
      const r = record(entry);
      const key = r ? text(r, 'key') : '';
      if (!r || !key) return [];
      const changes = Array.isArray(r.changes)
        ? r.changes.flatMap((c) => {
            const cr = record(c);
            return cr && text(cr, 'line') ? [{ line: text(cr, 'line'), value: text(cr, 'value') }] : [];
          })
        : [];
      return [{ key, title: text(r, 'title'), changes }];
    });
  });
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

  // The lines whose whole value is open, by ticket and line.
  let open = $state<Set<string>>(new Set());
  function toggle(id: string): void {
    const next = new Set(open);
    if (!next.delete(id)) next.add(id);
    open = next;
  }
</script>

<div class="mt-2 max-h-60 overflow-y-auto rounded border border-border bg-surface-1 px-2.5 py-1.5" data-testid="tracker-write-approval">
  <ul class="space-y-1">
    {#each tickets as ticket (ticket.key)}
      <li class="min-w-0 text-xs" data-testid="tracker-write-approval-ticket">
        <div class="flex min-w-0 items-baseline gap-1.5">
          <span class="shrink-0 font-mono font-semibold text-fg">{ticket.key}</span>
          {#if ticket.title}<span class="min-w-0 truncate text-fg-muted">{ticket.title}</span>{/if}
        </div>
        {#each ticket.changes as change, i (i)}
          {@const id = `${ticket.key}\n${i}`}
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
