<script lang="ts">
  // Compact row that renders a run of consecutive Read tool_calls as
  // a single line with a wrapped list of file names. No expansion
  // body — each member is reachable via its own EditorLink. The row
  // geometry intentionally mirrors `TranscriptDisclosureHeader`'s
  // chev / icon / label / body columns so it lines up with the
  // adjacent tool rows under the continuous left rail. The chev slot
  // renders an empty placeholder to keep the column width stable
  // (matches non-expandable disclosure rows).

  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import type { ReadGroupNode } from '../../utils/subagentGrouping';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { presentToolCardInputPreview } from './toolCardPreview';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import EditorLink from '../common/EditorLink.svelte';

  let {
    pane,
    group,
  }: {
    pane?: ThreadPane;
    group: ReadGroupNode;
  } = $props();

  let workspacePath = $derived(paneWorkspacePath(pane));

  interface ReadEntry {
    id: string;
    /** Path passed to EditorLink — workspace-relative when the read
     *  was inside the workspace, absolute otherwise. */
    path: string;
    line: number;
    col: number;
    /** Text rendered as the link label. Matches `path` whenever the
     *  presenter could relativize, otherwise falls back to whatever
     *  `presentToolCardInputPreview` produced for the row's body. */
    display: string;
  }

  interface DisplayReadEntry extends ReadEntry {
    label: string;
  }

  let entries = $derived<ReadEntry[]>(group.members.map((item) => entryFor(item, workspacePath)));
  let displayEntries = $derived<DisplayReadEntry[]>(labelDuplicateBasenames(entries));

  function entryFor(item: Item, workspacePath: string): ReadEntry {
    const summaryMeta = parseJsonObject(item.payloadMeta);
    const displayMeta = parseJsonObject(item.meta);
    const preview = presentToolCardInputPreview(item, summaryMeta, displayMeta, workspacePath);
    const fallback = preview.text;
    return {
      id: item.id,
      path: preview.path?.path ?? fallback,
      line: preview.path?.line ?? 0,
      col: preview.path?.col ?? 0,
      display: fallback,
    };
  }

  function labelDuplicateBasenames(entries: ReadEntry[]): DisplayReadEntry[] {
    const pathsByBasename = new Map<string, Set<string>>();
    for (const entry of entries) {
      const basename = basenameOf(entry.path);
      const paths = pathsByBasename.get(basename) ?? new Set<string>();
      paths.add(entry.path);
      pathsByBasename.set(basename, paths);
    }
    return entries.map((entry) => {
      const shouldShowPath = (pathsByBasename.get(basenameOf(entry.path))?.size ?? 0) > 1;
      return {
        ...entry,
        label: shouldShowPath ? withLocation(entry.path, entry.line, entry.col) : entry.display,
      };
    });
  }

  function basenameOf(path: string): string {
    const lastSep = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'));
    return lastSep === -1 ? path : path.slice(lastSep + 1);
  }

  function withLocation(path: string, line: number, col: number): string {
    if (line <= 0) return path;
    return col > 0 ? `${path}:${line}:${col}` : `${path}:${line}`;
  }
</script>

<div
  class="flex w-full items-baseline gap-2 py-0.5"
  data-testid="read-group-row"
  data-tool-kind="eye"
>
  <span class="size-3 shrink-0 opacity-30" aria-hidden="true"></span>
  <span class="size-3.5 shrink-0 inline-flex items-center justify-center">
    <ToolKindIcon kind="eye" ariaLabel="reads" />
  </span>
  <span
    class="w-12 shrink-0 truncate text-[11px] text-fg-hint"
    data-testid="read-group-row-label"
  >reads</span>
  <span
    class="min-w-0 flex-1 inline-flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-[12px] text-fg-muted/75"
    data-testid="read-group-row-list"
  >
    {#each displayEntries as entry (entry.id)}
      <EditorLink
        path={entry.path}
        line={entry.line}
        col={entry.col}
        workspacePath={workspacePath}
        label={entry.label}
        openLabel={entry.label}
        class="max-w-full break-all text-fg-muted/75 hover:text-accent"
      />
    {/each}
  </span>
</div>
