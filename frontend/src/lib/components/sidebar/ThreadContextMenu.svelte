<script lang="ts">
  import { threadHasScope } from '../../transport/entityScopes';
  // Popover-anchored right-click menu for a thread row. Mirrors the
  // ProjectContextMenu composition: Popover anchors to the row element,
  // Menu owns keyboard nav, MenuItem renders each action. Confirm
  // dialogs for the destructive Delete path live here so the owning
  // row doesn't have to duplicate them.
  //
  // Rename is delegated back to the row via the `onRename` prop so the
  // inline `<input>` can render in place of the title span — same
  // pattern as ProjectItem.
  //
  // Every action goes through threadRowActions.ts / ThreadActionCtx so
  // the menu stays a thin rendering shell; the business bits (bindings,
  // optimistic store patches, toasts) live with their siblings.

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import Popover from '../primitives/Popover.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import Menu from '../primitives/Menu.svelte';
  import { openAssistantProjectsDialog } from '../../stores/newAssistant.svelte';
  import { openPreviousThreadsDialog } from '../../stores/assistantPastThreads.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuSubmenuItem from '../primitives/MenuSubmenuItem.svelte';
  import {
    archiveThreadAction,
    copyThreadIdAction,
    copyThreadPathAction,
    deleteThreadAction,
    deleteThreadsAction,
    forkThreadAction,
    transferThreadAction,
    markThreadUnreadAction,
    PIN_GROUP_BACK,
    PIN_GROUP_FRONT,
    nextAssistantThreadAction,
    pinThreadAction,
    setThreadPinGroupAction,
    unpinThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';
  import { isStandingAgent } from '../../utils/sidebarTree';
  import {
    applyThreadImportUpdatesAction,
    checkThreadImportUpdatesAction,
  } from './threadImportUpdates';
  import type { ImportUpdateStatus } from '../../types/sessionImport';
  import {
    clearThreadSelection,
    getSelectedThreadIds,
    isThreadSelected,
  } from '../../stores/threadFilter.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { getThreadGroupsForProject } from '../../stores/threadGroups.svelte';
  import { expandProject } from '../../stores/sidebar.svelte';
  import {
    createThreadGroupAndMoveAction,
    moveThreadsToGroupAction,
    removeThreadsFromGroupAction,
    setThreadPlacementAction,
  } from './threadGroupActions';
  import { providerSupports } from '../../providers/catalog';
  import { openThreadFromNavigation, openThreadInNewPane } from '../../stores/panes.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { hasScope } from '../../transport/scopes';
  import { countNoun } from '../../utils/format';
  import { canOfferConversationTransfer } from '../../stores/conversationTransfers.svelte';
  import {
    ThreadDeleteNote,
    confirmsThreadDelete,
    threadDeleteTitle,
    withDeleteNote,
  } from '../../stores/assistants.svelte';
  import { threadMachine } from '../../stores/attachedBackends.svelte';

  interface Props {
    thread: Thread;
    pane: ThreadPane | null;
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: (reason?: PopoverCloseReason) => void;
    /** Fires when the user selects Rename — the parent row owns the
     *  inline input so focus + blur save happen in place. */
    onRename: () => void;
    /** Whether the row is currently the pane's active thread. Delete
     *  clears the pane when true. */
    isActive: boolean;
    /** A standing agent's own row: no group items. */
    agentBound?: boolean;
  }

  let {
    thread,
    pane,
    anchor,
    open,
    onClose,
    onRename,
    isActive,
    agentBound = false,
  }: Props = $props();

  let showDeleteConfirm = $state(false);
  let showArchiveConfirm = $state(false);
  let showBulkDeleteConfirm = $state(false);

  // ── Assistant ────────────────────────────────────────────────────────────
  //
  // An assistant's current thread offers New Thread, Same Assistant, its
  // projects, its previous threads and its loops and watches; its delete is
  // always confirmed and the confirm names its home folder and what is
  // removed with it.
  let isAgent = $derived(isStandingAgent(thread));

  async function openLoopsAndWatches(): Promise<void> {
    onClose();
    const target = pane ? await openThreadFromNavigation(thread, pane) : await openThreadFromNavigation(thread);
    target.setActivityRailBackgroundOpen(true);
  }
  const deleteNote = new ThreadDeleteNote();
  // Provider-update check: in flight while the backend reads the session
  // file (it builds the rows a refresh WOULD write, so it is a real read of
  // a possibly-large transcript, not an instant stat), then the plan it
  // found, which the confirm dialog describes.
  let checkingUpdates = $state(false);
  let pendingUpdate = $state<ImportUpdateStatus | null>(null);

  function ctx(): ThreadActionCtx {
    return {
      thread,
      isActive,
      clearPane: () => pane?.clear(),
      switchPane: async (t) => {
        if (pane) await openThreadFromNavigation(t, pane);
        else await openThreadFromNavigation(t);
      },
      reportError: (msg, err) => {
        if (pane) pane.setGeneralError(msg, err);
        else addErrorToast(msg, err);
      },
    };
  }

  // Multi-select context: when the row that opened this menu is part of
  // a multi-thread selection (N>1), the menu swaps to bulk actions so a
  // single right-click can archive / delete every selected thread at
  // once. Falls back to the single-row menu when the right-clicked row
  // is not in the selection.
  let selectedIds = $derived(getSelectedThreadIds());
  let inBulkContext = $derived(
    selectedIds.size > 1 && isThreadSelected(thread.id),
  );

  // Fork is gated on the same predicate the palette command uses
  // (canForkActiveThread) — sessionRef is the cheap stand-in for "this
  // thread has at least one turn and the provider has been started." It is
  // additionally gated on provider support: claude-tui has no AO-mediated
  // fork (forking happens inside the TUI, reached via take-control).
  let canFork = $derived(
    Boolean(thread.sessionRef) && providerSupports(thread.provider, 'fork'),
  );
  // Only a thread that came FROM a provider session file can be refreshed
  // from one. sessionRef would be the wrong gate — every thread that has run
  // a turn has one — so this reads the write-once import stamp.
  let canCheckImportUpdates = $derived(Boolean(thread.importSource));
  // Reading the provider session file is a local-disk operation the server
  // refuses over a remote connection. Visible-but-disabled, per the §10
  // treatment every other mutating affordance uses: hiding it would read as
  // "this thread wasn't imported", which is a different fact.
  // CheckThreadImportUpdates re-reads the provider session file and writes
  // what it finds into the thread.
  // The backend ships user-facing prose for the verdict it returned; it
  // knows the turn count and the exact wording, so it wins. The fallback
  // only covers a backend that sends none — and says the same two numbers.
  let pendingUpdateDescription = $derived.by(() => {
    if (pendingUpdate?.detail) return pendingUpdate.detail;
    const items = countNoun(pendingUpdate?.newItems ?? 0, 'new item');
    const turns = pendingUpdate?.newTurns ?? 0;
    const across = turns > 0 ? ` across ${countNoun(turns, 'turn')}` : '';
    return `${items}${across} since this thread was imported.`;
  });
  let profileOnlyUpdate = $derived(
    Boolean(pendingUpdate?.restoresModelProfile) && (pendingUpdate?.newItems ?? 0) === 0,
  );

  let canArchive = $derived(thread.mode !== 'terminal');
  let isPinned = $derived(thread.pinnedAt != null);
  let isBackBurner = $derived(isPinned && thread.pinGroup === PIN_GROUP_BACK);
  let selectedThreads = $derived.by(() => {
    if (!inBulkContext) return [] as Thread[];
    const out: Thread[] = [];
    for (const id of selectedIds) {
      const t = getThreadById(id);
      if (t) out.push(t);
    }
    return out;
  });
  // Every write here rides `threads:operate`, judged per thread on the
  // computer that owns it. A bulk menu is inert when ANY selected row
  // lacks it: the walk would otherwise succeed on some and refuse the rest.
  let operateUngranted = $derived(
    inBulkContext
      ? selectedThreads.some((t) => !threadHasScope('threads:operate', t.id, t.projectId))
      : !threadHasScope('threads:operate', thread.id, thread.projectId),
  );
  let ungrantedTitle = $derived(operateUngranted ? 'Not granted to this device' : undefined);
  // StartNextAssistantThread and SetAssistantProjects ride threads:autonomy.
  let nextThreadUngranted = $derived(!threadHasScope('threads:autonomy', thread.id, thread.projectId));

  /**
   * Run an async per-thread action across the selection sequentially —
   * a Promise.all() would race writes against the same SQLite store and
   * the toast pile-up on errors would mask the underlying failure. The
   * sequential walk also makes the optimistic-removal animation read as
   * an intentional cascade.
   *
   * Selection is cleared AFTER the loop, not before — if a mid-batch
   * failure throws, the user keeps the selection and can retry the
   * unaffected rows. Each action reports its failure through the
   * `reportError` given here, which counts it; the walk shows one
   * aggregate toast at the end.
   */
  /**
   * Delete the selection as one delete per computer (deleteThreadsAction).
   * Selection is cleared once every computer's delete went through. A
   * failure is a toast, as runBulk's is: the menu's pane may show a thread
   * the delete just removed.
   */
  async function deleteSelected(): Promise<void> {
    const complete = await deleteThreadsAction(selectedThreads.slice(), (msg, err) => addErrorToast(msg, err));
    if (complete) clearThreadSelection();
  }

  async function runBulk(
    action: (perThreadCtx: ThreadActionCtx) => Promise<void>,
  ): Promise<void> {
    const targets = selectedThreads.slice();
    let failures = 0;
    let lastError: string | null = null;
    let lastCause: unknown;
    for (const t of targets) {
      try {
        await action({
          thread: t,
          isActive: pane?.threadId === t.id,
          clearPane: () => pane?.clear(),
          switchPane: async (next) => {
            if (pane) await openThreadFromNavigation(next, pane);
            else await openThreadFromNavigation(next);
          },
          reportError: (msg, err) => {
            failures += 1;
            lastError = msg;
            lastCause = err;
          },
        });
      } catch (err) {
        failures += 1;
        lastError = err instanceof Error ? err.message : String(err);
        lastCause = err;
      }
    }
    if (failures === 0) {
      clearThreadSelection();
    } else {
      const message = `${targets.length - failures}/${targets.length} succeeded` +
        (lastError ? ` — ${lastError}` : '');
      // A toast: a pane the walk archived or deleted is gone by now.
      addErrorToast(message, lastCause);
    }
  }

  // Single-row Delete gate. Mirrors ThreadRow's `handleDelete` (the
  // terminal row-X): honor the global confirmDelete setting — off →
  // delete immediately, on → confirm first. Bulk delete (deleteSelected
  // via showBulkDeleteConfirm) keeps its own always-on confirm; a
  // multi-thread delete is a higher-stakes action.
  /**
   * The check reads the source transcript, which on a large session is not
   * instant — so the menu stays open with the item in a disabled "checking"
   * state rather than closing onto nothing. It closes once there is
   * something to say: a toast (handled inside the action) or the confirm
   * dialog below.
   */
  async function handleCheckImportUpdates(): Promise<void> {
    if (checkingUpdates) return;
    checkingUpdates = true;
    try {
      pendingUpdate = await checkThreadImportUpdatesAction(ctx());
    } finally {
      checkingUpdates = false;
      onClose();
    }
  }

  function handleDelete(): void {
    onClose();
    if (confirmsThreadDelete([thread])) {
      showDeleteConfirm = true;
      deleteNote.load([thread]);
    } else {
      void deleteThreadAction(ctx());
    }
  }

  // Same shape as ThreadRow's hover Archive: the confirmArchive setting
  // decides whether the dialog comes first.
  function handleArchive(): void {
    onClose();
    if (getSettings().confirmArchive) {
      showArchiveConfirm = true;
    } else {
      void archiveThreadAction(ctx());
    }
  }

  // ── Groups ───────────────────────────────────────────────────────────────
  //
  // Bulk moves need one project, because a group belongs to one — the store
  // would refuse the cross-project half of the call and roll the rest back.
  let bulkProjectId = $derived.by(() => {
    if (!inBulkContext) return '';
    let shared: string | null = null;
    for (const t of selectedThreads) {
      const id = t.projectId ?? '';
      if (!id) return '';
      if (shared === null) shared = id;
      else if (shared !== id) return '';
    }
    return shared ?? '';
  });
  let groupProjectId = $derived(inBulkContext ? bulkProjectId : (thread.projectId ?? ''));
  let moveTargetIds = $derived(inBulkContext ? selectedThreads.map((t) => t.id) : [thread.id]);
  // Ungroup writes only the rows that are IN a group; passing the rest would
  // spend a round trip per row to change nothing.
  let removeTargetIds = $derived.by(() => {
    if (!inBulkContext) return thread.groupId ? moveTargetIds : [];
    return selectedThreads
      .filter((t) => t.groupId)
      .map((t) => t.id);
  });
  // A standing agent's own row never joins a group; a bulk selection
  // holding one has no group move either.
  let bulkAgentBound = $derived(inBulkContext && selectedThreads.some((t) => isStandingAgent(t)));
  let canMoveToGroup = $derived(
    Boolean(groupProjectId) && moveTargetIds.length > 0
      && !(inBulkContext ? bulkAgentBound : agentBound),
  );
  let currentGroupId = $derived(inBulkContext ? '' : (thread.groupId ?? ''));
  let canRemoveFromGroup = $derived(canMoveToGroup && removeTargetIds.length > 0);
  // Sorted by name so the submenu reads as a list the user curated, not as
  // backend insertion order.
  let projectGroups = $derived.by(() => {
    if (!canMoveToGroup) return [];
    return [...getThreadGroupsForProject(groupProjectId)]
      .sort((a, b) => a.name.localeCompare(b.name));
  });

  // ── Placement ────────────────────────────────────────────────────────────
  //
  // A thread another thread spawned into a project, on this computer or a
  // paired one, is that thread's worker; it has a row here because it was
  // placed standalone or in a group (docs/specs/standing-agents.md ruling
  // 36). Remove from Sidebar makes it owned again, out of any group. The
  // backend refuses a thread no thread spawned as its worker and one
  // outside every project.
  let canRemoveFromSidebar = $derived(
    !isAgent && Boolean(thread.spawnedByThreadId || thread.spawnedByComputerId) && Boolean(thread.projectId),
  );

  // The project is expanded first for the same reason the project header
  // does it: the new group's row opens inline rename, and a row inside a
  // collapsed project would never render the editor.
  async function createGroupAndMove(): Promise<void> {
    expandProject(groupProjectId);
    await createThreadGroupAndMoveAction(groupProjectId, moveTargetIds);
  }
</script>

<!--
  The "Move to Group" submenu body, shared by the single-row and bulk menus:
  the project's groups, then the create-and-move escape hatch. The row's
  CURRENT group is rendered checked and inert — it is the answer to "where is
  this?", not an action.
-->
{#snippet groupTargets()}
  {#each projectGroups as group (group.id)}
    <MenuItem
      label={group.name}
      checked={group.id === currentGroupId}
      disabled={group.id === currentGroupId || operateUngranted}
      title={ungrantedTitle}
      onSelect={() => {
        onClose();
        void moveThreadsToGroupAction(moveTargetIds, group.id);
      }}
    />
  {/each}
  {#if projectGroups.length > 0}
    <MenuDivider />
  {/if}
  <MenuItem
    label="New Group…"
    disabled={operateUngranted}
    title={ungrantedTitle}
    onSelect={() => {
      onClose();
      void createGroupAndMove();
    }}
  />
{/snippet}

<Popover
  {anchor}
  {open}
  {onClose}
  dismissOnAnchorClick
  placement="bottom-start"
  role="none"
>
  {#snippet children()}
    <Menu
      ariaLabel={inBulkContext ? `Actions for ${selectedIds.size} Threads` : 'Thread Actions'}
      {onClose}
    >
      {#snippet children()}
        {#if inBulkContext}
          <MenuItem
            label={`Mark unread (${selectedIds.size})`}
            disabled={operateUngranted}
            title={ungrantedTitle}
            onSelect={() => {
              onClose();
              void runBulk(markThreadUnreadAction);
            }}
          />
          {#if canMoveToGroup}
            <MenuSubmenuItem label="Move to Group" disabled={operateUngranted}>
              {@render groupTargets()}
            </MenuSubmenuItem>
          {/if}
          {#if canRemoveFromGroup}
            <MenuItem
              label="Remove from Group"
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={() => {
                onClose();
                void removeThreadsFromGroupAction(removeTargetIds);
              }}
            />
          {/if}
          <MenuItem
            label={`Archive (${selectedIds.size})`}
            disabled={operateUngranted}
            title={ungrantedTitle}
            onSelect={() => {
              onClose();
              void runBulk(archiveThreadAction);
            }}
          />
          <MenuDivider />
          <MenuItem
            label={`Delete (${selectedIds.size})`}
            variant="danger"
            disabled={operateUngranted}
            title={ungrantedTitle}
            onSelect={() => {
              onClose();
              showBulkDeleteConfirm = true;
              deleteNote.load(selectedThreads.slice());
            }}
          />
        {:else}
          <MenuItem
            label="Open in New Pane"
            onSelect={() => {
              onClose();
              void openThreadInNewPane(thread);
            }}
          />
          <MenuItem
            label="Rename Thread"
            disabled={operateUngranted}
            title={ungrantedTitle}
            onSelect={() => {
              onClose();
              onRename();
            }}
          />
          {#if isAgent}
            <MenuItem
              label="New Thread, Same Assistant"
              disabled={nextThreadUngranted}
              title={nextThreadUngranted ? 'Not granted to this device' : undefined}
              onSelect={() => {
                onClose();
                void nextAssistantThreadAction(ctx());
              }}
            />
            <MenuItem
              label="Projects…"
              disabled={nextThreadUngranted}
              title={nextThreadUngranted ? 'Not granted to this device' : undefined}
              onSelect={() => {
                onClose();
                openAssistantProjectsDialog(thread);
              }}
            />
            <MenuItem
              label="Previous Threads…"
              onSelect={() => {
                onClose();
                openPreviousThreadsDialog(thread, pane);
              }}
            />
            <MenuItem
              label="Loops and Watches"
              onSelect={() => void openLoopsAndWatches()}
            />
          {/if}
          {#if canFork}
            <MenuItem
              label="Fork Thread"
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={() => {
                onClose();
                void forkThreadAction(ctx());
              }}
            />
          {/if}
          {#if canOfferConversationTransfer(thread)}
            <MenuItem
              label="Move or copy to computer…"
              disabled={!hasScope('threads:operate', threadMachine(thread.id, thread.projectId))}
              onSelect={() => { onClose(); transferThreadAction(ctx()); }}
            />
          {/if}
          {#if canCheckImportUpdates}
            <MenuItem
              label={checkingUpdates ? 'Checking for Provider Updates…' : 'Check for Provider Updates'}
              disabled={checkingUpdates || operateUngranted}
              title={ungrantedTitle}
              onSelect={() => void handleCheckImportUpdates()}
            />
          {/if}
          <MenuItem
            label="Mark Unread"
            disabled={operateUngranted}
            title={ungrantedTitle}
            onSelect={() => {
              onClose();
              void markThreadUnreadAction(ctx());
            }}
          />
          {#if canMoveToGroup}
            <MenuSubmenuItem label="Move to Group" disabled={operateUngranted}>
              {@render groupTargets()}
            </MenuSubmenuItem>
          {/if}
          {#if canRemoveFromGroup}
            <MenuItem
              label="Remove from Group"
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={() => {
                onClose();
                void removeThreadsFromGroupAction(removeTargetIds);
              }}
            />
          {/if}
          {#if canRemoveFromSidebar}
            <MenuItem
              label="Remove from Sidebar"
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={() => {
                onClose();
                void setThreadPlacementAction([thread.id], 'owned');
              }}
            />
          {/if}
          {#if isAgent}
            <!-- An assistant's row ignores its thread's pin. -->
          {:else if isPinned}
            <MenuItem
              label={isBackBurner ? 'Move to Front Burner' : 'Move to Back Burner'}
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={() => {
                onClose();
                void setThreadPinGroupAction(
                  ctx(),
                  isBackBurner ? PIN_GROUP_FRONT : PIN_GROUP_BACK,
                );
              }}
            />
            <MenuItem
              label="Unpin Thread"
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={() => {
                onClose();
                void unpinThreadAction(ctx());
              }}
            />
          {:else}
            <MenuItem
              label="Pin Thread"
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={() => {
                onClose();
                void pinThreadAction(ctx());
              }}
            />
          {/if}
          <MenuItem
            label="Copy Path"
            onSelect={() => {
              onClose();
              void copyThreadPathAction(ctx());
            }}
          />
          <MenuItem
            label="Copy Thread ID"
            onSelect={() => {
              onClose();
              void copyThreadIdAction(ctx());
            }}
          />
          <MenuDivider />
          <!-- The row's hover Archive is unreachable without a pointer, so
               the menu carries it too. Terminals are not archivable. -->
          {#if canArchive}
            <MenuItem
              label="Archive Thread"
              disabled={operateUngranted}
              title={ungrantedTitle}
              onSelect={handleArchive}
            />
          {/if}
          <MenuItem
            label="Delete"
            variant="danger"
            disabled={operateUngranted}
            title={ungrantedTitle}
            onSelect={handleDelete}
          />
        {/if}
      {/snippet}
    </Menu>
  {/snippet}
</Popover>

<ConfirmDialog
  open={showDeleteConfirm}
  title={threadDeleteTitle(thread)}
  description={withDeleteNote(
    'This will permanently delete this thread and all its messages. This action cannot be undone.',
    deleteNote.note,
  )}
  confirmLabel="Delete"
  confirmDisabled={deleteNote.pending}
  destructive={true}
  onConfirm={() => {
    showDeleteConfirm = false;
    deleteNote.clear();
    void deleteThreadAction(ctx());
  }}
  onCancel={() => {
    showDeleteConfirm = false;
    deleteNote.clear();
  }}
/>

<ConfirmDialog
  open={showArchiveConfirm}
  title="Archive Thread"
  description="This will hide the thread from the sidebar. Open Settings → Storage to find it later."
  confirmLabel="Archive"
  onConfirm={() => {
    showArchiveConfirm = false;
    void archiveThreadAction(ctx());
  }}
  onCancel={() => {
    showArchiveConfirm = false;
  }}
/>

<!-- Non-destructive, so Enter accepts: new history is appended, and a profile
     repair uses compare-and-swap so it cannot replace a newer user choice. -->
<ConfirmDialog
  open={pendingUpdate !== null}
  title={profileOnlyUpdate ? 'Restore Model Settings' : 'Import Provider Updates'}
  description={pendingUpdateDescription}
  confirmLabel={profileOnlyUpdate ? 'Restore' : 'Import'}
  onConfirm={() => {
    pendingUpdate = null;
    void applyThreadImportUpdatesAction(ctx());
  }}
  onCancel={() => {
    pendingUpdate = null;
  }}
/>

<ConfirmDialog
  open={showBulkDeleteConfirm}
  title={`Delete ${selectedIds.size} Threads`}
  description={withDeleteNote(
    `This will permanently delete ${selectedIds.size} threads and all their messages. This action cannot be undone.`,
    deleteNote.note,
  )}
  confirmLabel="Delete"
  confirmDisabled={deleteNote.pending}
  destructive={true}
  onConfirm={() => {
    showBulkDeleteConfirm = false;
    deleteNote.clear();
    void deleteSelected();
  }}
  onCancel={() => {
    showBulkDeleteConfirm = false;
    deleteNote.clear();
  }}
/>
