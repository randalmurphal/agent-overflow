<script lang="ts">
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import {
    THINKING_PAYLOAD_EXPANSION_STATE_KEY,
    thinkingPayloadVersionForItem,
  } from '../../utils/payloadVersion';
  import { nonOverlappingSuffix } from '../../utils/textOverlap';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => item.payloadId,
          () => item.threadId,
          {
            payloadVersion: () => thinkingPayloadVersionForItem(item),
            loadMode: 'full',
            cacheEnabled: () => item.status !== 'streaming',
          },
        ),
  );
  const expansion = $derived(
    pane
      ? pane.expansionStateFor(item, {
          loadMode: 'full',
          stateKey: THINKING_PAYLOAD_EXPANSION_STATE_KEY,
          payloadVersion: thinkingPayloadVersionForItem,
          cacheEnabled: (currentItem) => currentItem?.status !== 'streaming',
        })
      : localFallback!,
  );
  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(item.payloadId),
  );

  const isStreaming = $derived(item.status === 'streaming');

  // Body text source. `item.summary` is the bounded live tail: it is the
  // only source used while collapsed. After expansion, the full payload
  // comes from SQLite and future deltas append into the expansion handle.
  // While the row is still streaming, merge the live tail over the full
  // payload so a stale read cannot hide text that was already visible in
  // the collapsed row.
  const bodyText = $derived.by<string>(() => {
    const live = item.summary ?? '';
    const persisted = expansion.displayData ?? '';
    if (!expanded) return live;
    if (isStreaming) return mergeStreamingExpandedText(persisted, live);
    return persisted.length > live.length ? persisted : live;
  });

  // Single source of truth: the expansion handle. No more "default
  // expanded while streaming" — the row sits in the tail-clamped state
  // through streaming, settle, and reload until the user opts in.
  const expanded = $derived(expansion.expanded);

  let bodyEl: HTMLSpanElement | undefined = $state();

  // Tail-pin. While collapsed, the body has `max-height` capped to
  // 3 lines and `overflow: hidden`; writing `scrollTop = scrollHeight`
  // keeps the visible window aligned with the END of the text so
  // streaming deltas appear at the bottom and older lines scroll off
  // the top. When expanded the max-height is removed and the
  // assignment is harmless (scrollHeight === clientHeight, scrollTop
  // can't go past zero).
  $effect(() => {
    void bodyText;
    void expanded;
    if (!bodyEl) return;
    bodyEl.scrollTop = bodyEl.scrollHeight;
  });

  async function handleToggle() {
    if (expanded) {
      expansion.collapse();
    } else {
      await expansion.expand();
    }
  }

  const timestamp = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  // CopyButton getter — eagerly fetch the full payload before copying
  // so the hover-only affordance always yields complete content,
  // regardless of whether the row was previously expanded.
  async function getCopyText(): Promise<string> {
    if (!expansion.expanded) await expansion.expand();
    await expansion.ensureLoaded();
    return expansion.displayData ?? item.summary ?? '';
  }

  const canCopy = $derived(!isStreaming && /\S/.test(item.summary ?? ''));

  function mergeStreamingExpandedText(persisted: string, liveTail: string): string {
    if (!persisted) return liveTail;
    if (!liveTail) return persisted;
    return persisted + nonOverlappingSuffix(persisted, liveTail);
  }
</script>

<div class="group/thinking">
  <TranscriptDisclosureHeader
    {expanded}
    controls={`thinking-${item.id}`}
    ariaLabel="Toggle Thinking Block"
    testId="thinking-toggle"
    class="!items-start rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    buttonClass="!items-start"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, handleToggle)}
  >
    {#snippet icon()}<ToolKindIcon kind="brain" ariaLabel="think" />{/snippet}
    {#snippet label()}<span data-testid="thinking-label">think</span>{/snippet}
    {#snippet body()}
      <span
        bind:this={bodyEl}
        id={`thinking-${item.id}`}
        data-testid="thinking-body"
        class={[
          'flex-1 min-w-0 block text-[12px] text-fg-muted/70 italic whitespace-pre-wrap leading-relaxed',
          !expanded ? 'max-h-[3lh] overflow-hidden' : null,
        ]}
      >{bodyText}</span>
    {/snippet}
    {#snippet actions()}
      <div class="shrink-0 flex items-center gap-1.5 text-[10px] text-fg-hint pt-[2px]">
        {#if canCopy}
          <span class="opacity-0 transition-opacity duration-150 group-hover/thinking:opacity-100 focus-within:opacity-100">
            <CopyButton
              text={getCopyText}
              label="Copy thinking"
              onError={() => addToast('error', 'Failed to copy')}
            />
          </span>
        {/if}
        <time class="tabular-nums" datetime={isoTime}>{timestamp}</time>
      </div>
    {/snippet}
  </TranscriptDisclosureHeader>
</div>
