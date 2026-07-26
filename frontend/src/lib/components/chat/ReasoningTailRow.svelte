<script lang="ts">
  // Shared disclosure row for the two reasoning-tail kinds — thinking and
  // compaction_reasoning. Both stream model reasoning, clamp it to a sliding
  // 3-line tail while collapsed (via TailClampedText + reasoningBodyText), and
  // reveal the full payload on expand. They differ only in icon/label, their
  // id/testid prefix, and their payload-expansion namespace, all passed in as
  // props. Keeping the expansion wiring + disclosure chrome here means a
  // row-contract change lands once instead of drifting between two near-identical
  // components. ThinkingBlock and CompactionReasoning are the thin wrappers that
  // configure it; TimelineLeaf renders those by kind.
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  // The component above is the SVG dispatcher; this is its `kind` union. Aliased
  // because the value import and the type share the name `ToolKindIcon`.
  import type { ToolKindIcon as ToolKindName } from './toolCardHeader';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import {
    thinkingPayloadCacheEnabled,
    thinkingPayloadVersionForItem,
  } from '../../utils/payloadVersion';
  import { formatTimeOfDay } from '../../utils/format';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';
  import TailClampedText from './TailClampedText.svelte';
  import { reasoningBodyText } from './reasoningTailSource';

  let {
    pane,
    item,
    stateKey,
    iconKind,
    iconAriaLabel,
    labelText,
    idPrefix,
    toggleAriaLabel,
    copyLabel,
  }: {
    pane?: ThreadPane;
    item: Item;
    // Payload-expansion namespace for this row kind — keeps a thinking row, a
    // compaction reasoning row, and a sibling compaction divider from colliding
    // on the same item id.
    stateKey: string;
    iconKind: ToolKindName;
    iconAriaLabel: string;
    labelText: string;
    // Stable prefix for the controls id, the disclosure/body testids, and the
    // TailClampedText body id (e.g. 'thinking' → thinking-toggle / thinking-body).
    idPrefix: string;
    toggleAriaLabel: string;
    copyLabel: string;
  } = $props();

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
  const expansionRef = useLeasedItemExpansion({
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
    getOptions: () => ({
      loadMode: 'full',
      stateKey,
      // Module-scope helpers only: the pane registry retains these callbacks for
      // the entry's lifetime (see RowExpansionStateOptions). They are item-keyed
      // and kind-agnostic, so both reasoning kinds share them on purpose.
      payloadVersion: thinkingPayloadVersionForItem,
      cacheEnabled: thinkingPayloadCacheEnabled,
    }),
  });
  // One derived id for both halves of the disclosure (utils/chatDomIds.ts):
  // the header's `controls` and the body's `id` must be one string.
  let bodyDomId = $derived(chatRowDomId(pane, idPrefix, item.id));
  const expansion = $derived(expansionRef.current!);
  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(item.payloadId),
  );

  const isStreaming = $derived(item.status === 'streaming');

  // Single source of truth: the expansion handle. No "default expanded while
  // streaming" — the row sits in the tail-clamped state through streaming,
  // settle, and reload until the user opts in.
  const expanded = $derived(expansion.expanded);

  // Body text source. The per-pane live smoother tail grows monotonically —
  // TailClampedText requires that: its 3-line clip scrolls older lines off
  // the top, and its wrap-stable layout window assumes append-only growth
  // (`item.summary` is trimmed to THINKING_TAIL_RUNES for memory — reading
  // it directly reintroduces the "5 words appear at once past 400 runes"
  // symptom). The live-tail map is non-null only while the smoother runs;
  // once it settles we fall back to the trimmed summary / loaded payload.
  // See reasoningTailSource for the merge.
  const bodyText = $derived(
    reasoningBodyText({
      summary: item.summary ?? '',
      liveTail: pane?.liveThinkingTailForItem(item.id) ?? null,
      persisted: expansion.displayData ?? '',
      expanded,
      isStreaming,
    }),
  );

  async function handleToggle() {
    if (expanded) {
      expansion.collapse();
    } else {
      await expansion.expand();
    }
  }

  const time = $derived(formatTimeOfDay(item.createdAt));
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  // CopyButton getter — eagerly fetch the full payload before copying so the
  // hover-only affordance always yields complete content, regardless of whether
  // the row was previously expanded.
  async function getCopyText(): Promise<string> {
    if (!expansion.expanded) await expansion.expand();
    await expansion.ensureLoaded();
    return expansion.displayData ?? item.summary ?? '';
  }

  const canCopy = $derived(!isStreaming && /\S/.test(item.summary ?? ''));
</script>

<!--
  Both reasoning kinds share one group name. The copy button's hover reveal is
  scoped to its own row's `group/reasoning-row` ancestor, and these rows are
  always sibling timeline leaves (never nested in one another), so a single
  static name can't cross-talk — and Tailwind only generates the variant from a
  literal it can scan, which is why it lives here, not in a prop.
-->
<div class="group/reasoning-row">
  <TranscriptDisclosureHeader
    {expanded}
    controls={bodyDomId}
    ariaLabel={toggleAriaLabel}
    testId={`${idPrefix}-toggle`}
    class="!items-start rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    buttonClass="!items-start"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, handleToggle)}
  >
    {#snippet icon()}<ToolKindIcon kind={iconKind} ariaLabel={iconAriaLabel} />{/snippet}
    {#snippet label()}<span data-testid={`${idPrefix}-label`}>{labelText}</span>{/snippet}
    {#snippet body()}
      <TailClampedText
        text={bodyText}
        {expanded}
        id={bodyDomId}
        testId={`${idPrefix}-body`}
      />
    {/snippet}
    {#snippet actions()}
      <div class="shrink-0 flex items-center gap-1.5 text-[0.625rem] text-fg-hint pt-[2px]">
        <span
          data-testid={`${idPrefix}-copy-slot`}
          class="flex h-7 w-7 shrink-0 items-center justify-center"
        >
          {#if canCopy}
            <span class="opacity-0 transition-opacity duration-150 group-hover/reasoning-row:opacity-100 focus-within:opacity-100">
              <CopyButton
                text={getCopyText}
                label={copyLabel}
                onError={() => addToast('error', 'Failed to copy')}
              />
            </span>
          {/if}
        </span>
        <time class="tabular-nums" datetime={isoTime}>{time}</time>
      </div>
    {/snippet}
  </TranscriptDisclosureHeader>
</div>
