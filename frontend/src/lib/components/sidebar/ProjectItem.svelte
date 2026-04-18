<script lang="ts">
  // A single project row. Chevron + folder + name + hover-pencil on the
  // right for "new thread here". Right-click opens a small context menu
  // (Rename / Archive / Delete) rendered via ProjectContextMenu.
  //
  // Keeping the rename flow inline (not in the context menu component)
  // lets the row render the input in-place of the project name without
  // piping state across a component boundary.

  import type { ProjectWithCounts, Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { RenameProject } from '../../stores/bindings';
  import { updateProjectLocal } from '../../stores/projects.svelte';
  import {
    isProjectExpanded,
    toggleProject,
  } from '../../stores/sidebar.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import ProjectContextMenu from './ProjectContextMenu.svelte';
  import ProjectThreadList from './ProjectThreadList.svelte';

  interface Props {
    project: ProjectWithCounts;
    threads: Thread[];
    pane: ThreadPane;
    onStartDiscussion?: (thread: Thread) => void;
    /** Called with the project id when the user clicks the hover pencil
     * (or otherwise signals "create a new thread in this project"). */
    onNewThread?: (projectId: string) => void;
  }

  let { project, threads, pane, onStartDiscussion, onNewThread }: Props = $props();

  let rowEl: HTMLDivElement | undefined = $state(undefined);
  let contextMenuOpen = $state(false);
  let contextMenuAnchor: HTMLElement | undefined = $state(undefined);

  // Inline rename state — mirrors the pattern in ThreadRow.
  let renaming = $state(false);
  let renameValue = $state('');
  let renameInputEl: HTMLInputElement | undefined = $state(undefined);
  let renameSaving = $state(false);

  let expanded = $derived(isProjectExpanded(project.project.id));

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

  function handlePencilClick(e: MouseEvent): void {
    e.stopPropagation();
    onNewThread?.(project.project.id);
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
      addToast(
        'error',
        `Rename failed: ${err instanceof Error ? err.message : err}`,
      );
    } finally {
      renameSaving = false;
      renaming = false;
    }
  }

  function cancelRename(): void {
    renaming = false;
  }

  function handleRenameKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter') {
      e.preventDefault();
      void commitRename();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelRename();
    }
  }
</script>

<div
  bind:this={rowEl}
  role="group"
  aria-label={`Project ${project.project.name}`}
  data-testid="project-item"
  data-project-id={project.project.id}
  oncontextmenu={handleContextMenu}
  class="group relative flex flex-col"
>
  <div
    role="button"
    tabindex={0}
    aria-expanded={expanded}
    onclick={handleRowClick}
    onkeydown={handleRowKeydown}
    class="flex items-center gap-1 px-2 py-1.5 rounded-md cursor-pointer text-text-primary hover:bg-surface-2/60 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    <button
      type="button"
      onclick={handleToggle}
      aria-label={expanded ? 'Collapse project' : 'Expand project'}
      aria-expanded={expanded}
      data-testid="project-item-chevron"
      class="flex h-4 w-4 items-center justify-center shrink-0 rounded text-text-secondary/70 hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/50"
    >
      <svg
        class="h-3 w-3 transition-transform {expanded ? 'rotate-90' : ''}"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2.5"
        stroke-linecap="round"
        stroke-linejoin="round"
        aria-hidden="true"
      >
        <polyline points="9 18 15 12 9 6" />
      </svg>
    </button>
    <svg
      class="h-3.5 w-3.5 shrink-0 text-accent/80"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
    >
      <path
        d="M20 19a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h7a2 2 0 0 1 2 2z"
      />
    </svg>
    {#if renaming}
      <!-- svelte-ignore a11y_autofocus -->
      <input
        bind:this={renameInputEl}
        bind:value={renameValue}
        onkeydown={handleRenameKeydown}
        onblur={commitRename}
        onclick={(e) => e.stopPropagation()}
        disabled={renameSaving}
        aria-label="Rename project"
        class="text-sm flex-1 min-w-0 bg-surface-0 border border-accent rounded px-1 py-0.5 text-text-primary focus:outline-none"
      />
    {:else}
      <span
        class="text-sm font-medium truncate flex-1"
        title={project.project.path}
      >
        {project.project.name}
      </span>
      {#if project.threadCount > 0}
        <span
          class="ml-1 shrink-0 text-[10px] tabular-nums text-text-secondary/60"
          aria-hidden="true"
        >
          {project.threadCount}
        </span>
      {/if}
      <button
        type="button"
        onclick={handlePencilClick}
        title="New thread in this project"
        aria-label="New thread in this project"
        data-testid="project-item-new-thread"
        class="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity ml-1 shrink-0 flex h-5 w-5 items-center justify-center rounded text-text-secondary hover:text-text-primary hover:bg-surface-2/60 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        <svg
          class="h-3.5 w-3.5"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          aria-hidden="true"
        >
          <path d="M12 20h9" />
          <path d="M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4 12.5-12.5z" />
        </svg>
      </button>
    {/if}
  </div>

  {#if expanded}
    <ProjectThreadList {threads} {pane} {onStartDiscussion} />
  {/if}
</div>

<ProjectContextMenu
  {project}
  {pane}
  anchor={contextMenuAnchor}
  open={contextMenuOpen}
  onClose={closeContextMenu}
  onRename={beginRename}
/>
