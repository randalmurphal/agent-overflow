<script lang="ts">
  /*
   * Renders a tool_result row whose payload is a multi-file unified
   * diff (Claude Edit/Write/MultiEdit/NotebookEdit, Codex apply_patch
   * with N files). Stacks one DiffFileBlock per file inside, no outer
   * wrapper card — each block is a self-contained tool-call-style row.
   *
   * Modern tool_result metadata carries a line-bounded preview patch
   * per file, so multi-file tool calls can render one independent
   * chat row per file without byte-slicing a combined payload. The
   * combined payload still backs the review-pane affordance.
   *
   * Legacy rows without per-file previews keep the old lazy-fetch
   * path: fetch a small payload prefix, parse it, and match by path.
   * Files without a matching parsed entry render a header-only
   * placeholder so the row still appears with its metadata.
   *
   * A third case sits between them: a file whose preview the WIRE
   * PROJECTION removed (`previewElided`, internal/itemwire). Its stored
   * patch is intact and its chrome arrived whole, so it renders exactly
   * like any other collapsed file; expanding it recovers the patch
   * through GetThreadItemProjectionSource. That fetch is per ITEM, not
   * per file, so one expand recovers every elided file in the row — and
   * its spans, which keeps the highlight path identical to the
   * non-elided one. It is deliberately NOT the legacy path: the legacy
   * path fetches a payload prefix on MOUNT, which would spend on arrival
   * exactly what the projection saved.
  */
  import { untrack } from 'svelte';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import { parsePatchFilesCached, type PatchFile, type PatchLine } from '../../utils/patchFiles';
  import {
    INLINE_DIFF_PAYLOAD_PREVIEW_BYTES,
    inlineDiffOmittedFiles,
    inlineDiffPreviewFiles,
  } from '../../utils/inlineThreshold';
  import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
  import {
    readItemProjectionSource,
    requestItemProjectionSource,
    retryItemProjectionSource,
  } from '../../utils/itemProjectionSource.svelte';
  import { ingestPersistedPatchSpans } from '../../utils/persistedSpans';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { uniqueEachKeys } from '../../utils/uniqueEachKeys';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import DiffFileBlock from './DiffFileBlock.svelte';
  import { openReviewForItem } from './reviewTrigger';
  import { useLeasedPayloadExpansion } from './useLeasedPayloadExpansion.svelte';

  interface Props {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    item: Item;
    meta: ToolResultMeta;
    payloadId?: string;
  }

  let { pane, item, meta, payloadId }: Props = $props();

  // Warm the diff span cache from the payload's persisted preview
  // spans (payloads.preview_spans, joined onto the item row). The init
  // call is the cold-mount path: it seeds synchronously (tables
  // memoized) BEFORE the DiffFileBlock children mount, so their first
  // getSpansForLine reads hit and no highlight RPC fires. The
  // initial-value capture is the point — the $effect below covers the
  // blob arriving later (persist-tap write racing the row's first
  // emission, item upserts).
  // svelte-ignore state_referenced_locally
  ingestPersistedPatchSpans(item.threadId, item.payloadPreviewSpans);
  $effect(() => {
    ingestPersistedPatchSpans(item.threadId, item.payloadPreviewSpans);
  });

  // What the recovery route has returned for this row, if a reader
  // expanded an elided file. Read-only composition: the row keeps its
  // marker for its whole life and this supplies the value behind it —
  // nothing merges the two, so no cached or replicated row can ever
  // claim to be complete when it is not (utils/itemProjectionSource).
  let recovered = $derived(readItemProjectionSource(item.threadId, item.id, item.updatedAt));

  // Recovered spans warm the same cache the persisted blob does, so a
  // recovered file highlights through the identical path instead of
  // falling back to a per-file highlight RPC.
  $effect(() => {
    const spans = recovered.source?.payloadPreviewSpans;
    if (spans) ingestPersistedPatchSpans(item.threadId, spans);
  });

  // Stored preview patches, positionally aligned with `meta.inlineDiff.
  // files`: the projection deletes the `previewPatch` field in place and
  // never reorders or removes an entry, and inlineDiffPreviewFiles is a
  // prefix slice, so index i here is index i there. Positional rather
  // than path-keyed because paths are not unique across the row (see
  // renderableFileKeys below); the path is still compared before use, so
  // a shape that ever stopped lining up renders no patch instead of
  // another file's.
  let recoveredPatches = $derived.by(() => {
    const meta = recovered.source?.payloadMeta;
    if (!meta) return [] as ToolInlineDiffFile[];
    const parsed = parseJsonObject(meta) as ToolResultMeta | null;
    return parsed?.inlineDiff?.files ?? [];
  });

  // One handle per row, stable across renders so the blocks' expand
  // effects do not re-fire on every repaint. Getters keep it live.
  const elidedPreview = {
    get loading() {
      return recovered.loading;
    },
    get error() {
      return recovered.error;
    },
    request: () => requestItemProjectionSource(item.threadId, item.id, item.updatedAt),
    retry: () => retryItemProjectionSource(item.threadId, item.id, item.updatedAt),
  };

  // Always created: useLeasedPayloadExpansion returns this fallback both
  // for pane-less mounts AND for paned rows whose files all carry preview
  // patches (legacyPayloadId() === undefined — the modern path). In the
  // paned case the pane-guard in the payloadId getter keeps it inert: it
  // never fetches, it just satisfies the hook's non-null handle contract.
  const localFallback = untrack(() =>
    createPayloadExpansion(
      () => (pane ? undefined : legacyPayloadId()),
      () => item.threadId,
      {
        previewBytes: INLINE_DIFF_PAYLOAD_PREVIEW_BYTES,
        payloadVersion: () => item.updatedAt,
        loadOnMount: true,
      },
    ),
  );

  const expansionRef = useLeasedPayloadExpansion({
    getPane: () => pane,
    getPayloadId: legacyPayloadId,
    getThreadId: () => item.threadId,
    getFallback: () => localFallback,
    getOptions: () => ({
      stateKey: 'diff-stack-legacy',
      previewBytes: INLINE_DIFF_PAYLOAD_PREVIEW_BYTES,
      payloadVersion: item.updatedAt,
      loadOnMount: true,
    }),
  });
  const expansion = $derived(expansionRef.current!);

  let payloadData: string | null = $derived(expansion.displayData);
  let payloadPreviewIncomplete = $derived(payloadData !== null && !expansion.isComplete);

  let parsedFiles = $derived.by(() => {
    if (!payloadData) return [] as PatchFile[];
    return parsePatchFilesCached(payloadData);
  });

  let parsedByPath = $derived.by(() => {
    const map = new Map<string, PatchFile>();
    for (const file of parsedFiles) {
      map.set(file.path, file);
    }
    return map;
  });

  let renderableFiles = $derived.by(() => {
    const files = previewFiles();
    const lastParsedFilePath = parsedFiles.at(-1)?.path ?? null;
    return files.map((metaFile, index) => {
      const patch = metaFile.previewPatch ?? recoveredPatch(metaFile, index);
      const parsedFile = patch
        ? parsePatchFilesCached(patch)[0]
        : parsedByPath.get(metaFile.path);
      return {
        path: metaFile.path,
        file: applyMetaToPatchFile(parsedFile, metaFile),
        hasMoreDiffContent:
          metaFile.previewTruncated === true ||
          (payloadPreviewIncomplete && (parsedFile === undefined || metaFile.path === lastParsedFilePath)),
        // Present only while the file's patch is still behind the
        // marker: once recovered it renders like any other file, and
        // the affordance goes with it.
        elidedPreview: metaFile.previewElided === true && !patch ? elidedPreview : undefined,
      };
    });
  });

  // Paths are NOT unique across the wire's `changes[]`: triage's
  // buildInlineDiffFromChanges appends one entry per change with no path
  // dedupe, and a rename's Path is rewritten to the move destination — so a
  // rename A→B plus a separate edit of B yields two entries keyed B, and a
  // keyed `{#each}` over them throws `each_key_duplicate` (an aborted flush,
  // utils/uniqueEachKeys.ts). Deduping would drop a distinct previewPatch,
  // so the repair is key-side only.
  let renderableFileKeys = $derived(uniqueEachKeys(renderableFiles, (entry) => entry.path));

  let legacyFilesNeedMorePayload = $derived.by(() => {
    if (!expansion.hasMore) return false;
    const files = previewFiles();
    return files.some(
      (metaFile) =>
        !metaFile.previewPatch &&
        !metaFile.previewElided &&
        !parsedByPath.get(metaFile.path),
    );
  });

  let totalFiles = $derived(meta.inlineDiff?.totalFiles ?? meta.inlineDiff?.files.length ?? 0);
  let omittedFiles = $derived.by(() => {
    return inlineDiffOmittedFiles(
      totalFiles,
      previewFiles().length,
      meta.inlineDiff?.omittedFiles,
    );
  });
  let canOpenFullDiff = $derived(Boolean(pane && payloadId));

  $effect(() => {
    const needsMore = legacyFilesNeedMorePayload;
    const loading = expansion.loading;
    if (!needsMore || loading) return;
    untrack(() => {
      void expansion.showFull();
    });
  });

  function recoveredPatch(metaFile: ToolInlineDiffFile, index: number): string | undefined {
    const stored = recoveredPatches[index];
    if (!stored || stored.path !== metaFile.path) return undefined;
    return stored.previewPatch;
  }

  // Elided files are NOT legacy. The legacy path fetches a payload
  // prefix on mount for a row that never carried per-file previews; an
  // elided file's row does carry them, and its recovery is a per-item
  // fetch on expand. Treating one as the other would reintroduce the
  // arrival-time fetch this projection exists to avoid.
  function legacyPayloadId(): string | undefined {
    const files = previewFiles();
    if (files.every((file) => file.previewPatch || file.previewElided)) return undefined;
    return payloadId;
  }

  function previewFiles(): ToolInlineDiffFile[] {
    return inlineDiffPreviewFiles(meta.inlineDiff?.files);
  }

  function openFullDiff(): void {
    if (!pane || !payloadId) return;
    openReviewForItem(pane, { editItemId: item.id });
  }

  function applyMetaToPatchFile(
    parsedFile: PatchFile | undefined,
    metaFile: ToolInlineDiffFile,
  ): PatchFile {
    const file = parsedFile ?? fallbackFromMeta(metaFile);
    return {
      ...file,
      kind: metaFile.kind ?? file.kind,
      additions: metaFile.insertions ?? file.additions,
      deletions: metaFile.deletions ?? file.deletions,
    };
  }

  function fallbackFromMeta(metaFile: ToolInlineDiffFile): PatchFile {
    return {
      path: metaFile.path,
      kind: metaFile.kind ?? 'modified',
      additions: metaFile.insertions ?? 0,
      deletions: metaFile.deletions ?? 0,
      lines: [] as PatchLine[],
    };
  }
