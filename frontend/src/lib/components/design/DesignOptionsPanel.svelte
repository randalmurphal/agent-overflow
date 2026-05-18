<script lang="ts">
  // DesignOptionsPanel — when `ctx.activeOptionSet` is non-null, render
  // the small N-up grid of option iframes side-by-side. Each iframe
  // loads `/design/{threadId}/options/{setId}/{optionId}/` from the Go
  // file server. Clicking an option resolves the pick by sending a
  // structured user message (the agent's prompt teaches it to apply
  // the picked direction to main/) and then clears the active set so
  // the panel collapses and the main preview takes over.
  //
  // `ctx.activeOptionSet` is populated when the file watcher fires
  // `design:options-update` — the events handler refreshes pane state,
  // and this panel can call `ctx.refreshDesignOptions(threadId)` to
  // ask the backend for the latest unresolved option set.

  import type { PanelContext } from '../../stores/rhsPanelSlot.svelte';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import { DismissDesignOptionSet, SendMessage } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';

  let { ctx }: { ctx: PanelContext } = $props();

  let busyOptionId = $state<string | null>(null);
  let iframeReloadKey = $state(0);

  function refresh(): void {
    // Re-derive picker state from disk (in case the watcher missed
    // an event under burst conditions) AND bump the iframe reload
    // key so any options whose contents changed in-place but kept
    // the same path get a fresh fetch. Without the reload key,
    // Svelte's `(path)` keying on the each-block reuses the iframe
    // element for an unchanged path — the user's "refresh this
    // panel" intent wouldn't survive into the children.
    const threadId = ctx.threadId;
    if (threadId) void ctx.refreshDesignOptions(threadId);
    iframeReloadKey += 1;
  }

  function optionIdFromPath(path: string): string {
    // optionPaths take the shape `options/{setId}/{optionId}` (or with a
    // trailing slash). Strip the trailing slash, then split on `/` and
    // take the last segment so we render a stable label even when the
    // backend hands us a normalised path.
    const trimmed = path.replace(/\/+$/, '');
    const idx = trimmed.lastIndexOf('/');
    return idx >= 0 ? trimmed.slice(idx + 1) : trimmed;
  }

  function iframeSrcFor(
    threadId: string,
    setId: string,
    optionId: string,
    cb: number,
  ): string {
    // ?cb=N is the refresh-button's lever. Without it the URL is
    // identical between renders and Svelte's keyed each reuses the
    // existing iframe element with no network re-fetch.
    return `/design/${encodeURIComponent(threadId)}/options/${encodeURIComponent(
      setId,
    )}/${encodeURIComponent(optionId)}/?cb=${cb}`;
  }

  async function pick(optionId: string): Promise<void> {
    const set = ctx.activeOptionSet;
    const threadId = ctx.threadId;
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
      // Persist the dismissal to disk (`.picked` marker) so a
      // refresh / app restart doesn't re-hydrate the same picker the
      // user already resolved. Best-effort: a marker-write failure
      // is logged but doesn't block the UX — the pane still clears
      // locally; the worst case is a one-time re-hydration on next
      // mount which the user can dismiss manually.
      void DismissDesignOptionSet(threadId, set.setId).catch((err) => {
        // eslint-disable-next-line no-console
        console.warn('design: DismissDesignOptionSet failed:', err);
      });
      ctx.setActiveOptionSet(null);
    } catch (err) {
      const m = err instanceof Error ? err.message : String(err);
      addToast('error', `Failed to send option pick: ${m}`);
    } finally {
      busyOptionId = null;
    }
  }
</script>

{#if ctx.activeOptionSet && ctx.threadId}
  {@const set = ctx.activeOptionSet}
  {@const threadId = ctx.threadId}
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
      <div class="flex items-center gap-1">
        <button
          type="button"
          onclick={refresh}
          title="Refresh options"
          aria-label="Refresh options"
          class={[
            'inline-flex items-center justify-center rounded-[var(--radius-field)]',
            'border border-border-subtle bg-surface-0 p-1',
            'text-fg-subtle cursor-pointer transition-colors',
            'hover:border-border hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
          ].join(' ')}
        >
          <Icon icon={RefreshCw} size={14} />
        </button>
        <button
          type="button"
          onclick={() => ctx.setActiveOptionSet(null)}
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
                src={iframeSrcFor(threadId, set.setId, optionId, iframeReloadKey)}
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
