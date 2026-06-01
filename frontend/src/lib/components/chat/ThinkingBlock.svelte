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
  import { revealedSuffix } from '../../utils/textOverlap';

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

  // Body text source. For the collapsed view we prefer the per-pane
  // live smoother tail — it grows monotonically with the smoother's
  // revealed text, so the CSS clip (`max-h-[3lh] overflow-hidden`)
  // can scroll older lines off the top without `whitespace-pre-wrap`
  // re-wrapping a sliding-window string and shifting visible content
  // wholesale. `item.summary` is trimmed to THINKING_TAIL_RUNES for
  // memory/persistence; reading it directly produces the user-reported
  // "5 words appear at once past 400 runes" symptom because every
  // reveal recomputes wrap for the full bounded string. The live-tail
  // map is non-null only while the smoother is active; fall back to
  // `item.summary` once the stream settles (the smoother disposes and
  // the trimmed tail is the persisted final value).
  // After expansion, the full payload comes from SQLite and future
  // deltas append into the expansion handle.
  const bodyText = $derived.by<string>(() => {
    const summary = item.summary ?? '';
    const live = pane?.liveThinkingTailForItem(item.id) ?? summary;
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

  // Merge the loaded snapshot with the live reveal into the longer view of the
  // same canonical stream. revealedSuffix (textOverlap.ts) is containment-aware:
  // when the flushed snapshot leads the reveal (liveTail is a prefix of
  // persisted), it appends nothing instead of re-appending the whole prefix.
  function mergeStreamingExpandedText(persisted: string, liveTail: string): string {
    return persisted + revealedSuffix(persisted, liveTail);
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
          'flex-1 min-w-0 block text-[0.75rem] text-fg-muted/70 italic whitespace-pre-wrap leading-relaxed',
          !expanded ? 'max-h-[3lh] overflow-hidden' : null,
        ]}
      >{bodyText}</span>
    {/snippet}
    {#snippet actions()}
      <div class="shrink-0 flex items-center gap-1.5 text-[0.625rem] text-fg-hint pt-[2px]">
        <span data-testid="thinking-copy-slot" class="flex h-7 w-7 shrink-0 items-center justify-center">
          {#if canCopy}
            <span class="opacity-0 transition-opacity duration-150 group-hover/thinking:opacity-100 focus-within:opacity-100">
              <CopyButton
                text={getCopyText}
                label="Copy thinking"
                onError={() => addToast('error', 'Failed to copy')}
              />
            </span>
          {/if}
        </span>
        <time class="tabular-nums" datetime={isoTime}>{timestamp}</time>
      </div>
    {/snippet}
  </TranscriptDisclosureHeader>
</div>
