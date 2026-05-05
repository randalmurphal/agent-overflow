<script lang="ts">
  // DesignOptionsPanel — when `pane.activeOptionSet` is non-null, render
  // the small N-up grid of option iframes side-by-side. Each iframe
  // loads `/design/{threadId}/options/{setId}/{optionId}/` from the Go
  // file server. Clicking an option resolves the pick by sending a
  // structured user message (the agent's prompt teaches it to apply
  // the picked direction to main/) and then clears the active set so
  // the panel collapses and the main preview takes over.
  //
  // `pane.activeOptionSet` is populated when the file watcher fires
  // `design:options-update` — the events handler calls
  // `pane.applyDesignOptionsUpdate(threadId, setId)` which lists the
  // option directories under `options/{setId}/` and writes them onto
  // the pane.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import { SendMessage } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let busyOptionId = $state<string | null>(null);

  function optionIdFromPath(path: string): string {
    // optionPaths take the shape `options/{setId}/{optionId}` (or with a
    // trailing slash). Strip the trailing slash, then split on `/` and
    // take the last segment so we render a stable label even when the
    // backend hands us a normalised path.
    const trimmed = path.replace(/\/+$/, '');
    const idx = trimmed.lastIndexOf('/');
    return idx >= 0 ? trimmed.slice(idx + 1) : trimmed;
  }

  function iframeSrcFor(threadId: string, setId: string, optionId: string): string {
    return `/design/${encodeURIComponent(threadId)}/options/${encodeURIComponent(
      setId,
    )}/${encodeURIComponent(optionId)}/`;
  }

  async function pick(optionId: string): Promise<void> {
    const set = pane.activeOptionSet;
    const threadId = pane.threadId;
    if (!set || !threadId || busyOptionId) return;
    busyOptionId = optionId;
    try {
      const json = JSON.stringify(
        {
          kind: 'option_chosen',
          setId: set.setId,
          optionId,
          path: `options/${set.setId}/${optionId}`,
        },
        null,
        2,
      );
      const message = `Picked option ${optionId} from set ${set.setId}.\n\n\`\`\`aoflow-design\n${json}\n\`\`\``;
      await SendMessage(threadId, message, []);
      pane.setActiveOptionSet(null);
    } catch (err) {
      const m = err instanceof Error ? err.message : String(err);
      addToast('error', `Failed to send option pick: ${m}`);
    } finally {
      busyOptionId = null;
    }
  }
</script>

{#if pane.activeOptionSet && pane.threadId}
  {@const set = pane.activeOptionSet}
  {@const threadId = pane.threadId}
  <section
    class="flex flex-col h-full min-h-0 bg-transparent"
    data-testid="design-options-panel"
  >
    <div class="flex items-center justify-between px-3 pt-3 pb-2 border-b border-border-subtle shrink-0">
      <div>
        <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-fg-subtle">
          Pick an option
        </p>
        <p class="text-[10px] text-fg-hint mt-0.5">
          Set <span class="font-mono">{set.setId}</span>
          · {set.optionPaths.length} options
        </p>
      </div>
      <button
        type="button"
        onclick={() => pane.setActiveOptionSet(null)}
        title="Dismiss option set"
        class={[
          'inline-flex items-center gap-1 rounded-[var(--radius-field)]',
          'border border-border-subtle bg-surface-0 px-2 py-1',
          'text-[12px] text-fg cursor-pointer transition-colors',
          'hover:border-border focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        ].join(' ')}
      >
        Dismiss
      </button>
    </div>
    <div class="flex-1 min-h-0 overflow-y-auto p-2">
      {#if set.optionPaths.length === 0}
        <p class="px-2 py-4 text-[12px] text-fg-subtle">
          The agent declared an option set with no options yet — wait a moment.
        </p>
      {:else}
        <div
          class="grid gap-2"
          style="grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));"
        >
          {#each set.optionPaths as path (path)}
            {@const optionId = optionIdFromPath(path)}
            <article
              class="flex flex-col rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 overflow-hidden"
              data-testid="design-option-card"
            >
              <iframe
                title={`Option ${optionId}`}
                src={iframeSrcFor(threadId, set.setId, optionId)}
                sandbox="allow-scripts"
                referrerpolicy="no-referrer"
                class="w-full aspect-[4/3] bg-white border-b border-border-subtle"
              ></iframe>
              <div class="flex items-center justify-between px-2 py-1.5">
                <span class="text-[12px] text-fg truncate font-medium">{optionId}</span>
                <button
                  type="button"
                  onclick={() => void pick(optionId)}
                  disabled={busyOptionId !== null}
                  class={[
                    'inline-flex items-center rounded-[var(--radius-field)]',
                    'border border-accent/60 bg-accent/15 px-2 py-0.5',
                    'text-[11px] text-fg cursor-pointer transition-colors',
                    'hover:bg-accent/25',
                    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
                    'disabled:opacity-50 disabled:cursor-not-allowed',
                  ].join(' ')}
                  data-testid="design-option-pick"
                >
                  {busyOptionId === optionId ? 'Picking…' : 'Pick'}
                </button>
              </div>
            </article>
          {/each}
        </div>
      {/if}
    </div>
  </section>
{/if}
