<script lang="ts">
  // A single project row. Chevron + folder + name + new-thread action on the
  // right for "new thread here". Right-click opens a small context menu
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

  import type { ProjectWithCounts, Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { RenameProject } from '../../stores/bindings';
  import { getProjectLabel, updateProjectLocal } from '../../stores/projects.svelte';
  import {
    getProjectSortMode,
    isProjectExpanded,
    toggleProject,
  } from '../../stores/sidebar.svelte';
  import {
    getEffectiveThreadStatus,
  } from '../../stores/threadStatuses.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import FolderOpen from '@lucide/svelte/icons/folder-open';
  import Plus from '@lucide/svelte/icons/plus';
  import Terminal from '@lucide/svelte/icons/terminal';
  import Icon from '../primitives/Icon.svelte';
  import ProjectContextMenu from './ProjectContextMenu.svelte';
  import ProjectThreadList from './ProjectThreadList.svelte';
  import ThreadRow from './ThreadRow.svelte';
  import { buildSidebarThreadTree, rollupDisplayStatus } from '../../utils/sidebarTree';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import {
    shouldOpenProjectThreadInNewPane,
    type ProjectNewThreadHandler,
    type ProjectNewTerminalHandler,
  } from './projectNewThread';
  import {
    beginProjectDrag,
    computeReorderedIds,
    endProjectDrag,
    getDraggingProjectId,
    getDropPosition,
    getDropTargetProjectId,
    updateDropTarget,
  } from '../../stores/projectDnd.svelte';
  import { hasScope } from '../../transport/scopes';

  interface Props {
    project: ProjectWithCounts;
    threads: Thread[];
    pane: ThreadPane | null;
    /** Called with the project id when the user clicks the new-thread button
     * (or otherwise signals "create a new thread in this project"). */
    onNewThread?: ProjectNewThreadHandler;
    /** Called with the project id when the user clicks the new-terminal
     *  button — opens a fresh terminal pane rooted at this project. */
    onNewTerminal?: ProjectNewTerminalHandler;
    /** Current rendered ordering of project ids (visible projects in
     *  ProjectsSection). Required for DnD to compute the new order on
     *  drop without an extra round-trip. */
    orderedIds?: readonly string[];
    /** Commit a new ordering. Caller updates store + persists. */
    onReorder?: (newOrderedIds: string[]) => void;
    /** Adds the project-to-project rhythm used after the first item. */
    separatedFromPrevious?: boolean;
  }

  let {
    project,
    threads,
    pane,
    onNewThread,
    onNewTerminal,
    orderedIds,
    onReorder,
    separatedFromPrevious = false,
  }: Props = $props();

  // Creating a thread rides `threads:operate`; a terminal thread also
  // starts a PTY, which rides `terminal:operate`. Both controls stay in the
  // row and go inert rather than disappearing — a project whose row lost
  // half its affordances reads as a broken sidebar, not as a read-only one.
  // The hover-reveal is untouched, so nothing new is visible at rest.
  let newThreadUngranted = $derived(!hasScope('threads:operate'));
  let newTerminalUngranted = $derived(!hasScope('terminal:operate'));

  let rowEl: HTMLDivElement | undefined = $state(undefined);
  let contextMenuOpen = $state(false);
  let contextMenuAnchor: HTMLElement | undefined = $state(undefined);
  let lastNewThreadContextMenuAt = 0;

  // Inline rename state — mirrors the pattern in ThreadRow.
  let renaming = $state(false);
  let renameValue = $state('');
  let renameInputEl: HTMLInputElement | undefined = $state(undefined);
  let renameSaving = $state(false);

  let expanded = $derived(isProjectExpanded(project.project.id));

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
    });
    return rollupDisplayStatus(tree);
  });

  // When collapsed, find the active-thread row (if any) so we render
  // it inline beneath the project row. We do a flat `find` rather than
  // walking the tree because we render the row at indent=1 regardless
  // of where it sits in the discussion hierarchy — keeps the inline
  // pin from looking like a "floating indented row".
  let activeWhenCollapsed = $derived.by<Thread | null>(() => {
    if (expanded) return null;
    const activeId = pane?.threadId;
    if (!activeId) return null;
    return threads.find((t) => t.id === activeId) ?? null;
  });

  function handleToggle(e: MouseEvent): void {
    e.stopPropagation();
    toggleProject(project.project.id);
  }

  function handleRowClick(): void {
    // Clicking the row body toggles expansion too — it's a bigger hit area
    // than the chevron and matches the t3-code feel.
    toggleProject(project.project.id);
  }

  function handleRowKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggleProject(project.project.id);
    }
  }

  function handleNewThreadClick(e: MouseEvent): void {
    e.stopPropagation();
    if (Date.now() - lastNewThreadContextMenuAt < 500) return;
    onNewThread?.(project.project.id, {
      openInNewPane: shouldOpenProjectThreadInNewPane(e),
    });
  }

  function handleNewThreadContextMenu(e: MouseEvent): void {
    if (!shouldOpenProjectThreadInNewPane(e)) return;
    e.preventDefault();
    e.stopPropagation();
    lastNewThreadContextMenuAt = Date.now();
    onNewThread?.(project.project.id, { openInNewPane: true });
  }

  function handleNewTerminalClick(e: MouseEvent): void {
    e.stopPropagation();
    onNewTerminal?.(project.project.id);
  }

  function handleContextMenu(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    if (rowEl) {
      contextMenuAnchor = rowEl;
      contextMenuOpen = true;
    }
  }

  function closeContextMenu(): void {
    contextMenuOpen = false;
  }

  function beginRename(): void {
    renaming = true;
    renameValue = project.project.name;
    requestAnimationFrame(() => {
      renameInputEl?.focus();
      renameInputEl?.select();
    });
  }

  async function commitRename(): Promise<void> {
    if (renameSaving) return;
    const next = renameValue.trim();
    if (!next || next === project.project.name) {
      renaming = false;
      return;
    }
    renameSaving = true;
    try {
      const updated = await RenameProject(project.project.id, next);
      updateProjectLocal(updated);
    } catch (err) {
      console.error('Failed to rename project:', err);
      addToast('error', userFacingError(err));
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
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter') {
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
  // Duplicate names are legal (paths are the unique key), so the label
  // comes from the store's disambiguation map: unique names render bare,
  // duplicates gain a dim parent-dir prefix.
  let label = $derived(
    getProjectLabel(project.project.id) ?? { prefix: '', name: project.project.name },
  );

  let manualMode = $derived(getProjectSortMode() === 'manual');
  let isDragging = $derived(getDraggingProjectId() === project.project.id);
  let dropMarker = $derived.by<'before' | 'after' | null>(() => {
    if (getDropTargetProjectId() !== project.project.id) return null;
    return getDropPosition();
  });

  function handleDragStart(e: DragEvent): void {
    if (!manualMode) {
      e.preventDefault();
      return;
    }
    beginProjectDrag(project.project.id, e);
  }

  function handleDragOver(e: DragEvent): void {
    if (!manualMode || !rowEl) return;
    if (getDraggingProjectId() === null) return;
    updateDropTarget(project.project.id, e, rowEl);
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
  aria-label={`Project: ${project.project.name}`}
  data-testid="project-item"
  data-project-id={project.project.id}
  oncontextmenu={handleContextMenu}
  ondragover={handleDragOver}
  ondrop={handleDrop}
  ondragend={handleDragEnd}
  class={
    'group relative flex flex-col transition-opacity ' +
    (separatedFromPrevious ? 'mt-[3px] ' : '') +
    (isDragging ? 'opacity-40' : '')
  }
>
  {#if dropMarker === 'before'}
    <div
      aria-hidden="true"
      data-testid="project-item-drop-indicator"
      data-position="before"
      class="absolute -top-px left-2 right-2 h-0.5 bg-accent rounded-full pointer-events-none"
    ></div>
  {/if}
  <div
    role="button"
    tabindex={0}
    aria-expanded={expanded}
    draggable={manualMode}
    ondragstart={handleDragStart}
    onclick={handleRowClick}
    onkeydown={handleRowKeydown}
    class={
      'flex items-center gap-1.5 px-2 py-1 rounded-[var(--radius-field)] text-fg hover:bg-surface-2/40 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 ' +
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
        class="text-[0.78125rem] flex-1 min-w-0 bg-surface-0 border border-accent/50 rounded-[var(--radius-field)] px-1 py-0.5 text-fg focus:outline-none"
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
          title={project.project.path}
          data-testid="project-item-label"
        >
          <span class="text-[0.6875rem] text-fg-subtle min-w-0 break-all leading-tight">
            {label.prefix}/
          </span>
          <span class="text-[0.78125rem] font-medium text-fg min-w-0 break-words leading-tight">
            {label.name}
          </span>
        </span>
      {:else}
        <span
          class="text-[0.78125rem] font-medium truncate flex-1 text-fg"
          title={project.project.path}
          data-testid="project-item-label"
        >
          {project.project.name}
        </span>
      {/if}
      <button
        type="button"
        onclick={handleNewTerminalClick}
        disabled={newTerminalUngranted}
        title={newTerminalUngranted ? 'Local only' : 'New Terminal in This Project'}
        aria-label="New Terminal in This Project"
        data-testid="project-item-new-terminal"
        class="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity ml-1 shrink-0 flex h-5 w-5 items-center justify-center rounded text-fg-subtle hover:text-fg hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:text-fg-subtle disabled:hover:text-fg-subtle disabled:hover:bg-surface-2/0"
      >
        <Icon icon={Terminal} size={12} strokeWidth={2} class="opacity-90" />
      </button>
      <button
        type="button"
        onclick={handleNewThreadClick}
        oncontextmenu={handleNewThreadContextMenu}
        disabled={newThreadUngranted}
        title={newThreadUngranted ? 'Local only' : 'New Thread in This Project'}
        aria-label="New Thread in This Project"
        data-testid="project-item-new-thread"
        class="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity ml-1 shrink-0 flex h-5 w-5 items-center justify-center rounded text-fg-subtle hover:text-fg hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:text-fg-subtle disabled:hover:text-fg-subtle disabled:hover:bg-surface-2/0"
      >
        <Icon icon={Plus} size={12} strokeWidth={2} class="opacity-90" />
      </button>
    {/if}
  </div>

  {#if expanded}
    <ProjectThreadList projectId={project.project.id} {threads} {pane} {onNewThread} />
  {:else if activeWhenCollapsed}
    <!--
      Active-thread pin: the user is reading this thread but the project
      is collapsed. Render the row inline so they don't lose context.
      Indent=1 matches a top-level row under an expanded project.
    -->
    <div class="flex flex-col gap-px ml-2 pl-2 border-l border-border-subtle/60" data-testid="project-item-active-pin">
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

<ProjectContextMenu
  {project}
  anchor={contextMenuAnchor}
  open={contextMenuOpen}
  onClose={closeContextMenu}
  onRename={beginRename}
/>
