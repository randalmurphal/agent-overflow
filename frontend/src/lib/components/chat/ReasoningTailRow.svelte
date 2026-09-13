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
  import type {
    PaneSession,
    RevealRead,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
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
  }: {
    pane?: PaneSession & RevealRead & RowUiRegistry & ScrollHost;
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
  // symptom). The live tail survives a content-consistent settle (retained
  // until the offscreen row-UI prune reclaims it) so the clamp's visible
  // lines never re-wrap in front of the reader at the settle boundary;
  // overwrite settles, removal, and remounts fall back to the trimmed
  // summary / loaded payload. See reasoningTailSource for the merge.
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
</script>

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
    <time class="shrink-0 pt-[2px] text-[0.625rem] tabular-nums text-fg-hint" datetime={isoTime}>{time}</time>
  {/snippet}
</TranscriptDisclosureHeader>
