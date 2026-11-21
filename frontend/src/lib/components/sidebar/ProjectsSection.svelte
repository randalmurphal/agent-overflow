<script lang="ts">
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  // The sidebar's two sections, Assistants above Projects, in one scroll
  // area: the Projects header and list here, the Assistants section
  // (AssistantsSection) fed from the same bucketing, absent while no
  // computer this window shows has assistants on, and without the rows of
  // a computer that has them off (hiddenAssistantThread). Owns the "Add project"
  // modal open state and the sort-direction toggle. Search-driven
  // filtering happens here so each section only sees what matches the
  // current query.

  import { onDestroy } from 'svelte';
  import type { ProjectWithCounts, Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    entryIdFor,
    getProjectLiveActivityAt,
    getProjects,
    projectEntryKey,
    projectOrderPosition,
    reorderProjects,
    representativeOf,
  } from '../../stores/projects.svelte';
  import {
    expandProject,
    collapseProject,
    isProjectExpanded,
    isSidebarSectionShown,
    getProjectSortMode,
    setSidebarSectionExpanded,
  } from '../../stores/sidebar.svelte';
  import { getThreadFilterQuery } from '../../stores/threadFilter.svelte';
  import { getThreads } from '../../stores/threads.svelte';
  import { getThreadGroups } from '../../stores/threadGroups.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { reuseUnchangedBuckets } from '../../utils/stableBuckets';
  import {
    openDraftThreadForProject,
    openTerminalThread,
  } from '../../stores/threadCreation.svelte';
  import { openSessionImport } from '../../stores/sessionImport.svelte';
  import { hasScope } from '../../transport/scopes';
  import History from '@lucide/svelte/icons/history';
  import Plus from '@lucide/svelte/icons/plus';
  import IconButton from '../primitives/IconButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import SidebarDeviceFilter from './SidebarDeviceFilter.svelte';
  import { sidebarBackendVisible, sidebarDeviceFilterActive, sidebarThreadVisible } from '../../stores/sidebarDevices.svelte';
  import { projectBackend, threadGroupBackend } from '../../transport/entityIndex';
  import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
  import {
    backendDisplayName,
    getAttachedBackends,
    hasMultipleBackends,
    threadMachine,
  } from '../../stores/attachedBackends.svelte';
  import { CATALOG_KINDS, catalogLoadState } from '../../stores/catalogLoad.svelte';
  import { catalogTreeListed, loadSidebarCatalogs } from '../../stores/sidebarCatalogs';
  import CatalogStatusRow from './CatalogStatusRow.svelte';
  import ProjectList from './ProjectList.svelte';
  import { bucketSidebarThreads } from './sidebarBuckets';
  import type {
    ProjectNewThreadHandler,
    ProjectNewTerminalHandler,
  } from './projectNewThread';
  import ProjectSortMenu from './ProjectSortMenu.svelte';
  import AddProjectModal from './AddProjectModal.svelte';
  import AssistantsSection from './AssistantsSection.svelte';
  import { anyAssistantsOn, hiddenAssistantThread } from '../../stores/assistants.svelte';
  import SidebarSectionToggle from './SidebarSectionToggle.svelte';

  interface Props {
    pane: ThreadPane | null;
  }

  let { pane }: Props = $props();

  let addProjectOpen = $state(false);
  let flashProjectId: string | null = $state(null);

  // Session import walks the provider homes and writes threads.
  let importUngranted = $derived(!hasScope('threads:operate', selectedBackend()));

  // Normalise the search query once per derivation so every filter path
  // uses the same lowercase form.
  let query = $derived(getThreadFilterQuery().trim().toLowerCase());

  // Threads and groups bucketed by sidebar ENTRY (a repo checked out on
  // two attached machines is one entry, and both machines' rows sit under
  // it), filtered by the search query, with the assistants' rows apart
  // (bucketSidebarThreads).
  //
  // Both maps are re-minted per derivation, and the ROWS inside them are
  // stable references. A bucket whose rows did not change keeps its array,
  // so a thread change rebuilds only its own project's tree; the identity
  // cutoffs downstream (visibleProjects here, sameSidebarVisibleRows in
  // SidebarTreeList) keep a streaming beat off the DOM. The assistants'
  // rows keep their array the same way, so a project thread's change does
  // not rebuild the Assistants section's tree.
  //
  // Every row of a computer's tree is held until the tree lists
  // (catalogTreeListed): its project headers through listedProjects below,
  // its project threads and its groups here. An assistant's row belongs to
  // no project and is not held.
  //
  // `treeListed` is the only reader of that gate in this file, so what a
  // computer shows before its first complete round is one decision. Its memo
  // is per derivation: each derivation reads the gate once per computer and
  // nothing may cache a reactive read across derivations.
  function treeListed(): (backend: BackendKey) => boolean {
    const seen = new Map<BackendKey, boolean>();
    return (backend) => {
      let value = seen.get(backend);
      if (value === undefined) seen.set(backend, value = catalogTreeListed(backend));
      return value;
    };
  }

  // One derivation of every entry the sidebar renders, and of everything a
  // header shows or the list sorts by.
  //
  // Members whose computer holds its tree are out of it entirely, and of
  // those that remain the device filter decides which ones an entry counts,
  // dates and speaks for. So a computer that answers its projects before
  // its threads changes nothing on screen, and a filter changes only what
  // the filter is for.
  //
  // The entry each member renders under, its key, and which member
  // represents it are three separate things:
  //   - `entryOf` is the id of the row rendered, so threads bucket under
  //     the header that exists;
  //   - `keys` is the entry's repository key (stores/projects.svelte.ts),
  //     stable across every representative change, so the each block never
  //     remounts a row that is still the same entry;
  //   - the representative is elected over the LISTED members, filter
  //     ignored, so the label, the action target and the manual position
  //     can change only when a computer's tree lists, and never because
  //     the filter changed.
  // Disclosure needs none of the three: it is persisted by project id and
  // read over an entry's members (stores/sidebar.svelte.ts).
  let listedProjects = $derived.by(() => {
    const listedOn = treeListed();
    const groups = new Map<string, { listed: ProjectWithCounts[]; shown: ProjectWithCounts[] }>();
    for (const row of getProjects()) {
      const backend = projectBackend(row.project.id) ?? HOME_BACKEND;
      if (!listedOn(backend)) continue;
      // The store's entry id groups the members; which one of them wears
      // the entry here is decided below.
      const token = entryIdFor(row.project.id);
      let group = groups.get(token);
      if (group === undefined) groups.set(token, group = { listed: [], shown: [] });
      group.listed.push(row);
      if (sidebarBackendVisible(backend)) group.shown.push(row);
    }
    const entries: ProjectWithCounts[] = [];
    const entryOf = new Map<string, string>();
    const members = new Map<string, ProjectWithCounts[]>();
    const keys = new Map<string, string>();
    for (const group of groups.values()) {
      if (group.shown.length === 0) continue;
      const rep = representativeOf(group.listed);
      const id = rep.project.id;
      let threadCount = 0;
      let lastActive = 0;
      for (const member of group.shown) {
        threadCount += member.threadCount;
        lastActive = Math.max(lastActive, member.lastActive ?? 0);
      }
      for (const member of group.listed) entryOf.set(member.project.id, id);
      members.set(id, group.shown);
      keys.set(id, projectEntryKey(id));
      // The representative's own row while it speaks for every member it
      // has, so the identity the cutoffs downstream compare stays stable.
      entries.push(threadCount === rep.threadCount && lastActive === (rep.lastActive ?? 0)
        ? rep
        : { ...rep, threadCount, lastActive });
    }
    // The newest activity of an entry, over the members it shows: a
    // streaming beat bumps a box keyed by its own project id, so reading
    // the representative's alone missed a member's beat and counted a
    // filtered-out member's. A function, not a field: a field would
    // allocate a new entry on every beat and defeat the cutoff below.
    const activityOf = (entryId: string): number => {
      let newest = 0;
      for (const member of members.get(entryId) ?? []) {
        newest = Math.max(newest, getProjectLiveActivityAt(member));
      }
      return newest;
    };
    return { entries, entryOf, members, keys, activityOf };
  });

  let previousThreadBuckets: ReadonlyMap<string, Thread[]> = new Map();
  let previousGroupBuckets: ReadonlyMap<string, ThreadGroup[]> = new Map();
  let previousAssistantLists: ReadonlyMap<'assistants', Thread[]> = new Map();
  let searchBuckets = $derived.by(() => {
    const filterDevices = sidebarDeviceFilterActive();
    const listedOn = treeListed();
    const entryOf = listedProjects.entryOf;
    const buckets = bucketSidebarThreads({
      threads: getThreads(),
      groups: getThreadGroups(),
      query,
      // The id of the header that exists, which is the listed members'
      // representative and not always the store's.
      entryOf: (projectId) => entryOf.get(projectId) ?? projectId,
      threadVisible: (thread) => sidebarThreadVisible(thread) && !hiddenAssistantThread(thread)
        && (!!thread.agentId || listedOn(threadMachine(thread.id, thread.projectId))),
      groupVisible: (group) => listedOn(threadGroupBackend(group.id) ?? HOME_BACKEND),
      filterDevices,
    });
    previousThreadBuckets = reuseUnchangedBuckets(previousThreadBuckets, buckets.threadsByProject);
    previousGroupBuckets = reuseUnchangedBuckets(previousGroupBuckets, buckets.groupsByProject);
    previousAssistantLists = reuseUnchangedBuckets(previousAssistantLists, new Map([['assistants', buckets.assistants] as const]));
    return {
      ...buckets,
      assistants: previousAssistantLists.get('assistants') ?? buckets.assistants,
    };
  });

  let threadsByProject = $derived(searchBuckets.threadsByProject);
  let groupsByProject = $derived(searchBuckets.groupsByProject);
  let assistants = $derived(searchBuckets.assistants);
  let projectsShown = $derived(isSidebarSectionShown('projects', query !== ''));

  // Visible computers with a catalog that has not loaded, or whose thread
  // list is still held, each with a status row naming the first such
  // catalog. While any remains, what the list shows is incomplete, so it is
  // never presented as empty.
  let pendingCatalogs = $derived.by(() => getAttachedBackends().flatMap((entry) => {
    if (!sidebarBackendVisible(entry.id)) return [];
    const loads = CATALOG_KINDS.map((kind) => ({ kind, load: catalogLoadState(entry.id, kind) }));
    const unloaded = loads.find(({ load }) => load.phase !== 'loaded');
    if (unloaded === undefined && catalogTreeListed(entry.id)) return [];
    const failed = loads.find(({ load }) => load.phase === 'failed');
    return [{
      backend: entry.id,
      name: hasMultipleBackends() ? backendDisplayName(entry) : '',
      kind: unloaded?.kind ?? 'threads',
      error: failed === undefined ? null : failed.load.error,
    }];
  }));

  // Visible projects: respect search (name match OR thread match) and
  // the current sort mode. Three modes:
  //   - lastActivity: most-recently-touched thread first (sidebar default).
  //   - createdAt: project creation time, newest first.
  //   - manual: user-defined via DnD; persisted in Project.sortPosition.
  // Identity cutoff: streaming beats bump the per-project activity box
  // (getProjectLiveActivityAt), which wakes this derived on every flush
  // while a turn runs. The rows themselves are stable references, so
  // when the re-sort lands in the same order the PREVIOUS array is
  // returned and the animated project each-block (FLIP measure = forced
  // layout) never reconciles for a beat that changed no ordering.
  // The listing and device tests are already applied, by member, in
  // listedProjects: every entry here has a member on a computer that passes
  // both, so this derivation reads no computer and makes one pass.
  let prevVisibleProjects: ProjectWithCounts[] = [];
  let visibleProjects = $derived.by(() => {
    const mode = getProjectSortMode();
    const entries: readonly ProjectWithCounts[] = listedProjects.entries
      .filter((p) => !p.project.archived)
      .filter((p) => {
        if (!query) return true;
        if (p.project.name.toLowerCase().includes(query)) return true;
        if ((threadsByProject.get(p.project.id)?.length ?? 0) > 0) return true;
        // A group whose name matched but whose members are all archived
        // still makes its project visible — the row the user searched for
        // is in it.
        return (groupsByProject.get(p.project.id)?.length ?? 0) > 0;
      });
    const next = [...entries].sort((a, b) => {
      switch (mode) {
        case 'manual': {
          const cmp = projectOrderPosition(a) - projectOrderPosition(b);
          if (cmp !== 0) return cmp;
          return a.project.createdAt - b.project.createdAt;
        }
        case 'createdAt':
          return b.project.createdAt - a.project.createdAt;
        case 'lastActivity':
        default: {
          const aActive = listedProjects.activityOf(a.project.id);
          const bActive = listedProjects.activityOf(b.project.id);
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
    if (
      next.length === prevVisibleProjects.length
      && next.every((p, i) => p === prevVisibleProjects[i])
    ) {
      return prevVisibleProjects;
    }
    prevVisibleProjects = next;
    return next;
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
    const matchedProjectIds = new Set<string>();
    for (const [projectId, threads] of threadsByProject.entries()) {
      if (threads.length > 0) matchedProjectIds.add(projectId);
    }
    for (const [projectId, groups] of groupsByProject.entries()) {
      if (groups.length > 0) matchedProjectIds.add(projectId);
    }
    for (const projectId of matchedProjectIds) {
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
    // Sidebar "+" opens a chat draft.
    try {
      await openDraftThreadForProject({
        projectId,
        targetPane: pane,
        openInNewPane: options.openInNewPane ?? false,
      });
    } catch (err) {
      console.error('Failed to create draft thread:', err);
      addErrorToast(userFacingError(err), err);
    }
  };

  // Per-project New Terminal from the compact menu (rooted at the project). openTerminalThread owns
  // its own error toast, so there's nothing to catch here.
  const handleNewTerminal: ProjectNewTerminalHandler = (projectId) => {
    void openTerminalThread({ projectId });
  };

  // Commit a manual reorder. The operation is the store's
  // (stores/projects.svelte.ts#reorderProjects): it writes the positions
  // locally so the list does not wait for the round trip, and puts back
  // what it wrote, where it is still what it wrote, if the write is
  // refused. What the user sees of a failure is this section's: the toast,
  // and a refresh of the tree, which reconciles an order the computer
  // applied in part.
  async function handleReorder(newOrderedIds: string[]): Promise<void> {
    try {
      await reorderProjects(newOrderedIds);
    } catch (err) {
      console.error('Failed to persist project order:', err);
      addErrorToast(userFacingError(err), err);
      try {
        await loadSidebarCatalogs();
      } catch {
        // Already shown by the catalog status row and retried there.
      }
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
    // The added or duplicate project is brought into view: a folded
    // Projects section unfolds, and so does its row.
    setSidebarSectionExpanded('projects', true);
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

<!--
  One scroll area for both sections: the Assistants section above, then
  the Projects header and list. ProjectList itself doesn't scroll.
-->
<div class="flex-1 min-h-0 overflow-y-auto" data-testid="sidebar-sections">
  {#if anyAssistantsOn()}
    <AssistantsSection
      {assistants}
      {pane}
      searching={query !== ''}
    />
  {/if}
  <section
    class="flex flex-col"
    aria-label="Projects"
    data-testid="sidebar-projects-section"
  >
    <header class="flex items-center gap-1 px-3 pt-2 pb-1.5">
      <SidebarSectionToggle section="projects" label="Projects" shown={projectsShown} />
      <h2 class="flex-1 min-w-0"><SidebarDeviceFilter /></h2>
      <ProjectSortMenu />
      <IconButton
        label="Import Sessions"
        title={importUngranted ? 'Not granted to this device' : undefined}
        size="sm"
        disabled={importUngranted}
        onClick={() => openSessionImport()}
      >
        {#snippet children()}
          <span data-testid="sidebar-import-sessions-icon" class="flex items-center">
            <Icon icon={History} size={13} strokeWidth={2} class="opacity-80" />
          </span>
        {/snippet}
      </IconButton>
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

    {#if projectsShown}
      <div data-flashing-project={flashProjectId}>
        {#if pendingCatalogs.length > 0}
          <div class="px-2 pt-1" data-testid="sidebar-catalog-status">
            {#each pendingCatalogs as pending (pending.backend)}
              <CatalogStatusRow backend={pending.backend} name={pending.name} kind={pending.kind} error={pending.error} />
            {/each}
          </div>
        {/if}
        {#if visibleProjects.length > 0 || pendingCatalogs.length === 0}
          <ProjectList
            projects={visibleProjects}
            membersByProject={listedProjects.members}
            entryKeys={listedProjects.keys}
            emptyMessage={sidebarDeviceFilterActive() ? 'No projects on the selected computers.' : undefined}
            {threadsByProject}
            {groupsByProject}
            {pane}
            onNewThread={handleNewThread}
            onNewTerminal={handleNewTerminal}
            onReorder={handleReorder}
          />
        {/if}
      </div>
    {/if}
  </section>
</div>

<AddProjectModal
  open={addProjectOpen}
  onClose={handleAddClose}
  onDuplicate={(id) => flashProject(id)}
  onCreated={(p) => flashProject(p.id)}
/>
<!--
  The import surface itself is NOT mounted here. Sidebar renders this
  section only while expanded, so a mod+b collapse would unmount the modal
  mid-run. It hangs off App.svelte with the other store-gated overlays; this
  header owns the trigger only.
-->
