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
   * combined payload still backs the sidebar.
   *
   * Legacy rows without per-file previews keep the old lazy-fetch
   * path: fetch a small payload prefix, parse it, and match by path.
   * Files without a matching parsed entry render a header-only
   * placeholder so the row still appears with its metadata.
  */
  import { untrack } from 'svelte';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { parsePatchFilesCached, type PatchFile, type PatchLine } from '../../utils/patchFiles';
  import {
    INLINE_DIFF_PAYLOAD_PREVIEW_BYTES,
    inlineDiffOmittedFiles,
    inlineDiffPreviewFiles,
  } from '../../utils/inlineThreshold';
  import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import DiffFileBlock from './DiffFileBlock.svelte';
  import { openDiffSidebar } from './diffSidebarTrigger';
  import { useLeasedPayloadExpansion } from './useLeasedPayloadExpansion.svelte';

  interface Props {
    pane?: ThreadPane;
    item: Item;
    meta: ToolResultMeta;
    payloadId?: string;
  }

  let { pane, item, meta, payloadId }: Props = $props();

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
    return files.map((metaFile) => {
      const parsedFile = previewFileFromMeta(metaFile) ?? parsedByPath.get(metaFile.path);
      return {
        path: metaFile.path,
        file: applyMetaToPatchFile(parsedFile, metaFile),
        hasMoreDiffContent:
          metaFile.previewTruncated === true ||
          (payloadPreviewIncomplete && (parsedFile === undefined || metaFile.path === lastParsedFilePath)),
      };
    });
  });

  let legacyFilesNeedMorePayload = $derived.by(() => {
    if (!expansion.hasMore) return false;
    const files = previewFiles();
    return files.some((metaFile) => !metaFile.previewPatch && !parsedByPath.get(metaFile.path));
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

  function previewFileFromMeta(metaFile: ToolInlineDiffFile): PatchFile | undefined {
    if (!metaFile.previewPatch) return undefined;
    return parsePatchFilesCached(metaFile.previewPatch)[0];
  }

  function legacyPayloadId(): string | undefined {
    const files = previewFiles();
    if (files.every((file) => file.previewPatch)) return undefined;
    return payloadId;
  }

  function previewFiles(): ToolInlineDiffFile[] {
    return inlineDiffPreviewFiles(meta.inlineDiff?.files);
  }

  function openFullDiff(): void {
    if (!pane || !payloadId) return;
    openDiffSidebar(pane, { payloadId });
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

{#each renderableFiles as { path, file, hasMoreDiffContent } (path)}
  <DiffFileBlock
    {pane}
    {file}
    {payloadId}
    threadId={item.threadId}
    itemId={item.id}
    workspacePath={paneWorkspacePath(pane)}
    toolName={item.toolName}
    createdAt={item.createdAt}
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
        ariaLabel="Open Full Diff in Side Panel"
        title="Open full diff in side panel"
        testId="diff-file-overflow-open-sidebar"
      >
        {#snippet leading()}<Icon icon={PanelRightOpen} size={12} />{/snippet}
        {#snippet children()}Open full diff{/snippet}
      </Button>
    {/if}
  </div>
{/if}
