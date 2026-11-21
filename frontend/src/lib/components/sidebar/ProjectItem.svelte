<script lang="ts">
  import { isLocalEnter } from '../../utils/localEnter';
  import { projectHasScope } from '../../transport/entityScopes';
  // A single project row. Chevron + folder + name + new-group and new-thread
  // actions on the right. Right-click opens a small context menu
  // (Rename / Archive / Delete) rendered via ProjectContextMenu.
  //
  // Keeping the rename flow inline (not in the context menu component)
  // lets the row render the input in-place of the project name without
  // piping state across a component boundary.
  //
  // Collapsed-state polish:
  //   - The chevron crossfades with a "project status rollup" dot — the
  //     most-important display status across this project's top-level
  //     threads. The dot tells the user "something needs attention in
  //     here" without expanding; the chevron reappears on hover.
  //   - If the active thread belongs to this project, it renders as a
  //     single row underneath the collapsed header so the user never
  //     loses sight of where they are.

  import type { Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { RefreshProjectIdentity, RenameProject } from '../../stores/bindings';
  import { updateProjectLocal } from '../../stores/projects.svelte';
  import { getProjectSortMode } from '../../stores/sidebar.svelte';
  import {
    getEffectiveThreadStatus,
    getThreadWaitingOn,
  } from '../../stores/threadStatuses.svelte';
  import { unreachableThreadMachineName } from '../../stores/attachedBackends.svelte';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import FolderOpen from '@lucide/svelte/icons/folder-open';
  import FolderPlus from '@lucide/svelte/icons/folder-plus';
  import Plus from '@lucide/svelte/icons/plus';
  import { openNewAssistantDialog } from '../../stores/newAssistant.svelte';
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import Icon from '../primitives/Icon.svelte';
  import ProjectContextMenu from './ProjectContextMenu.svelte';
  import SidebarRowMenuButton from './SidebarRowMenuButton.svelte';
  import ProjectThreadList from './ProjectThreadList.svelte';
  import ThreadRow from './ThreadRow.svelte';
  import { buildSidebarThreadTree } from '../../utils/sidebarTree';
  import { rollupDisplayStatus } from '../../utils/sidebarTreeView';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import {
    shouldOpenProjectThreadInNewPane,
    type ProjectNewThreadHandler,
    type ProjectNewTerminalHandler,
  } from './projectNewThread';
  import { newThreadGroupInProject } from './threadGroupActions';
  import {
    beginProjectDrag,
    computeReorderedIds,
    endProjectDrag,
    getDraggingProjectId,
    getDropPosition,
    getDropTargetProjectId,
    updateDropTarget,
  } from '../../stores/projectDnd.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { formatPathLabel, type PathLabel } from '../../utils/pathDisplay';
  import { projectTarget, type ProjectTarget, type SidebarEntry } from './sidebarEntries';

  interface Props {
    /** Everything this row shows and acts on (./sidebarEntries.ts). */
    entry: SidebarEntry;
    /** The row's label, disambiguated by the containing list against the
     *  rows beside it. */
    displayLabel: PathLabel;
    threads: Thread[];
    /** The entry's groups, already search-filtered by ProjectsSection. */
    groups?: readonly ThreadGroup[];
    pane: ThreadPane | null;
    /** Called with the project id when the user clicks the new-thread button
     * (or otherwise signals "create a new thread in this project"). */
    onNewThread?: ProjectNewThreadHandler;
    /** Called with the project id when the user picks New Terminal from the
     *  compact context menu — opens a fresh terminal pane rooted at this
     *  project. Desktop has no per-project terminal control; terminals open
     *  from the palette, the chat header or the global control. */
    onNewTerminal?: ProjectNewTerminalHandler;
    /** Current rendered ordering of project ids (visible entries in
     *  ProjectsSection). Required for DnD to compute the new order on
     *  drop without an extra round-trip. */
    orderedIds?: readonly string[];
    /** Commit a new ordering. Caller updates store + persists. */
    onReorder?: (newOrderedIds: string[]) => void;
    /** Fold or unfold this entry. The state belongs to the entry, so the
     *  section writes it by key; this row only reports the gesture. */
    onSetExpanded?: (key: string, expanded: boolean) => void;
    /** Draws the hairline divider that separates this project from the one above. */
    separatedFromPrevious?: boolean;
  }

  let {
    entry,
    displayLabel,
    threads,
    groups = [],
    pane,
    onNewThread,
    onNewTerminal,
    orderedIds,
    onReorder,
    onSetExpanded,
    separatedFromPrevious = false,
  }: Props = $props();

  // Creating a thread rides `threads:operate`. The control stays in the
  // row and goes inert rather than disappearing — a project whose row lost
  // an affordance reads as a broken sidebar, not as a read-only one.
  // The hover-reveal is untouched, so nothing new is visible at rest.
  let newThreadUngranted = $derived(!projectHasScope('threads:operate', entry.id));
  // Compact: no drag (a phone's long press is the menu, and Android starts a
  // drag on a held draggable), no text selection under a hold, a taller
  // header, and the create controls the hover would have revealed.
  let compact = $derived(isCompactLayout());

  let rowEl: HTMLDivElement | undefined = $state(undefined);
  // The header line, not `rowEl`: an expanded project's container runs
  // the full height of its thread list, and a menu anchored to it opens
  // under the last thread instead of under the header the user clicked.
  let headerEl: HTMLDivElement | undefined = $state(undefined);
  let contextMenuOpen = $state(false);
  let lastNewThreadContextMenuAt = 0;

  // Inline rename state — mirrors the pattern in ThreadRow.
  let renaming = $state(false);
  let renameValue = $state('');
  let renameInputEl: HTMLInputElement | undefined = $state(undefined);
  // The project the rename started on, so a representative change under an
  // open input cannot move where it lands.
  let renameTarget: ProjectTarget | null = $state(null);
  let renameSaving = $state(false);

  let expanded = $derived(entry.expanded);

  // Tree + rollup are only meaningful when the project is collapsed —
  // when expanded, the nested ProjectThreadList computes its own. We
  // skip the build entirely when expanded so the hot path (status
  // streaming → re-derive across every project) doesn't pay a per-tick
  // cost for output we'd throw away.
  let rollup = $derived.by(() => {
    if (expanded) return null;
    const tree = buildSidebarThreadTree({
      threads,
      statusOf: (thread) => getEffectiveThreadStatus(thread),
      waitingOnOf: getThreadWaitingOn,
      unreachableMachineOf: (thread) => unreachableThreadMachineName(thread.id, thread.projectId),
    });
    return rollupDisplayStatus(tree);
  });

  // When collapsed, find the active-thread row (if any) so we render
  // it inline beneath the project row. We do a flat `find` rather than
  // walking the tree because we render the row at indent=1 regardless
  // of whether it sits in a group — keeps the inline pin from looking
  // like a "floating indented row".
  let activeWhenCollapsed = $derived.by<Thread | null>(() => {
    if (expanded) return null;
    const activeId = pane?.threadId;
    if (!activeId) return null;
    return threads.find((t) => t.id === activeId) ?? null;
  });

  function toggle(): void {
    onSetExpanded?.(entry.key, !entry.expanded);
  }

  function handleToggle(e: MouseEvent): void {
    e.stopPropagation();
    toggle();
  }

  function handleRowClick(): void {
    // Clicking the row body toggles expansion too — it's a bigger hit area
    // than the chevron and matches the t3-code feel.
    toggle();
  }

  function handleRowKeydown(e: KeyboardEvent): void {
    if (isLocalEnter(e) || e.key === ' ') {
      e.preventDefault();
      toggle();
    }
  }

  function handleNewThreadClick(e: MouseEvent): void {
    e.stopPropagation();
    if (Date.now() - lastNewThreadContextMenuAt < 500) return;
    onNewThread?.(entry.id, {
      openInNewPane: shouldOpenProjectThreadInNewPane(e),
    });
  }

  function handleNewThreadContextMenu(e: MouseEvent): void {
    if (!shouldOpenProjectThreadInNewPane(e)) return;
    e.preventDefault();
    e.stopPropagation();
    lastNewThreadContextMenuAt = Date.now();
    onNewThread?.(entry.id, { openInNewPane: true });
  }

  let recheckingIdentity = $state(false);

  // Rechecks each checkout that failed on its own computer, then shows any
  // reason that remains as a toast, so a touch screen without hover can read
  // it too.
  async function recheckIdentity(e: MouseEvent): Promise<void> {
    e.stopPropagation();
    if (recheckingIdentity) return;
    recheckingIdentity = true;
    try {
      // Captured: the members this row speaks for now, not whatever it
      // speaks for when the rechecks answer.
      const failing = entry.warnings.map((warning) => warning.id);
      const results = await Promise.allSettled(failing.map((id) => RefreshProjectIdentity(id)));
      for (const result of results) {
        if (result.status === 'fulfilled') updateProjectLocal(result.value);
        else addErrorToast(userFacingError(result.reason), result.reason);
      }
      if (identityProblem) addToast('warning', identityProblem);
    } finally {
      recheckingIdentity = false;
    }
  }

  function handleNewGroupClick(e: MouseEvent): void {
    e.stopPropagation();
    void newThreadGroupInProject(entry.id);
  }

  // The menu and its confirmations act on the project the gesture named.
  // The snapshot outlives the menu's close, because its confirm dialogs and
  // its deletion preview run after it.
  let menuTarget: ProjectTarget | null = $state(null);
  function openContextMenu(): void {
    menuTarget = projectTarget(entry, formatPathLabel(displayLabel));
    contextMenuOpen = true;
  }

  function handleContextMenu(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    if (headerEl) openContextMenu();
  }

  function closeContextMenu(): void {
    contextMenuOpen = false;
  }

  function beginRename(): void {
    renaming = true;
    renameTarget = projectTarget(entry, formatPathLabel(displayLabel));
    renameValue = entry.name;
    requestAnimationFrame(() => {
      renameInputEl?.focus();
      renameInputEl?.select();
    });
  }

  async function commitRename(): Promise<void> {
    if (renameSaving || renameTarget === null) return;
    const target = renameTarget;
    const next = renameValue.trim();
    if (!next || next === target.name) {
      renaming = false;
      return;
    }
    renameSaving = true;
    try {
      const updated = await RenameProject(target.id, next);
      updateProjectLocal(updated);
    } catch (err) {
      console.error('Failed to rename project:', err);
      addErrorToast(userFacingError(err), err);
    } finally {
      renameSaving = false;
      renaming = false;
    }
  }

  function cancelRename(): void {
    renaming = false;
  }

  function handleRenameKeydown(e: KeyboardEvent): void {
    // Enter confirms the IME candidate while composing a CJK name; committing
    // the rename here would save the pre-composition text and exit edit mode.
    if (isLocalEnter(e) && isImeComposingEvent(e)) return;
    if (isLocalEnter(e)) {
      e.preventDefault();
      void commitRename();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelRename();
    }
  }

  // Manual-mode DnD — the whole project row is the drag activator
  // (cursor-grab on the whole row, no separate grip
  // icon). Click still toggles expand because click fires only when no
  // drag completed; HTML5 suppresses click after a successful drag.
  let label = $derived(displayLabel);

  // A checkout git could not read keeps its last identity and may not merge
  // with its other checkouts; say why on the entry rather than nowhere.
  let identityProblem = $derived(entry.warnings
    .map((warning) => `Could not fully verify ${warning.path}: ${warning.reason}`)
    .join('\n'));

  let manualMode = $derived(getProjectSortMode() === 'manual');
  let isDragging = $derived(getDraggingProjectId() === entry.id);
  let dropMarker = $derived.by<'before' | 'after' | null>(() => {
    if (getDropTargetProjectId() !== entry.id) return null;
    return getDropPosition();
  });

  function handleDragStart(e: DragEvent): void {
    if (!manualMode) {
      e.preventDefault();
      return;
    }
    beginProjectDrag(entry.id, e);
  }

  function handleDragOver(e: DragEvent): void {
    if (!manualMode || !rowEl) return;
    if (getDraggingProjectId() === null) return;
    updateDropTarget(entry.id, e, rowEl);
  }

  function handleDrop(e: DragEvent): void {
    if (!manualMode) return;
    e.preventDefault();
    if (!orderedIds || !onReorder) {
      endProjectDrag();
      return;
    }
    const next = computeReorderedIds(orderedIds);
    if (next) onReorder(next);
    endProjectDrag();
  }

  function handleDragEnd(): void {
    endProjectDrag();
  }
</script>

<div
  bind:this={rowEl}
  role="group"
  aria-label={`Project: ${entry.name}`}
  data-testid="project-item"
  data-project-id={entry.id}
  oncontextmenu={handleContextMenu}
  ondragover={handleDragOver}
  ondrop={handleDrop}
  ondragend={handleDragEnd}
  class={
    'group relative flex flex-col transition-opacity ' +
    (isDragging ? 'opacity-40' : '')
  }
>
  {#if separatedFromPrevious}
    <!--
      Project boundary. The rails and the row gaps alone do not tell the
      eye where one project's threads stop and the next header starts, so
      each project after the first opens with an inset hairline. The drop
      indicator above it marks the same boundary during a reorder.
    -->
    <div
      role="separator"
      aria-hidden="true"
      data-testid="project-item-divider"
      class="mx-2 my-1.5 border-t border-border-subtle"
    ></div>
  {/if}
  {#if dropMarker === 'before'}
    <div
      aria-hidden="true"
      data-testid="project-item-drop-indicator"
      data-position="before"
      class="absolute -top-px left-2 right-2 h-0.5 bg-accent rounded-full pointer-events-none"
    ></div>
  {/if}
  <div
    bind:this={headerEl}
    role="button"
    tabindex={0}
    aria-expanded={expanded}
    draggable={manualMode && !compact}
    ondragstart={handleDragStart}
    onclick={handleRowClick}
    onkeydown={handleRowKeydown}
    class={
      'flex items-center gap-1.5 px-2 py-1 compact:py-2 compact:select-none rounded-[var(--radius-field)] text-fg hover:bg-surface-2/40 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 ' +
      (manualMode ? 'cursor-grab active:cursor-grabbing' : 'cursor-pointer')
    }
  >
    <!--
      Chevron + status-rollup dot share one 16px slot. When collapsed
      and any thread is non-idle, the rollup dot is visible by default
      and the chevron fades in on row hover. When expanded, the chevron
      is always visible (and rotated). This avoids layout shift across
      the swap.
    -->
    <div class="relative flex h-4 w-4 items-center justify-center shrink-0">
      <button
        type="button"
        onclick={handleToggle}
        aria-label={expanded ? 'Collapse Project' : 'Expand Project'}
        aria-expanded={expanded}
        data-testid="project-item-chevron"
        class={
          'absolute inset-0 flex items-center justify-center rounded text-fg-subtle hover:text-fg cursor-pointer transition-opacity focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40 ' +
          (rollup && !expanded ? 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100' : 'opacity-100')
        }
      >
        <Icon
          icon={ChevronRight}
          size={11}
          strokeWidth={2.5}
          class={'opacity-80 transition-transform ' + (expanded ? 'rotate-90' : '')}
        />
      </button>
      {#if rollup && !expanded}
        <span
          class="pointer-events-none absolute inset-0 flex items-center justify-center transition-opacity opacity-100 group-hover:opacity-0"
          aria-hidden="true"
          data-testid="project-item-status-dot"
          data-status={rollup.liveStatus}
        >
          <span
            class="w-2 h-2 rounded-full {rollup.pill.dotClass} {rollup.pill.pulse ? 'animate-pulse' : ''}"
          ></span>
        </span>
      {/if}
    </div>
    <Icon icon={FolderOpen} size={13} strokeWidth={2} class="shrink-0 text-accent/80 opacity-100" />
    {#if renaming}
      <!-- svelte-ignore a11y_autofocus -->
      <input
        bind:this={renameInputEl}
        bind:value={renameValue}
        onkeydown={handleRenameKeydown}
        onblur={commitRename}
        onclick={(e) => e.stopPropagation()}
        disabled={renameSaving}
        aria-label="Rename Project"
        class="text-[0.78125rem] compact:text-base flex-1 min-w-0 bg-surface-0 border border-accent/50 rounded-[var(--radius-field)] px-1 py-0.5 text-fg focus:outline-none"
      />
    {:else}
      {#if label.prefix}
        <!--
          Name conflict: show the disambiguating parent dirs, dim and
          smaller, before the name. flex-wrap means a too-long pair
          breaks into prefix-above / name-below instead of ellipsizing —
          the prefix exists to be read, so it must never truncate.
        -->
        <span
          class="flex-1 min-w-0 flex flex-wrap items-baseline gap-x-1"
          title={entry.path}
          data-testid="project-item-label"
        >
          <span class="text-[0.6875rem] text-fg-subtle min-w-0 break-all leading-tight">
            {label.prefix}{label.prefix.endsWith('/') ? '' : '/'}
          </span>
          <span class="text-[0.78125rem] font-medium text-fg min-w-0 break-words leading-tight">
            {label.name}
          </span>
        </span>
      {:else}
        <span
          class="text-[0.78125rem] font-medium truncate flex-1 text-fg"
          title={entry.path}
          data-testid="project-item-label"
        >
          {entry.name}
        </span>
      {/if}
      {#if identityProblem}
        <button
          type="button"
          onclick={recheckIdentity}
          disabled={recheckingIdentity}
          aria-busy={recheckingIdentity}
          title={identityProblem}
          aria-label={identityProblem}
          data-testid="project-item-identity-error"
          class="compact:h-9 compact:w-9 shrink-0 flex h-5 w-5 items-center justify-center rounded text-warning hover:bg-surface-2/40 cursor-pointer disabled:cursor-progress disabled:opacity-60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        >
          <Icon icon={TriangleAlert} size={12} strokeWidth={2} />
        </button>
      {/if}
      <button
        type="button"
        onclick={handleNewGroupClick}
        disabled={newThreadUngranted}
        title={newThreadUngranted ? 'Not granted to this device' : 'New Group in This Project'}
        aria-label="New Group in This Project"
        data-testid="project-item-new-group"
        class="compact:hidden opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity shrink-0 flex h-5 w-5 items-center justify-center rounded text-fg-subtle hover:text-fg hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      >
        <Icon icon={FolderPlus} size={12} strokeWidth={2} class="opacity-90" />
      </button>
      <button
        type="button"
        onclick={handleNewThreadClick}
        oncontextmenu={handleNewThreadContextMenu}
        disabled={newThreadUngranted}
        title={newThreadUngranted ? 'Not granted to this device' : 'New Thread in This Project'}
        aria-label="New Thread in This Project"
        data-testid="project-item-new-thread"
        class="compact:opacity-100 compact:h-9 compact:w-9 opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity shrink-0 flex h-5 w-5 items-center justify-center rounded text-fg-subtle hover:text-fg hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:text-fg-subtle disabled:hover:text-fg-subtle disabled:hover:bg-surface-2/0"
      >
        <Icon icon={Plus} size={12} strokeWidth={2} class="opacity-90" />
      </button>
      <SidebarRowMenuButton label="Project actions" testId="project-item-menu" onOpen={handleContextMenu} />
    {/if}
  </div>

  {#if expanded}
    <ProjectThreadList projectId={entry.id} {threads} {groups} {pane} {onNewThread} />
  {:else if activeWhenCollapsed}
    <!--
      Active-thread pin: the user is reading this thread but the project
      is collapsed. Render the row inline so they don't lose context.
      Indent=1 and the ml-4 rail match a top-level row under an expanded
      project, so expanding the project does not move the row.
    -->
    <div class="flex flex-col gap-px ml-4 border-l border-border-subtle/60" data-testid="project-item-active-pin">
      <ThreadRow thread={activeWhenCollapsed} {pane} indent={1} />
    </div>
  {/if}

  {#if dropMarker === 'after'}
    <div
      aria-hidden="true"
      data-testid="project-item-drop-indicator"
      data-position="after"
      class="absolute -bottom-px left-2 right-2 h-0.5 bg-accent rounded-full pointer-events-none"
    ></div>
  {/if}
</div>

{#if menuTarget}
  {@const target = menuTarget}
  <ProjectContextMenu
    {target}
    onNewThread={() => onNewThread?.(target.id, { openInNewPane: false })}
    onNewAssistant={() => openNewAssistantDialog({ projectId: target.id, targetPane: pane })}
    onNewTerminal={() => onNewTerminal?.(target.id)}
    anchor={headerEl}
    open={contextMenuOpen}
    onClose={closeContextMenu}
    onRename={beginRename}
  />
{/if}
