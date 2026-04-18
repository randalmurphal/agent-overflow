<script lang="ts">
  // Section header + project list. Owns the "Add project" modal open state
  // and the sort-direction toggle. Search-driven filtering happens here so
  // the list only sees projects that match the current query (with their
  // matching threads).

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import {
    expandProject,
    getSortDirection,
    toggleSortDirection,
  } from '../../stores/sidebar.svelte';
  import { getThreadFilterQuery } from '../../stores/threadFilter.svelte';
  import { getThreads } from '../../stores/threads.svelte';
  import { CreateThread } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import {
    getProjectDraft,
    setProjectDraft,
  } from '../../stores/draftThreads.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import ProjectList from './ProjectList.svelte';
  import AddProjectModal from './AddProjectModal.svelte';

  interface Props {
    pane: ThreadPane;
    onStartDiscussion?: (thread: Thread) => void;
  }

  let { pane, onStartDiscussion }: Props = $props();

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
  // the current sort direction. We sort by `updatedAt` desc as the
  // default — active-first matches the t3-code reference.
  let visibleProjects = $derived.by(() => {
    const dir = getSortDirection();
    const entries = getProjects()
      .filter((p) => !p.project.archived)
      .filter((p) => {
        if (!query) return true;
        if (p.project.name.toLowerCase().includes(query)) return true;
        return (threadsByProject.get(p.project.id)?.length ?? 0) > 0;
      });
    const sorted = [...entries].sort((a, b) => {
      const nameCmp = a.project.name.localeCompare(b.project.name);
      return dir === 'asc' ? nameCmp : -nameCmp;
    });
    return sorted;
  });

  // When a search is active, auto-expand any project whose threads
  // matched so results are visible without a manual chevron click.
  $effect(() => {
    if (!query) return;
    for (const [projectId, threads] of threadsByProject.entries()) {
      if (threads.length > 0) expandProject(projectId);
    }
  });

  async function handleNewThread(projectId: string): Promise<void> {
    expandProject(projectId);
    // If a draft is already in-flight for this project (user clicked
    // "New Thread" earlier, typed something, then wandered off), reuse
    // it so the persisted composer draft repopulates under the pane.
    // This matches t3-code's "one draft per project" UX.
    const existing = getProjectDraft(projectId);
    if (existing) {
      await pane.switchThread(existing);
      return;
    }
    try {
      // CreateThread as of v13 takes a struct. Defaults come from settings.
      // We persist the row so the thread has a stable id for draft
      // saves, but we deliberately do NOT prepend it to the sidebar or
      // spawn a provider session — the thread stays a draft until the
      // first SendMessage promotes it (lazy session start in
      // app_send.go; sidebar prepend in Composer.send()).
      const thread = (await CreateThread({ projectId })) as Thread;
      setProjectDraft(projectId, thread);
      await pane.switchThread(thread);
    } catch (err) {
      console.error('Failed to create thread:', err);
      const message = err instanceof Error ? err.message : String(err);
      addToast('error', `Create thread failed: ${message}`);
    }
  }

  function handleAddClick(): void {
    addProjectOpen = true;
  }

  function handleAddClose(): void {
    addProjectOpen = false;
  }

  function flashProject(id: string): void {
    expandProject(id);
    flashProjectId = id;
    // 1.2s feels substantial but not distracting.
    setTimeout(() => {
      if (flashProjectId === id) flashProjectId = null;
    }, 1200);
  }
</script>

<section
  class="flex flex-col min-h-0 flex-1"
  aria-label="Projects"
  data-testid="sidebar-projects-section"
>
  <header class="flex items-center gap-1 px-3 py-2">
    <h2
      class="flex-1 text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70 select-none"
    >
      Projects
    </h2>
    <IconButton
      label={`Sort ${getSortDirection() === 'asc' ? 'descending' : 'ascending'}`}
      size="sm"
      onClick={toggleSortDirection}
    >
      {#snippet children()}
        <svg
          class="h-3.5 w-3.5"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          aria-hidden="true"
          data-testid="sidebar-sort-icon"
          data-direction={getSortDirection()}
        >
          <path d="m3 16 4 4 4-4" />
          <path d="M7 20V4" />
          <path d="m21 8-4-4-4 4" />
          <path d="M17 4v16" />
        </svg>
      {/snippet}
    </IconButton>
    <IconButton
      label="Add project"
      size="sm"
      onClick={handleAddClick}
    >
      {#snippet children()}
        <svg
          class="h-3.5 w-3.5"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2.2"
          stroke-linecap="round"
          stroke-linejoin="round"
          aria-hidden="true"
          data-testid="sidebar-add-project-icon"
        >
          <path d="M12 5v14" />
          <path d="M5 12h14" />
        </svg>
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
      {onStartDiscussion}
      onNewThread={handleNewThread}
    />
  </div>
</section>

<AddProjectModal
  open={addProjectOpen}
  onClose={handleAddClose}
  onDuplicate={(id) => flashProject(id)}
  onCreated={(p) => flashProject(p.id)}
/>
