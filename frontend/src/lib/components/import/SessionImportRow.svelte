<script lang="ts">
  // One catalogue row. Fixed 44px, strictly single-line: VirtualList sizes
  // every row from a constant, so anything that could wrap would desync the
  // spacer from real geometry.
  //
  // Rows are separate virtualized elements, so the columns line up only
  // because every row lays out on the same grid tracks (ROW_GRID). Every
  // cell is always present, empty when it has nothing to say; optional
  // chips and a run's outcome reason sit inside the title or project cell
  // they describe instead of taking a track of their own, so they shorten
  // their own row's title and never move a column. The title is the one
  // flexible track; every other track is sized to what it shows, and the
  // outcome track is one glyph wide whether or not a run has reported, so
  // an outcome arriving moves nothing. Below the `import` container's 3xl
  // width (SessionImportList) the branch and size tracks drop, the chips
  // collapse to icon forms and the reason moves to the glyph's tooltip.
  //
  // The row is a listbox `option`, not a `<label>` wrapping a checkbox: the
  // list drives a roving `aria-activedescendant` cursor, and an option that
  // owns an interactive input is invalid ARIA. The check box here is a
  // decorative glyph — the whole row is the hit target.

  import Bot from '@lucide/svelte/icons/bot';
  import CircleAlert from '@lucide/svelte/icons/circle-alert';
  import FolderPlus from '@lucide/svelte/icons/folder-plus';
  import Import from '@lucide/svelte/icons/import';
  import Icon from '../primitives/Icon.svelte';
  import { getProviderDefinition } from '../../providers/catalog';
  import type { ImportRowResult, ImportableSession } from '../../types/sessionImport';
  import { countNoun, relativeTime } from '../../utils/format';
  import { getSessionImportBackend } from '../../stores/sessionImport.svelte';
  import { formatPayloadSize } from '../../utils/payloadExpansion.svelte';

  interface Props {
    row: ImportableSession;
    /** DOM id — the list points `aria-activedescendant` at it. */
    domId: string;
    selected: boolean;
    /** The roving keyboard cursor sits on this row. */
    active: boolean;
    /** Outcome folded from progress frames; undefined outside a run. */
    result: ImportRowResult | undefined;
    disabled: boolean;
    onToggle: () => void;
  }

  let { row, domId, selected, active, result, disabled, onToggle }: Props = $props();

  const CHIP_CLASS =
    'inline-flex shrink-0 items-center gap-0.5 rounded-[3px] px-1 py-px text-[0.625rem] leading-4 tabular-nums';
  // The full label in the wide layout; the icon form in the narrow one.
  const WIDE_ONLY = '@max-3xl/import:hidden';
  const NARROW_ONLY = 'hidden @max-3xl/import:inline-flex items-center gap-0.5';

  const ROW_GRID = [
    'grid grid-cols-[0.875rem_1rem_minmax(0,1fr)_minmax(0,12rem)_9rem_4.5rem_5.5rem_1rem]',
    '@max-3xl/import:grid-cols-[0.875rem_1rem_minmax(0,1fr)_minmax(0,6rem)_4rem_1rem]',
  ].join(' ');

  let provider = $derived(getProviderDefinition(row.provider));
  let warnings = $derived(row.warnings ?? []);
  // No thread-count chip: one selected provider session maps to one AO
  // thread, so repeating "1 thread" on every row adds no information.
  //
  // Every other chip and stamp reads from the row/result, so derive the
  // labels here rather than branching inside the template.
  let subagentLabel = $derived(row.subagentCount > 0 ? countNoun(row.subagentCount, 'subagent') : '');
  let sizeLabel = $derived(row.sizeBytes > 0 ? formatPayloadSize(row.sizeBytes) : '');
  // These rows are hidden by default and look like any other when shown, so
  // the answer to "why is this one here?" is a hover away rather than a chip
  // spending row width. The marker names WHAT recorded the provenance, which
  // is the follow-up question.
  let alreadyRanTitle = $derived.by(() => {
    if (!row.ranInAgentOverflow) return undefined;
    const base = 'This session already ran in Agent Overflow';
    return row.origin ? `${base} (${row.origin})` : base;
  });

  // Codex can import a conversation from another coding agent; when it has,
  // the resulting session looks like any other Codex session and its real
  // origin is only in Codex's own ledger. The backend derives the agent id,
  // so this map is a LABEL table, not a membership test — an id it does not
  // know still gets a badge, just a generic one.
  const EXTERNAL_AGENT_LABELS: Record<string, string> = {
    'claude-code': 'Claude Code',
    cursor: 'Cursor',
  };
  let importedFrom = $derived(row.importedFrom ?? undefined);
  let importedFromLabel = $derived.by(() => {
    if (!importedFrom) return '';
    const agent = EXTERNAL_AGENT_LABELS[importedFrom.agent];
    const base = agent ? `from ${agent}` : 'imported into Codex';
    // The duplicate half earns its width: without it the user is offered the
    // same conversation twice with nothing to tell them apart.
    return importedFrom.duplicateOfThreadId ? `${base} · duplicate` : base;
  });
  let importedFromTitle = $derived.by(() => {
    if (!importedFrom) return undefined;
    const agent = EXTERNAL_AGENT_LABELS[importedFrom.agent] ?? 'another agent';
    const lines = [`Codex imported this conversation from ${agent}.`];
    if (importedFrom.sourcePath) lines.push(importedFrom.sourcePath);
    if (importedFrom.duplicateOfThreadId) {
      lines.push('Agent Overflow already has this conversation, imported directly.');
    }
    return lines.join('\n');
  });
  // What the outcome glyph stands for, for its tooltip and for the reason
  // the title cell shows beside the title.
  let outcomeReason = $derived(
    result && result.status !== 'imported' ? (result.error ?? '') : '',
  );

  function handleClick(): void {
    if (disabled) return;
    onToggle();
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- Keyboard lives one level up: SessionImportModal owns the roving cursor
     (Arrow/Space/Enter) for the whole surface, so a per-row key handler
     would be a second, competing implementation. `tabindex={-1}` for the
     same reason: in an aria-activedescendant listbox the container holds
     focus and points at the active option; the options are never tab stops. -->
<div
  id={domId}
  role="option"
  aria-selected={selected}
  tabindex={-1}
  aria-disabled={disabled ? 'true' : undefined}
  data-testid={`session-import-row-${row.id}`}
  data-active={active ? 'true' : undefined}
  title={alreadyRanTitle}
  onclick={handleClick}
  class={[
    ROW_GRID,
    'h-full min-w-0 items-center gap-2 border-b border-border-subtle/50 px-3',
    'text-[0.75rem] select-none',
    disabled ? 'cursor-default' : 'cursor-pointer hover:bg-surface-2/40',
    selected ? 'bg-accent/10' : '',
    active ? 'ring-1 ring-inset ring-accent/50' : '',
  ].join(' ')}
>
  <span
    aria-hidden="true"
    class={[
      'inline-flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded-[3px] border',
      'text-[0.5625rem] leading-none',
      selected ? 'border-accent bg-accent text-surface-0' : 'border-border-strong',
    ].join(' ')}
  >
    {selected ? '✓' : ''}
  </span>

  <span
    class={[
      'inline-flex h-4 w-4 shrink-0 items-center justify-center rounded-[3px]',
      'text-[0.625rem] font-semibold',
      provider?.badgeClass ?? 'bg-surface-2 text-fg-muted',
    ].join(' ')}
    title={provider?.label ?? row.provider}
  >
    {provider?.shortLabel ?? '?'}
  </span>

  <span data-col="title" class="flex min-w-0 items-center gap-2">
    <span class="min-w-0 truncate text-fg" title={row.title}>{row.title}</span>

    {#if subagentLabel}
      <span
        class="{CHIP_CLASS} bg-surface-2/70 text-fg-muted"
        title={subagentLabel}
        aria-label={subagentLabel}
        data-testid={`session-import-subagents-${row.id}`}
      >
        <span class={WIDE_ONLY}>{subagentLabel}</span>
        <span class={NARROW_ONLY} aria-hidden="true">
          <Icon icon={Bot} size={10} />{row.subagentCount}
        </span>
      </span>
    {/if}

    {#if importedFromLabel}
      <span
        class={[
          CHIP_CLASS,
          'max-w-[12rem] truncate',
          importedFrom?.duplicateOfThreadId
            ? 'bg-warning/10 text-warning'
            : 'bg-surface-2/70 text-fg-muted',
        ].join(' ')}
        data-testid={`session-import-origin-${row.id}`}
        title={importedFromTitle}
        aria-label={importedFromLabel}
      >
        <span class="{WIDE_ONLY} truncate">{importedFromLabel}</span>
        <span class={NARROW_ONLY} aria-hidden="true"><Icon icon={Import} size={10} /></span>
      </span>
    {/if}

    {#if warnings.length > 0}
      <span class="shrink-0 text-warning" title={warnings.join('\n')}>
        <Icon icon={CircleAlert} size={12} />
      </span>
    {/if}

    <!-- A run's reason for a skipped or failed row. A skip is information,
         not a failure, so it reads in the muted palette. -->
    {#if outcomeReason}
      <span
        class={[
          'min-w-0 max-w-[50%] truncate',
          WIDE_ONLY,
          result?.status === 'failed' ? 'text-error' : 'text-fg-hint',
        ].join(' ')}
        title={outcomeReason}
        data-testid={`session-import-outcome-reason-${row.id}`}
      >
        {outcomeReason}
      </span>
    {/if}
  </span>

  <span data-col="project" class="flex min-w-0 items-center gap-2">
    <span class="min-w-0 truncate text-fg-muted" title={row.projectPath}>{row.projectLabel}</span>

    {#if !row.knownProject}
      <span
        class="{CHIP_CLASS} bg-accent/10 text-accent"
        title="Agent Overflow has no project here yet — importing creates one"
        aria-label="new project"
      >
        <span class={WIDE_ONLY}>new project</span>
        <span class={NARROW_ONLY} aria-hidden="true"><Icon icon={FolderPlus} size={10} /></span>
      </span>
    {/if}
  </span>

  <span
    data-col="branch"
    class="min-w-0 truncate text-fg-hint @max-3xl/import:hidden"
    title={row.gitBranch || undefined}
  >
    {row.gitBranch}
  </span>

  <span class="text-right tabular-nums text-fg-hint @max-3xl/import:hidden">{sizeLabel}</span>

  <span data-col="time" class="text-right tabular-nums text-fg-hint">
    <!-- The listing is `selected`-routed, so the stamp was minted by the
         machine the person is looking at, not necessarily by home. -->
    {relativeTime(row.lastActivityAt, 'locale', getSessionImportBackend())}
  </span>

  <!-- Outcome glyph. Present only once a run has reported on this row; its
       reason is beside the title (wide) and in its tooltip (always). -->
  <span
    data-col="outcome"
    class="flex justify-end"
    data-testid={result ? `session-import-outcome-${row.id}` : undefined}
  >
    {#if result?.status === 'imported'}
      <span class="text-success" title="Imported">✓</span>
    {:else if result?.status === 'skipped'}
      <span class="text-fg-hint" title={outcomeReason || 'Skipped'}>–</span>
    {:else if result?.status === 'failed'}
      <span class="text-error" title={outcomeReason || 'Failed'}>✗</span>
    {/if}
  </span>
</div>
