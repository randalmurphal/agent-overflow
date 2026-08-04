<script lang="ts">
  // Output of a slash command the provider CLI executed itself — `/usage`,
  // `/context`, a skill, a plugin command. The model never saw it, so this row
  // deliberately does NOT look like an assistant bubble and is never routed
  // through ChatMarkdown: the CLI hands us terminal text with ANSI already
  // stripped (docs/references/claude-wire.md §"Slash commands"), and markdown
  // rendering would re-flow aligned columns and eat `#`/`*` glyphs the command
  // meant literally. It is a system row — muted label line over a monospaced
  // block — matching NotificationRow's register rather than a message.
  //
  // Shape is fixed at first render: triage writes the row completed in one
  // shot (internal/triage/command_result.go), so `truncated` cannot flip under
  // the reader and no affordance appears late.
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import {
    createPayloadExpansion,
    formatPayloadSize,
  } from '../../utils/payloadExpansion.svelte';
  import {
    COMMAND_RESULT_PAYLOAD_EXPANSION_STATE_KEY,
    payloadVersionForItem,
  } from '../../utils/payloadVersion';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';
  import { preservePaneScrollAnchorAt } from './preserveScrollAnchor';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import { formatTimeOfDay } from '../../utils/format';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { readCommandResultView } from './commandResultMeta';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  const LOAD_BUTTON_CLASS =
    'cursor-pointer rounded text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50';

  const view = $derived(readCommandResultView(item));

  // No-pane fallback (standalone renders, tests). The payload is written with
  // the row and never grows, so the default module cache and the settled
  // payloadVersion are correct — no streaming-freshness handling.
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => item.payloadId,
          () => item.threadId,
          { payloadVersion: () => payloadVersionForItem(item) },
        ),
  );
  const expansionRef = useLeasedItemExpansion({
    // An untruncated row's whole output is already in meta; minting a
    // registry entry for it would put a payload-less handle in the pane's
    // expansion map and count toward `hasUserExpansionWithin`.
    enabled: () => view.truncated,
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
    getOptions: () => ({
      stateKey: COMMAND_RESULT_PAYLOAD_EXPANSION_STATE_KEY,
      // Module-scope helper only: the pane registry retains this callback for
      // the entry's lifetime (see RowExpansionStateOptions).
      payloadVersion: payloadVersionForItem,
    }),
  });
  const expansion = $derived(expansionRef.current);

  // The reader's intent lives in the pane registry, so a windowing remount
  // finds the row already open with its bytes still in the payload cache.
  const expanded = $derived(expansion?.expanded ?? false);
  // Preview first, loaded bytes once they land: the text never blanks to a
  // spinner mid-expand, it grows in place.
  const outputText = $derived(
    (expanded ? expansion?.displayData : null) ?? view.preview,
  );
  const sizeLabel = $derived(view.totalBytes > 0 ? formatPayloadSize(view.totalBytes) : '');

  const bodyDomId = $derived(chatRowDomId(pane, 'command-result-output', item.id));
  const time = $derived(formatTimeOfDay(item.createdAt));
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  // The block grows DOWNWARD from the row's top edge, and the control sits
  // below it — anchoring the button (preservePaneScrollAnchor's default) would
  // hold the button still and push the output off the top of the viewport.
  // Anchor the row instead: the reader keeps looking at the same place and the
  // rows underneath absorb the delta. See preserveScrollAnchor.ts.
  let rowEl = $state<HTMLElement | null>(null);
  function withRowAnchored(action: () => void | Promise<void>): void | Promise<void> {
    return preservePaneScrollAnchorAt(pane, rowEl, action);
  }

  function handleToggle(): Promise<void> | void {
    if (!expansion) return;
    if (expanded) {
      // Collapsing releases the loaded bytes back to the payload cache's LRU;
      // the row falls back to the preview it always had.
      expansion.collapse();
      return;
    }
    return expansion.expand();
  }

  // Copy always yields the WHOLE output, expanded or not — a reader copying a
  // collapsed `/context` dump should not silently get the bounded head.
  async function getCopyText(): Promise<string> {
    if (!view.truncated || !expansion) return view.preview;
    await expansion.expand();
    await expansion.showFull();
    return expansion.displayData ?? view.preview;
  }
</script>

<div bind:this={rowEl} class="group/command-result mb-1.5" data-testid="command-result-row">
  <div class="flex items-center gap-2 px-1 py-0.5 text-[0.6875rem] text-fg-hint">
    <ToolKindIcon kind="terminal" ariaLabel="command" />
    <span data-testid="command-result-label">command</span>
    {#if sizeLabel}
      <span class="tabular-nums text-fg-subtle" data-testid="command-result-size">{sizeLabel}</span>
    {/if}
    <span class="flex-1"></span>
    <span
      class="flex h-7 w-7 shrink-0 items-center justify-center"
      data-testid="command-result-copy-slot"
    >
      <span
        class="opacity-0 transition-opacity duration-150 group-hover/command-result:opacity-100 focus-within:opacity-100"
      >
        <CopyButton
          text={getCopyText}
          label="Copy output"
          onError={() => addToast('error', 'Failed to copy')}
        />
      </span>
    </span>
    <time class="tabular-nums" datetime={isoTime}>{time}</time>
  </div>

  <div
    id={bodyDomId}
    class="ml-5 max-h-60 min-w-0 max-w-full overflow-y-auto overflow-x-hidden whitespace-pre-wrap break-words border-l border-border-subtle bg-surface-0/35 px-3 py-2 font-mono text-[0.6875rem] leading-relaxed text-fg-muted"
    use:nestedScroll
    data-testid="command-result-output">{outputText}</div>

  <!-- Present for the whole life of a truncated row, in every load state: the
       control the reader just used must not vanish out from under their focus,
       and `aria-expanded` is only honest on a control that reports both
       states. An untruncated row has nothing to fetch and renders none of it. -->
  {#if view.truncated && expansion}
    <div class="ml-5 border-l border-border-subtle px-3 pb-2 text-[0.6875rem]">
      {#if expansion.loading}
        <p class="animate-pulse text-fg-subtle" role="status" aria-live="polite">Loading…</p>
      {:else if expansion.error}
        <p class="text-error" role="alert">Failed to load: {expansion.error}</p>
        <button
          type="button"
          class={LOAD_BUTTON_CLASS}
          onclick={() => expansion.retry()}
          data-testid="command-result-retry"
        >
          Retry
        </button>
      {:else}
        <button
          type="button"
          class={LOAD_BUTTON_CLASS}
          aria-expanded={expanded}
          aria-controls={bodyDomId}
          onclick={() => withRowAnchored(handleToggle)}
          data-testid="command-result-show-full"
        >
          {#if expanded}Show less ↑{:else}Show full output{sizeLabel ? ` (${sizeLabel})` : ''} ↓{/if}
        </button>
        {#if expanded && expansion.hasMore}
          <button
            type="button"
            class="{LOAD_BUTTON_CLASS} ml-3"
            onclick={() => withRowAnchored(() => expansion.showFull())}
            data-testid="command-result-show-more"
          >
            Load more output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
      {/if}
    </div>
  {/if}
</div>
