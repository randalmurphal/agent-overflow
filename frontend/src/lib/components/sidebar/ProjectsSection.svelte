<script lang="ts">
  // Section header + project list. Owns the "Add project" modal open state
  // and the sort-direction toggle. Search-driven filtering happens here so
  // the list only sees projects that match the current query (with their
  // matching threads).

  import { onDestroy } from 'svelte';
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import {
    expandProject,
    collapseProject,
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
  import { seedDefaultWorktreeIntentForDraft } from '../../stores/worktreeIntent.svelte';
  import ArrowDownUp from 'lucide-svelte/icons/arrow-down-up';
  import Plus from 'lucide-svelte/icons/plus';
  import IconButton from '../primitives/IconButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import ProjectList from './ProjectList.svelte';
  import AddProjectModal from './AddProjectModal.svelte';

  interface Props {
    pane: ThreadPane;
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
  // The projects we auto-expanded on the previous render get tracked
  // so we can collapse them again when the search clears — otherwise
  // clearing the query left every matched project expanded, which
  // isn't what the user asked for.
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
      expandProject(projectId);
      next.add(projectId);
    }
    // Collapse projects we previously auto-expanded that no longer
    // match the refined query (user typed more characters, matches
    // shrank). Leave ones the user had expanded manually before the
    // search alone — we only touch ids we own.
    for (const id of searchAutoExpanded) {
      if (!next.has(id)) collapseProject(id);
    }
    searchAutoExpanded = next;
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
      seedDefaultWorktreeIntentForDraft(thread);
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
    <IconButton
      label={`Sort ${getSortDirection() === 'asc' ? 'descending' : 'ascending'}`}
      size="sm"
      onClick={toggleSortDirection}
    >
      {#snippet children()}
        <span
          data-testid="sidebar-sort-icon"
          data-direction={getSortDirection()}
          class="flex items-center"
        >
          <Icon icon={ArrowDownUp} size={13} strokeWidth={2} class="opacity-80" />
        </span>
      {/snippet}
    </IconButton>
    <IconButton
      label="Add project"
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
    />
  </div>
</section>

<AddProjectModal
  open={addProjectOpen}
  onClose={handleAddClose}
  onDuplicate={(id) => flashProject(id)}
  onCreated={(p) => flashProject(p.id)}
/>