</script>

{#each renderableFiles as { file, hasMoreDiffContent, elidedPreview: elided }, fileIndex (renderableFileKeys[fileIndex] ?? fileIndex)}
  <DiffFileBlock
    {pane}
    {file}
    {payloadId}
    elidedPreview={elided}
    threadId={item.threadId}
    itemId={item.id}
    workspacePath={paneWorkspacePath(pane)}
    toolName={item.toolName}
    createdAt={item.createdAt}
    statusItem={item}
    {hasMoreDiffContent}
  />
{/each}

{#if omittedFiles > 0}
  <div
    class="mx-auto mt-1 flex w-full max-w-[62rem] items-center justify-between gap-3 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/35 px-3 py-2 text-[0.75rem] text-text-secondary"
    data-testid="diff-file-overflow"
  >
    <span class="min-w-0 truncate">
      {omittedFiles} more {omittedFiles === 1 ? 'file' : 'files'} changed
      {#if totalFiles > 0}<span class="text-fg-subtle"> · {totalFiles} total</span>{/if}
    </span>
    {#if canOpenFullDiff}
      <Button
        variant="ghost"
        size="xs"
        onclick={openFullDiff}
        ariaLabel="Open full diff in review pane"
        title="Open full diff in review pane"
        testId="diff-file-overflow-open-sidebar"
      >
        {#snippet leading()}<Icon icon={PanelRightOpen} size={12} />{/snippet}
        {#snippet children()}Open full diff{/snippet}
      </Button>
    {/if}
  </div>
{/if}
