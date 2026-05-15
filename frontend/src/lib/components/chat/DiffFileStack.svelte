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
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { parsePatchFiles, type PatchFile, type PatchLine } from '../../utils/patchFiles';
  import { INLINE_DIFF_PAYLOAD_PREVIEW_BYTES } from '../../utils/inlineThreshold';
  import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
  import DiffFileBlock from './DiffFileBlock.svelte';

  interface Props {
    pane?: ThreadPane;
    item: Item;
    meta: ToolResultMeta;
    payloadId?: string;
  }

  let { pane, item, meta, payloadId }: Props = $props();

  // Local expansion handle. Reads payloadId/threadId straight off the
  // props — no pane-registry indirection — so the fetch fires
  // reliably as soon as the component mounts with a valid payloadId.
  // The cost is one expansion handle per row mount (vs. a shared
  // pane-keyed handle); for diff rows this is fine because each row
  // owns one fetch and there's no expand/collapse interaction sharing
  // state across mounts.
  //
  // Legacy rows still auto-load on mount. That does two things:
  //   1. Synchronously hydrates from the module-level payload cache
  //      when (threadId, payloadId, updatedAt) hits — so re-entering
  //      a thread we've already loaded paints with full diff content
  //      at frame 0 (no empty-then-loaded oscillation that whipsaws
  //      virtua's per-row size cache and the controller's contentRO).
  //   2. Drives `expand()` itself so we don't carry a per-component
  //      $effect just to trigger the fetch.
  const expansion = createPayloadExpansion(
    () => legacyPayloadId(),
    () => item.threadId,
    {
      previewBytes: INLINE_DIFF_PAYLOAD_PREVIEW_BYTES,
      payloadVersion: () => item.updatedAt,
      loadOnMount: true,
    },
  );

  let payloadData: string | null = $derived(expansion.displayData);
  let payloadPreviewIncomplete = $derived(payloadData !== null && !expansion.isComplete);

  let parsedFiles = $derived.by(() => {
    if (!payloadData) return [] as PatchFile[];
    return parsePatchFiles(payloadData);
  });

  let parsedByPath = $derived.by(() => {
    const map = new Map<string, PatchFile>();
    for (const file of parsedFiles) {
      map.set(file.path, file);
    }
    return map;
  });

  let renderableFiles = $derived.by(() => {
    const files = meta.inlineDiff?.files ?? [];
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
    const files = meta.inlineDiff?.files ?? [];
    return files.some((metaFile) => !metaFile.previewPatch && !parsedByPath.get(metaFile.path));
  });

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
    return parsePatchFiles(metaFile.previewPatch)[0];
  }

  function legacyPayloadId(): string | undefined {
    const files = meta.inlineDiff?.files ?? [];
    if (files.every((file) => file.previewPatch)) return undefined;
    return payloadId;
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
    workspacePath={paneWorkspacePath(pane)}
    toolName={item.toolName}
    {hasMoreDiffContent}
  />
{/each}
