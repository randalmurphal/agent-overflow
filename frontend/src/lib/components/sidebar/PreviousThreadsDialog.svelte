<script lang="ts">
  // An assistant's Previous Threads (docs/specs/standing-agents.md,
  // "Sidebar"), from its row's menu: the threads earlier handoffs
  // superseded, newest first, each with when it was superseded. A row opens
  // that thread where the menu was opened and closes the dialog. The store
  // (assistantPastThreads.svelte.ts) owns the reads and drops a page that
  // answers for a closed or reopened dialog.
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import {
    closePreviousThreadsDialog,
    getPreviousThreads,
    openPreviousThread,
    showMorePreviousThreads,
  } from '../../stores/assistantPastThreads.svelte';
  import { assistantCurrentThread } from '../../stores/threads.svelte';
  import { assistantDisplayName, assistantsShown } from '../../stores/assistants.svelte';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { relativeTime } from '../../utils/format';

  const revealButtonClass = 'mt-0.5 inline-flex self-start rounded px-1 py-0.5 text-[0.6875rem] text-fg-hint/65 transition-colors hover:bg-surface-2/40 hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35';

  let view = $derived(getPreviousThreads());
  let agent = $derived(view ? assistantCurrentThread(view.opening.agentId) : undefined);

  // Archived, deleted or detached with its computer: nothing left to list.
  // Assistants turned off: nothing of them shows.
  $effect(() => {
    if (view !== null && (agent === undefined || !assistantsShown())) closePreviousThreadsDialog(view.opening);
  });

  function superseded(at: number | null | undefined): string {
    getMinuteNow();
    if (!at || !view) return '';
    return `Superseded ${relativeTime(at, getSettings().timestampFormat, view.opening.backend)}`;
  }
</script>

<Modal
  open={view !== null && agent !== undefined}
  title={agent ? `${assistantDisplayName(agent)}'s previous threads` : 'Previous threads'}
  onClose={() => closePreviousThreadsDialog()}
  width="md"
>
  {#snippet children()}
    <div class="flex flex-col gap-2 text-xs text-fg-muted" data-testid="previous-threads-dialog">
      {#if view}
        {#if view.threads.length > 0}
          <ul
            class="flex max-h-80 flex-col overflow-y-auto rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 py-1"
            aria-label="Previous threads"
            data-testid="previous-threads-list"
          >
            {#each view.threads as thread (thread.id)}
              <li>
                <button
                  type="button"
                  class="flex w-full min-w-0 items-center gap-2 px-2 py-1 compact:py-2 text-left text-xs text-fg hover:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40"
                  onclick={() => void openPreviousThread(view.opening, thread)}
                  data-testid="previous-threads-item"
                  data-thread-id={thread.id}
                >
                  <span class="min-w-0 flex-1 truncate" title={thread.title}>{thread.title || 'Untitled'}</span>
                  <span class="shrink-0 text-[0.625rem] tabular-nums text-fg-hint" data-testid="previous-threads-superseded">
                    {superseded(thread.agentSupersededAt)}
                  </span>
                </button>
              </li>
            {/each}
          </ul>
        {:else if view.loaded && view.error === null}
          <p class="text-fg-hint" data-testid="previous-threads-empty">No previous threads.</p>
        {/if}
        {#if view.loading}
          <p class="text-fg-hint" data-testid="previous-threads-loading">Loading…</p>
        {/if}
        {#if view.error}
          <p role="alert" data-testid="previous-threads-error" class="rounded-md border border-error/40 bg-error/10 px-3 py-1.5 text-xs text-error">
            {view.error}
          </p>
        {/if}
        {#if view.next !== null && !view.loading && (view.threads.length > 0 || view.error !== null)}
          <button
            type="button"
            class={revealButtonClass}
            onclick={showMorePreviousThreads}
            data-testid="previous-threads-show-more"
          >
            {view.error !== null ? 'Try again' : 'Show more…'}
          </button>
        {/if}
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={() => closePreviousThreadsDialog()} testId="previous-threads-close">
      {#snippet children()}Close{/snippet}
    </Button>
  {/snippet}
</Modal>
