<script lang="ts">
  // What a tracker write that names several tickets will send, for the
  // person to approve (internal/trackertools Approval): each ticket with
  // its title and changes, and the comment each gets. The actions are the
  // tool approval panel's.
  import type { ApprovalRequest } from '../../../types/events';

  let { approval }: { approval: ApprovalRequest } = $props();

  type Ticket = { key: string; title: string; changes: string[] };

  const input = $derived(
    approval.input && typeof approval.input === 'object' && !Array.isArray(approval.input)
      ? (approval.input as Record<string, unknown>)
      : {},
  );
  const tickets: Ticket[] = $derived.by(() => {
    const raw = input.tickets;
    if (!Array.isArray(raw)) return [];
    return raw.flatMap((entry) => {
      if (!entry || typeof entry !== 'object' || Array.isArray(entry)) return [];
      const record = entry as Record<string, unknown>;
      const key = typeof record.key === 'string' ? record.key : '';
      if (!key) return [];
      const changes = Array.isArray(record.changes) ? record.changes.filter((c): c is string => typeof c === 'string') : [];
      return [{ key, title: typeof record.title === 'string' ? record.title : '', changes }];
    });
  });
  const comment = $derived(typeof input.comment === 'string' ? input.comment : '');
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
          <div class="ml-3 text-fg-muted" data-testid="tracker-write-approval-change">{change}</div>
        {/each}
      </li>
    {/each}
  </ul>
  {#if comment}
    <div class="mt-1.5 border-t border-border pt-1.5" data-testid="tracker-write-approval-comment">
      <span class="mb-0.5 block text-[0.625rem] text-fg-subtle">Comment on each ticket</span>
      <p class="whitespace-pre-wrap break-words text-xs text-fg">{comment}</p>
    </div>
  {/if}
</div>
