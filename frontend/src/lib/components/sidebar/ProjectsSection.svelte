<script lang="ts">
  // Section header + project list. Owns the "Add project" modal open state
  // and the sort-direction toggle. Search-driven filtering happens here so
  // the list only sees projects that match the current query (with their
  // matching threads).

  import { onDestroy } from 'svelte';
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    getProjects,
    refreshProjects,
    updateProjectLocal,
  } from '../../stores/projects.svelte';
  import {
    expandProject,
    collapseProject,
    isProjectExpanded,
    getProjectSortMode,
  } from '../../stores/sidebar.svelte';
  import { getThreadFilterQuery } from '../../stores/threadFilter.svelte';
  import { getThreads } from '../../stores/threads.svelte';
  import { UpdateProjectSortPositions } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { openDraftThreadForProject } from '../../stores/threadCreation.svelte';
  import Plus from 'lucide-svelte/icons/plus';
  import IconButton from '../primitives/IconButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import ProjectList from './ProjectList.svelte';
  import type { ProjectNewThreadHandler } from './projectNewThread';
  import ProjectSortMenu from './ProjectSortMenu.svelte';
  import AddProjectModal from './AddProjectModal.svelte';

  interface Props {
    pane: ThreadPane | null;
  }

  let { pane }: Props = $props();

  let addProjectOpen = $state(false);
  let flashProjectId: string | null = $state(null);

  // Normalise the search query once per derivation so every filter path
  // uses the same lowercase form.
  let query = $derived(getThreadFilterQuery().trim().toLowerCase());

  // Threads grouped by project id (filtered by search when active).
  let threadsByProject = $derived.by(() => {
    const out = new Map<string, Thread[]>();
    const all = getThreads();
    for (const t of all) {
      if (t.archived) continue;
      // Thread.projectId arrives in Wave 1/2; fall back to empty string
      // so unmigrated fixtures in tests don't crash the list.
      const key = (t as { projectId?: string }).projectId ?? '';
      if (!key) continue;
      if (query) {
        const hay = `${t.title ?? ''} ${t.workspacePath ?? ''}`.toLowerCase();
        if (!hay.includes(query)) continue;
      }
      const bucket = out.get(key);
      if (bucket) bucket.push(t);
      else out.set(key, [t]);
    }
    return out;
  });

  // Visible projects: respect search (name match OR thread match) and
  // the current sort mode. Three modes mirror forge / t3-code:
  //   - lastActivity: most-recently-touched thread first (sidebar default).
  //   - createdAt: project creation time, newest first.
  //   - manual: user-defined via DnD; persisted in Project.sortPosition.
  let visibleProjects = $derived.by(() => {
    const mode = getProjectSortMode();
    const entries = getProjects()
      .filter((p) => !p.project.archived)
      .filter((p) => {
        if (!query) return true;
        if (p.project.name.toLowerCase().includes(query)) return true;
        return (threadsByProject.get(p.project.id)?.length ?? 0) > 0;
      });
    return [...entries].sort((a, b) => {
      switch (mode) {
        case 'manual': {
          const cmp = a.project.sortPosition - b.project.sortPosition;
          if (cmp !== 0) return cmp;
          return a.project.createdAt - b.project.createdAt;
        }
        case 'createdAt':
          return b.project.createdAt - a.project.createdAt;
        case 'lastActivity':
        default: {
          const aActive = a.lastActive ?? 0;
          const bActive = b.lastActive ?? 0;
          // Fall back to project.updatedAt when no thread has touched
          // this project yet — the sidebar still surfaces a freshly
          // renamed / added project even with zero threads.
          const aRank = aActive > 0 ? aActive : a.project.updatedAt;
          const bRank = bActive > 0 ? bActive : b.project.updatedAt;
          if (aRank !== bRank) return bRank - aRank;
          return a.project.name.localeCompare(b.project.name);
        }
      }
    });
  });

  // When a search is active, auto-expand any project whose threads
  // matched so results are visible without a manual chevron click.
  // The projects we auto-expanded on the previous render get tracked
  // so we can collapse them again when the search clears — otherwise
  // clearing the query left every matched project expanded, which
  // isn't what the user asked for.
  //
  // Only track ids we actually flipped from collapsed → expanded.
  // Projects that were already expanded (the default, or the user
  // expanded them manually) stay untouched — we don't own their state,
  // so we mustn't re-collapse them on search clear.
  //
  // Plain `let`, NOT $state — the $effect below reads AND writes this
  // set. If it were reactive, every assignment would re-trigger the
  // effect on the same `query` value and loop infinitely. The set
  // exists only to remember which auto-expansions to roll back when the
  // query clears; nothing else reads it.
  let searchAutoExpanded = new Set<string>();
  $effect(() => {
    if (!query) {
      // Search cleared — undo anything we expanded for this session.
      for (const id of searchAutoExpanded) collapseProject(id);
      searchAutoExpanded = new Set<string>();
      return;
    }
    const next = new Set<string>();
    for (const [projectId, threads] of threadsByProject.entries()) {
      if (threads.length === 0) continue;
      if (searchAutoExpanded.has(projectId)) {
        // Already auto-expanded earlier in this search session — keep
        // tracking so we still own the rollback.
        next.add(projectId);
      } else if (!isProjectExpanded(projectId)) {
        // Was collapsed before search — flip and remember.
        expandProject(projectId);
        next.add(projectId);
      }
      // else: user/default-expanded — leave alone, don't track.
    }
    // Collapse projects we previously auto-expanded that no longer
    // match the refined query (user typed more characters, matches
    // shrank).
    for (const id of searchAutoExpanded) {
      if (!next.has(id)) collapseProject(id);
    }
    searchAutoExpanded = next;
  });

  const handleNewThread: ProjectNewThreadHandler = async (projectId, options = {}) => {
    // Sidebar "+" always opens a chat draft. Design drafts are created
    // from the in-pane ThreadModePicker (or the "Thread: New Design"
    // palette commands) — keeping the sidebar single-purpose avoids
    // doubling the button count for an entry point most users won't
    // reach for.
    try {
      await openDraftThreadForProject({
        projectId,
        mode: 'chat',
        targetPane: pane,
        openInNewPane: options.openInNewPane ?? false,
      });
    } catch (err) {
      console.error('Failed to create draft thread:', err);
      addToast('error', userFacingError(err));
    }
  };

  /**
   * Commit a manual reorder. Updates each project's sortPosition
   * locally so the next derive() reorders without a refresh-flicker,
   * then persists to the backend. On error we re-fetch to recover.
   */
  async function handleReorder(newOrderedIds: string[]): Promise<void> {
    for (const [index, id] of newOrderedIds.entries()) {
      const existing = getProjects().find((p) => p.project.id === id);
      if (!existing) continue;
      updateProjectLocal({ ...existing.project, sortPosition: index });
    }
    try {
      await UpdateProjectSortPositions(newOrderedIds);
    } catch (err) {
      console.error('Failed to persist project order:', err);
      addToast('error', userFacingError(err));
      await refreshProjects();
    }
  }

  function handleAddClick(): void {
    addProjectOpen = true;
  }

  function handleAddClose(): void {
    addProjectOpen = false;
  }

  // Flash-clear timer — tracked so a quick unmount (or a back-to-back
  // flash) doesn't leave a stale timer firing against a disposed
  // component and writing to $state in an orphaned reactivity scope.
  let flashTimer: ReturnType<typeof setTimeout> | null = null;
  function flashProject(id: string): void {
    expandProject(id);
    flashProjectId = id;
    if (flashTimer) clearTimeout(flashTimer);
    // 1.2s feels substantial but not distracting.
    flashTimer = setTimeout(() => {
      if (flashProjectId === id) flashProjectId = null;
      flashTimer = null;
    }, 1200);
  }

  onDestroy(() => {
    if (flashTimer) clearTimeout(flashTimer);
  });
</script>

<section
  class="flex flex-col min-h-0 flex-1"
  aria-label="Projects"
  data-testid="sidebar-projects-section"
>
  <header class="flex items-center gap-1 px-3 pt-2 pb-1.5">
    <MicroLabel as="h2" class="flex-1 select-none">Projects</MicroLabel>
    <ProjectSortMenu />
    <IconButton
      label="Add Project"
      size="sm"
      onClick={handleAddClick}
    >
      {#snippet children()}
        <span data-testid="sidebar-add-project-icon" class="flex items-center">
          <Icon icon={Plus} size={14} strokeWidth={2.2} class="opacity-80" />
        </span>
      {/snippet}
    </IconButton>
  </header>

  <div
    class="flex-1 min-h-0 flex flex-col"
    data-flashing-project={flashProjectId}
  >
    <ProjectList
      projects={visibleProjects}
      {threadsByProject}
      {pane}
      onNewThread={handleNewThread}
      onReorder={handleReorder}
    />
  </div>
</section>

<AddProjectModal
  open={addProjectOpen}
  onClose={handleAddClose}
  onDuplicate={(id) => flashProject(id)}
  onCreated={(p) => flashProject(p.id)}
/>
