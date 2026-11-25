<script lang="ts">
  import { isLocalEnter } from '../../utils/localEnter';
  import { projectScopeGrant } from '../../transport/entityScopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
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
  import { rollupProjectThreads, sameThreadStatusPill } from '../../utils/sidebarTreeView';
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
  import { createProjectActions } from './projectActions.svelte';

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
     *  section writes it; this row only reports the gesture. */
    onSetExpanded?: (entry: SidebarEntry, expanded: boolean) => void;
    /** A search shows this row open whatever its fold. */
    searchOpened?: boolean;
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
    searchOpened = false,
    separatedFromPrevious = false,
  }: Props = $props();

  // Creating a thread rides `threads:operate`. The control stays in the
  // row and goes inert rather than disappearing — a project whose row lost
  // an affordance reads as a broken sidebar, not as a read-only one.
  // The hover-reveal is untouched, so nothing new is visible at rest.
  let newThreadGrant = $derived(projectScopeGrant('threads:operate', entry.id));
  let newThreadUngranted = $derived(newThreadGrant !== 'granted');
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

  // Every action this row starts, as one run at a time
  // (./projectActions.svelte.ts): the rename below and the menu's archive
  // and delete share it, so the newest gesture is the only one that can
  // still show a dialog or send a call.
  const actions = createProjectActions();
  let renameRun = $derived(actions.current?.kind === 'rename' ? actions.current : null);
  let renameInputEl: HTMLInputElement | undefined = $state(undefined);

  let expanded = $derived(entry.expanded || searchOpened);

  // Single-pass status rollup when the project is collapsed — avoids
  // building or sorting a full sidebar tree just to pick the top status
  // dot, and keeps object identity stable across unchanged beats.
  let prevRollup: ReturnType<typeof rollupProjectThreads> = null;
  let rollup = $derived.by(() => {
    if (expanded) {
      prevRollup = null;
      return null;
    }
    const next = rollupProjectThreads({
      threads,
      statusOf: (thread) => getEffectiveThreadStatus(thread),
      waitingOnOf: getThreadWaitingOn,
      unreachableMachineOf: (thread) => unreachableThreadMachineName(thread.id, thread.projectId),
    });
    if (
      prevRollup !== null && next !== null
      && prevRollup.liveStatus === next.liveStatus
      && sameThreadStatusPill(prevRollup.pill, next.pill)
    ) {
      return prevRollup;
    }
    prevRollup = next;
    return next;
  });

  // When collapsed, find the active-thread row (if any) so we render
  // it inline beneath the project row. Uses the pane's per-thread signal
  // when available so thread switches in other projects do not wake this
  // collapsed project.
  let activeWhenCollapsed = $derived.by<Thread | null>(() => {
    if (expanded || !pane) return null;
    if (typeof pane.isShowingThread === 'function') {
      for (const t of threads) {
        if (pane.isShowingThread(t.id)) return t;
      }
      return null;
    }
    const activeId = pane.threadId;
    if (!activeId) return null;
    return threads.find((t) => t.id === activeId) ?? null;
  });

  function toggle(): void {
    onSetExpanded?.(entry, !expanded);
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

  // On the target the menu was opened on, which is not the row's current
  // one once a listing has changed hands under the open menu.
  function beginRename(target: ProjectTarget): void {
    actions.start('rename', target);
    requestAnimationFrame(() => {
      renameInputEl?.focus();
      renameInputEl?.select();
    });
  }

  async function commitRename(): Promise<void> {
    const run = renameRun;
    if (run === null) return;
    const next = run.name.trim();
    if (!next || next === run.target.name) {
      run.end();
      return;
    }
    if (!run.beginSubmit()) return;
    try {
      const updated = await RenameProject(run.target.id, next);
      updateProjectLocal(updated);
    } catch (err) {
      console.error('Failed to rename project:', err);
      addErrorToast(userFacingError(err), err);
    } finally {
      run.end();
    }
  }

  function cancelRename(): void {
    renameRun?.end();
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
    {#if renameRun}
      {@const run = renameRun}
      <!-- svelte-ignore a11y_autofocus -->
      <input
        bind:this={renameInputEl}
        value={run.name}
        oninput={(e) => run.setName(e.currentTarget.value)}
        onkeydown={handleRenameKeydown}
        onblur={commitRename}
        onclick={(e) => e.stopPropagation()}
        disabled={run.submitting}
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
        title={grantBlockedTitle(newThreadGrant) ?? 'New Group in This Project'}
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
        title={grantBlockedTitle(newThreadGrant) ?? 'New Thread in This Project'}
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
    <ProjectThreadList projectId={entry.id} listKey={entry.key} listLimit={entry.listLimit} {threads} {groups} {pane} {onNewThread} />
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
    {actions}
    onNewThread={() => onNewThread?.(target.id, { openInNewPane: false })}
    onNewAssistant={() => openNewAssistantDialog({ projectId: target.id, targetPane: pane })}
    onNewTerminal={() => onNewTerminal?.(target.id)}
    anchor={headerEl}
    open={contextMenuOpen}
    onClose={closeContextMenu}
    onRename={beginRename}
  />
{/if}
