<script lang="ts">
  /*
   * Renders a tool_result row whose payload is a multi-file unified
   * diff (Claude Edit/Write/MultiEdit/NotebookEdit, Codex apply_patch
   * with N files). Stacks one DiffFileBlock per file inside, no outer
   * wrapper card — each block is a self-contained tool-call-style row.
   *
   * Lazy-fetches the payload preview on mount via a LOCAL
   * createPayloadExpansion handle. We deliberately don't reach into
   * the pane's expansion-state registry here: the registry's
   * payloadId lookup goes through `getCurrentItem()` against
   * pane.items, which can be slightly behind the prop the parent
   * already has. Reading payloadId directly from the prop makes the
   * fetch fire reliably the moment we mount.
   *
   * The preview fetch is intentionally small because DiffFileBlock
   * caps chat rendering to the inline diff preview limit. The sidebar
   * owns full-payload loading.
   *
   * Each file's slice is computed via `parsePatchFiles(payloadData)`
   * + match by path. Files in `meta.inlineDiff.files` without a
   * matching parsed entry (summary-only path: NotebookEdit pre-
   * upgrade, Codex pre-upgrade) render a header-only placeholder
   * PatchFile so the row still appears with its metadata.
   */
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { parsePatchFiles, type PatchFile, type PatchLine } from '../../utils/patchFiles';
  import { INLINE_DIFF_PAYLOAD_PREVIEW_BYTES } from '../../utils/inlineThreshold';
  import { createPayloadExpansion } from './payloadExpansion.svelte';
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
  // `loadOnMount: true` does two things on every mount:
  //   1. Synchronously hydrates from the module-level payload cache
  //      when (threadId, payloadId, updatedAt) hits — so re-entering
  //      a thread we've already loaded paints with full diff content
  //      at frame 0 (no empty-then-loaded oscillation that whipsaws
  //      virtua's per-row size cache and the controller's contentRO).
  //   2. Drives `expand()` itself so we don't carry a per-component
  //      $effect just to trigger the fetch.
  const expansion = createPayloadExpansion(
    () => payloadId,
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
      const parsedFile = parsedByPath.get(metaFile.path);
      return {
        path: metaFile.path,
        file: parsedFile ?? fallbackFromMeta(metaFile),
        hasMoreDiffContent:
          payloadPreviewIncomplete && (parsedFile === undefined || metaFile.path === lastParsedFilePath),
      };
    });
  });

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
