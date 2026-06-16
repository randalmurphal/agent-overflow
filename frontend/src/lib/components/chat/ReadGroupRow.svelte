<script lang="ts">
  // Stable read row for one or more adjacent Read tool_calls. A
  // single Read projects through this component from first render, so
  // adding another adjacent Read appends a file link instead of
  // replacing GenericToolCallRow with a different row shell.

  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import type { ReadGroupNode } from '../../utils/subagentGrouping';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { formatTimeOfDay } from '../../utils/format';
  import { presentToolCardInputPreview } from './toolCardPreview';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorWithFallback } from './rowState';

  let {
    pane,
    group,
  }: {
    pane?: ThreadPane;
    group: ReadGroupNode;
  } = $props();

  let workspacePath = $derived(paneWorkspacePath(pane));
  let members = $derived(group.members.map((item) => pane?.getItemById?.(item.id) ?? item));
  let firstMember = $derived(members[0] ?? null);
  let statusProjection = $derived(statusProjectionFor(members));
  let statusItem = $derived(statusProjection?.item ?? null);
  let statusMeta = $derived(statusProjection?.meta ?? null);
  let indicatorState = $derived(
    statusItem ? indicatorStateForItem(statusItem, { meta: statusMeta }) : null,
  );
  let rowError = $derived(
    statusItem
      ? rowErrorWithFallback(statusItem, { meta: statusMeta, fallback: 'Read failed' })
      : null,
  );
  let timestampSlot = $derived(
    firstMember === null
      ? undefined
      : {
          testId: 'read-group-row-time',
          value: firstMember.createdAt,
          label: formatTimeOfDay(firstMember.createdAt),
        },
  );

  interface ReadEntry {
    id: string;
    /** Path passed to EditorLink — workspace-relative when the read
     *  was inside the workspace, absolute otherwise. Undefined means
     *  the preview could not prove a file target and must render as
     *  plain escaped text, not as an editor-open link. */
    path?: string;
    line: number;
    col: number;
    /** Text rendered as the link label. Repo-local paths usually
     *  collapse to a basename; outside-workspace absolute paths stay
     *  absolute. */
    display: string;
  }

  interface DisplayReadEntry extends ReadEntry {
    label: string;
  }

  interface StatusProjection {
    item: Item;
    meta: Record<string, unknown> | null;
  }

  let rawEntries = $derived<ReadEntry[]>(members.map((item) => entryFor(item, workspacePath)));
  let entries = $derived<ReadEntry[]>(
    rawEntries.length <= 1 ? rawEntries : dedupeEntries(rawEntries),
  );
  let displayEntries = $derived<DisplayReadEntry[]>(
    entries.length <= 1
      ? entries.map((entry) => ({ ...entry, label: entry.display }))
      : labelDuplicateBasenames(entries),
  );

  function entryFor(item: Item, workspacePath: string): ReadEntry {
    const summaryMeta = parseJsonObject(item.payloadMeta);
    const displayMeta = parseJsonObject(item.meta);
    const preview = presentToolCardInputPreview(item, summaryMeta, displayMeta, workspacePath);
    const fallback = preview.text;
    return {
      id: item.id,
      path: preview.path?.path,
      line: preview.path?.line ?? 0,
      col: preview.path?.col ?? 0,
      display: fallback,
    };
  }

  function statusProjectionFor(members: Item[]): StatusProjection | null {
    for (let i = members.length - 1; i >= 0; i -= 1) {
      const member = members[i];
      if (member.status === 'running' || member.status === 'streaming') {
        return { item: member, meta: parseJsonObject(member.payloadMeta) };
      }
    }
    for (let i = members.length - 1; i >= 0; i -= 1) {
      const member = members[i];
      const meta = parseJsonObject(member.payloadMeta);
      const state = indicatorStateForItem(member, { meta });
      if (state === 'error' || state === 'declined') return { item: member, meta };
    }
    const first = members[0];
    return first ? { item: first, meta: parseJsonObject(first.payloadMeta) } : null;
  }

  function labelDuplicateBasenames(entries: ReadEntry[]): DisplayReadEntry[] {
    const pathsByBasename = new Map<string, Set<string>>();
    for (const entry of entries) {
      if (!entry.path) continue;
      const basename = basenameOf(entry.path);
      const paths = pathsByBasename.get(basename) ?? new Set<string>();
      paths.add(entry.path);
      pathsByBasename.set(basename, paths);
    }
    return entries.map((entry) => {
      if (!entry.path) return { ...entry, label: entry.display };
      const shouldShowPath = (pathsByBasename.get(basenameOf(entry.path))?.size ?? 0) > 1;
      return {
        ...entry,
        label: shouldShowPath ? withLocation(entry.path, entry.line, entry.col) : entry.display,
      };
    });
  }

  function dedupeEntries(entries: ReadEntry[]): ReadEntry[] {
    const seen = new Set<string>();
    const unique: ReadEntry[] = [];
    for (const entry of entries) {
      const key = readTargetKey(entry);
      if (seen.has(key)) continue;
      seen.add(key);
      unique.push(entry);
    }
    return unique;
  }

  function readTargetKey(entry: ReadEntry): string {
    if (!entry.path) return `item:${entry.id}`;
    return JSON.stringify([entry.path, entry.line, entry.col]);
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

<div class="group/tool overflow-hidden" data-testid="read-group-row" data-tool-kind="eye">
  <TranscriptDisclosureHeader
    expanded={false}
    expandable={false}
    testId="read-group-row-toggle"
    interactiveBody
    class="rounded-[var(--radius-control)] px-1 py-1"
  >
    {#snippet icon()}<ToolKindIcon kind="eye" ariaLabel="read" />{/snippet}
    {#snippet label()}<span data-testid="read-group-row-label">read</span>{/snippet}
    {#snippet body()}
      <span
        class="min-w-0 flex-1 inline-flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-[0.75rem] text-fg-muted/75"
        data-testid="read-group-row-list"
      >
        {#each displayEntries as entry (entry.id)}
          {#if entry.path}
            <EditorLink
              path={entry.path}
              line={entry.line}
              col={entry.col}
              workspacePath={workspacePath}
              label={entry.label}
              openLabel={entry.label}
              tone="inherit"
              class="max-w-full break-all text-fg-muted/75 hover:text-accent focus-visible:text-accent"
            />
          {:else}
            <span class="max-w-full break-all text-fg-muted/75">{entry.label}</span>
          {/if}
        {/each}
      </span>
    {/snippet}
    {#snippet actions()}
      {#if statusItem}
        <ToolDecisionChip decision={statusItem.decision} />
      {/if}
      <ToolHeaderMeta statusSlotTestId="read-group-row-status-slot" timestamp={timestampSlot}>
        {#snippet status()}
          {#if statusItem}
            <ToolRowStatusIndicator
              item={statusItem}
              state={indicatorState}
              testId="read-group-row-status"
            />
          {/if}
        {/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if rowError}
    <div class="ml-[5.25rem] px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}
</div>
