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

  import { onDestroy, untrack } from 'svelte';
  import type { Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { reorderProjects } from '../../stores/projects.svelte';
  import { buildSidebarEntries, rankOf, sidebarBucketOf, type SidebarEntry } from './sidebarEntries';
  import { revealProject, setEntryExpanded } from '../../stores/sidebarEntryContinuity.svelte';
  import {
    isSidebarSectionShown,
    getProjectSortMode,
    setSidebarSectionExpanded,
    type ProjectSortMode,
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
  import { scopeGrant } from '../../transport/scopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import History from '@lucide/svelte/icons/history';
  import Plus from '@lucide/svelte/icons/plus';
  import IconButton from '../primitives/IconButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import SidebarDeviceFilter from './SidebarDeviceFilter.svelte';
  import { sidebarBackendVisible, sidebarDeviceFilterActive, sidebarThreadVisible } from '../../stores/sidebarDevices.svelte';
  import { threadGroupBackend } from '../../transport/entityIndex';
  import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
  import {
    backendDisplayName,
    getAttachedBackends,
    hasMultipleBackends,
    threadMachine,
  } from '../../stores/attachedBackends.svelte';
  import { catalogTreeListed, computerCatalogPending, loadSidebarCatalogs } from '../../stores/sidebarCatalogs';
  import { backendUnreachableSettled, getTransportStatusFor } from '../../stores/transportStatus.svelte';
  import { isTerminalConnectionStatus } from '../../transport/connectionRefusal';
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
  let importGrant = $derived(scopeGrant('threads:operate', selectedBackend()));

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
  // (catalogTreeListed): its project headers by the entry continuity store,
  // which groups only listed computers' rows, and its project threads and
  // its groups here. An assistant's row belongs to no project and is not
  // held.
  //
  // `treeListed` is what holds the threads and groups, so what a computer
  // shows of them before its first complete round is one decision. Its memo
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

  // One derivation of every entry the sidebar renders and of everything a
  // header shows or the list sorts by (./sidebarEntries.ts, which carries
  // the rules). Nothing below reads a project row.
  let previousEntries: readonly SidebarEntry[] = [];
  let sidebarEntries = $derived.by(() => {
    const built = buildSidebarEntries({ visible: sidebarBackendVisible, previous: previousEntries });
    previousEntries = built;
    return built;
  });

  let previousThreadBuckets: ReadonlyMap<string, Thread[]> = new Map();
  let previousGroupBuckets: ReadonlyMap<string, ThreadGroup[]> = new Map();
  let previousAssistantLists: ReadonlyMap<'assistants', Thread[]> = new Map();
  let searchBuckets = $derived.by(() => {
    const filterDevices = sidebarDeviceFilterActive();
    const listedOn = treeListed();
    const buckets = bucketSidebarThreads({
      threads: getThreads(),
      groups: getThreadGroups(),
      query,
      // The rowKey of the project's entry, which neither the filter nor its
      // key moving changes.
      entryOf: sidebarBucketOf,
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
  // list is still held.
  let pendingCatalogs = $derived.by(() => getAttachedBackends().flatMap((entry) => {
    if (!sidebarBackendVisible(entry.id)) return [];
    const pending = computerCatalogPending(entry.id);
    if (pending === null) return [];
    return [{ backend: entry.id, name: hasMultipleBackends() ? backendDisplayName(entry) : '', ...pending }];
  }));

  // Only this computer's own pending lists get a line, and only while a load
  // can make progress: the computer is starting, dialing for the first time,
  // or connected and reading. While it is unreachable the banner says so and
  // carries Retry. A paired computer's connection and load state live in
  // the Projects menu (SidebarDeviceFilter.svelte), so a computer that is
  // off or keeps dropping changes nothing here but its own rows.
  let homeStatus = $derived(getTransportStatusFor(HOME_BACKEND).status);
  let homeLine = $derived(homeStatus === 'reconnecting' || isTerminalConnectionStatus(homeStatus)
    ? undefined
    : pendingCatalogs.find((pending) => pending.backend === HOME_BACKEND));

  // While a pending computer may still answer, what the list shows is
  // incomplete, so it is never presented as empty. One unreachable for
  // UNREACHABLE_SETTLE_MS no longer holds the empty state off: a person
  // with no projects is not left with a blank list while a computer is off.
  let awaitingCatalogs = $derived(pendingCatalogs.some((pending) => !backendUnreachableSettled(pending.backend)));

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
  // The listing and device tests are applied by member in sidebarEntries,
  // and an entry keeps its identity while nothing about it changed, so a
  // beat that moves no row returns the previous array.
  let prevVisibleProjects: readonly SidebarEntry[] = [];
  let visibleProjects = $derived.by(() => {
    const mode = getProjectSortMode();
    const entries = sidebarEntries.filter((entry) => {
      if (!query) return true;
      if (entry.name.toLowerCase().includes(query)) return true;
      if ((threadsByProject.get(entry.rowKey)?.length ?? 0) > 0) return true;
      // A group whose name matched but whose members are all archived
      // still makes its project visible — the row the user searched for
      // is in it.
      return (groupsByProject.get(entry.rowKey)?.length ?? 0) > 0;
    });
    const next = sortEntries(entries, mode);
    if (
      next.length === prevVisibleProjects.length
      && next.every((entry, i) => entry === prevVisibleProjects[i])
    ) {
      return prevVisibleProjects;
    }
    prevVisibleProjects = next;
    return next;
  });

  // The three orders. Last activity ranks each entry once per sort: a rank
  // scans the members and reads their live activity, which the comparator
  // would otherwise repeat for every pair it compares.
  function sortEntries(entries: readonly SidebarEntry[], mode: ProjectSortMode): SidebarEntry[] {
    switch (mode) {
      case 'manual':
        return [...entries].sort((a, b) => (a.position - b.position) || (a.createdAt - b.createdAt));
      case 'createdAt':
        return [...entries].sort((a, b) => b.createdAt - a.createdAt);
      case 'lastActivity': {
        const rank = new Map(entries.map((entry) => [entry, rankOf(entry)]));
        return [...entries].sort((a, b) => (rank.get(b)! - rank.get(a)!) || a.name.localeCompare(b.name));
      }
    }
  }

  // A search opens every row whose threads or groups matched, so its results
  // show without a chevron click. The opening is this section's view of the
  // search, never a write to the row's fold: a fold written here would be
  // copied to a checkout that splits off mid-search, and kept by a client
  // closed mid-search. A row shows open while its fold says so or the search
  // opens it.
  //
  // A row the user folds or unfolds during the search is theirs again: its
  // fold is written as always, and the search stops opening it until the row
  // stops matching or the search clears.
  const NO_ROWS: ReadonlySet<string> = new Set();
  let searchMatched = $derived.by(() => {
    if (!query) return NO_ROWS;
    const matched = new Set<string>();
    for (const entry of sidebarEntries) {
      if ((threadsByProject.get(entry.rowKey)?.length ?? 0) > 0 || (groupsByProject.get(entry.rowKey)?.length ?? 0) > 0) {
        matched.add(entry.rowKey);
      }
    }
    return matched;
  });
  let searchToggled: ReadonlySet<string> = $state.raw(NO_ROWS);
  $effect.pre(() => {
    const matched = searchMatched;
    untrack(() => {
      if (searchToggled.size === 0) return;
      const kept = new Set([...searchToggled].filter((rowKey) => matched.has(rowKey)));
      if (kept.size !== searchToggled.size) searchToggled = kept.size === 0 ? NO_ROWS : kept;
    });
  });
  let searchOpened = $derived.by(() => {
    if (searchMatched.size === 0) return NO_ROWS;
    if (searchToggled.size === 0) return searchMatched;
    return new Set([...searchMatched].filter((rowKey) => !searchToggled.has(rowKey)));
  });

  function setRowExpanded(entry: SidebarEntry, expanded: boolean): void {
    if (searchMatched.has(entry.rowKey) && !searchToggled.has(entry.rowKey)) {
      searchToggled = new Set(searchToggled).add(entry.rowKey);
    }
    setEntryExpanded(entry.key, expanded);
  }

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
    revealProject(id);
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
        title={grantBlockedTitle(importGrant)}
        size="sm"
        disabled={importGrant !== 'granted'}
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
        {#if homeLine}
          <div class="px-2 pt-1" data-testid="sidebar-catalog-status">
            <CatalogStatusRow backend={homeLine.backend} name={homeLine.name} kind={homeLine.kind} error={homeLine.error} />
          </div>
        {/if}
        {#if visibleProjects.length > 0 || !awaitingCatalogs}
          <ProjectList
            entries={visibleProjects}
            onSetExpanded={setRowExpanded}
            {searchOpened}
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
