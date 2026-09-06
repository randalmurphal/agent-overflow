<script lang="ts">
  // One catalogue row. Fixed 44px, strictly single-line: VirtualList sizes
  // every row from a constant, so anything that could wrap would desync the
  // spacer from real geometry. Title and project label are the only two
  // flexible cells; everything else is `shrink-0` and either always narrow
  // (chips, stamps) or omitted.
  //
  // The row is a listbox `option`, not a `<label>` wrapping a checkbox: the
  // list drives a roving `aria-activedescendant` cursor, and an option that
  // owns an interactive input is invalid ARIA. The check box here is a
  // decorative glyph — the whole row is the hit target.

  import CircleAlert from '@lucide/svelte/icons/circle-alert';
  import Icon from '../primitives/Icon.svelte';
  import { getProviderDefinition } from '../../providers/catalog';
  import type { ImportRowResult, ImportableSession } from '../../types/sessionImport';
  import { relativeTime } from '../../utils/format';
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
    'shrink-0 rounded-[3px] px-1 py-px text-[0.625rem] leading-4 text-fg-muted bg-surface-2/70';

  let provider = $derived(getProviderDefinition(row.provider));
  let warnings = $derived(row.warnings ?? []);
  // No thread-count chip: one selected provider session maps to one AO
  // thread, so repeating "1 thread" on every row adds no information.
  //
  // Every other chip and stamp reads from the row/result, so derive the
  // labels here rather than branching inside the template.
  let subagentLabel = $derived(row.subagentCount > 0 ? `${row.subagentCount} subagents` : '');
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
    'flex h-full min-w-0 items-center gap-2 border-b border-border-subtle/50 px-3',
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

  <span class="min-w-0 flex-[3] truncate text-fg" title={row.title}>{row.title}</span>

  <span class="min-w-0 flex-[2] truncate text-fg-muted" title={row.projectPath}>
    {row.projectLabel}
  </span>

  {#if row.gitBranch}
    <span class="max-w-[9rem] shrink-0 truncate text-fg-hint" title={row.gitBranch}>
      {row.gitBranch}
    </span>
  {/if}

  {#if subagentLabel}
    <span class={CHIP_CLASS}>{subagentLabel}</span>
  {/if}

  {#if importedFromLabel}
    <span
      class={[
        'max-w-[12rem] shrink-0 truncate rounded-[3px] px-1 py-px text-[0.625rem] leading-4',
        importedFrom?.duplicateOfThreadId
          ? 'bg-warning/10 text-warning'
          : 'bg-surface-2/70 text-fg-muted',
      ].join(' ')}
      data-testid={`session-import-origin-${row.id}`}
      title={importedFromTitle}
    >
      {importedFromLabel}
    </span>
  {/if}

  {#if !row.knownProject}
    <span
      class="shrink-0 rounded-[3px] bg-accent/10 px-1 py-px text-[0.625rem] leading-4 text-accent"
      title="Agent Overflow has no project here yet — importing creates one"
    >
      new project
    </span>
  {/if}

  {#if warnings.length > 0}
    <span class="shrink-0 text-warning" title={warnings.join('\n')}>
      <Icon icon={CircleAlert} size={12} />
    </span>
  {/if}

  {#if sizeLabel}
    <span class="w-[4.5rem] shrink-0 text-right tabular-nums text-fg-hint">{sizeLabel}</span>
  {/if}

  <span class="w-[5.5rem] shrink-0 text-right tabular-nums text-fg-hint">
    <!-- The listing is `selected`-routed, so the stamp was minted by the
         machine the person is looking at, not necessarily by home. -->
    {relativeTime(row.lastActivityAt, 'locale', getSessionImportBackend())}
  </span>

  <!-- Outcome stamp. Present only once a run has reported on this row.
       A skipped row carries prose too ("already imported / the file is
       gone"), and it is information, not a failure — so it reads in the
       muted palette rather than the error one. -->
  <span
    class="flex w-[10rem] shrink-0 items-center justify-end gap-1"
    data-testid={result ? `session-import-outcome-${row.id}` : undefined}
  >
    {#if result?.status === 'imported'}
      <span class="text-success" title="Imported">✓</span>
    {:else if result?.status === 'skipped'}
      <span class="shrink-0 text-fg-hint" title="Skipped">–</span>
      {#if result.error}
        <span class="min-w-0 truncate text-fg-hint" title={result.error}>{result.error}</span>
      {/if}
    {:else if result?.status === 'failed'}
      <span class="shrink-0 text-error">✗</span>
      {#if result.error}
        <span class="min-w-0 truncate text-error" title={result.error}>{result.error}</span>
      {/if}
    {/if}
  </span>
</div>
