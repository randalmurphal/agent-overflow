<script lang="ts">
  // Right-click menu for a group row. Same composition as ThreadContextMenu:
  // Popover anchors to the row, Menu owns keyboard nav, MenuItem renders each
  // action, and the confirm dialogs live here so the row doesn't duplicate
  // them. Rename is delegated back to the row via `onRename` because the
  // inline <input> renders in place of the name.
  //
  // None of these actions delete a thread. Archive Threads hides the members
  // (the group and the membership survive an unarchive), Ungroup All returns
  // them to the project's top level, and Delete Group ungroups rather than
  // cascading — which is what the dialog says.

  import type { ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import Popover from '../primitives/Popover.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { getThreadById, getThreads } from '../../stores/threads.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import {
    deleteThreadGroupAction,
    pinThreadGroupAction,
    removeThreadsFromGroupAction,
    setThreadGroupPinGroupAction,
    unpinThreadGroupAction,
  } from './threadGroupActions';
  import {
    archiveThreadAction,
    PIN_GROUP_BACK,
    PIN_GROUP_FRONT,
  } from './threadRowActions';

  interface Props {
    group: ThreadGroup;
    pane: ThreadPane | null;
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: (reason?: PopoverCloseReason) => void;
    /** Fires on Rename Group; the row owns the inline input. */
    onRename: () => void;
  }

  let { group, pane, anchor, open, onClose, onRename }: Props = $props();

  let showArchiveConfirm = $state(false);
  let showDeleteConfirm = $state(false);

  /**
   * Membership from the STORE, not from the rows on screen: under a search
   * filter the group renders only its matching members, and "Archive Threads
   * (n)" acting on that subset would silently do a fraction of what it says.
   * Top-level only — a discussion child follows its root through the backend.
   */
  let memberThreadIds = $derived(
    getThreads()
      .filter((t) => t.groupId === group.id && !t.archived && !t.parentThreadId)
      .map((t) => t.id),
  );
  let memberCount = $derived(memberThreadIds.length);
  let isPinned = $derived(group.pinnedAt != null);
  let isBackBurner = $derived(isPinned && group.pinGroup === PIN_GROUP_BACK);

  /**
   * Archive every member, sequentially and for the same reason
   * ThreadContextMenu's bulk walk is sequential: parallel writes race each
   * other against one SQLite store, and a pile of simultaneous toasts hides
   * whichever failure actually mattered.
   */
  async function archiveMembers(): Promise<void> {
    for (const id of memberThreadIds.slice()) {
      const thread = getThreadById(id);
      if (!thread) continue;
      try {
        await archiveThreadAction({
          thread,
          isActive: pane?.threadId === thread.id,
          clearPane: () => pane?.clear(),
          switchPane: async (next) => {
            if (pane) await openThreadFromNavigation(next, pane);
            else await openThreadFromNavigation(next);
          },
          reportError: (msg) => addToast('error', msg),
        });
      } catch (err) {
        addToast('error', err instanceof Error ? err.message : String(err));
      }
    }
  }

  function handleArchive(): void {
    onClose();
    if (getSettings().confirmArchive) showArchiveConfirm = true;
    else void archiveMembers();
  }

  function handleDelete(): void {
    onClose();
    if (getSettings().confirmDelete) showDeleteConfirm = true;
    else void deleteThreadGroupAction(group.id);
  }
</script>

<Popover {anchor} {open} {onClose} dismissOnAnchorClick placement="bottom-start" role="none">
  {#snippet children()}
    <Menu ariaLabel="Group Actions" {onClose}>
      {#snippet children()}
        <MenuItem
          label="Rename Group"
          onSelect={() => {
            onClose();
            onRename();
          }}
        />
        {#if isPinned}
          <MenuItem
            label={isBackBurner ? 'Move to Front Burner' : 'Move to Back Burner'}
            onSelect={() => {
              onClose();
              void setThreadGroupPinGroupAction(
                group.id,
                isBackBurner ? PIN_GROUP_FRONT : PIN_GROUP_BACK,
              );
            }}
          />
          <MenuItem
            label="Unpin Group"
            onSelect={() => {
              onClose();
              void unpinThreadGroupAction(group.id);
            }}
          />
        {:else}
          <MenuItem
            label="Pin Group"
            onSelect={() => {
              onClose();
              void pinThreadGroupAction(group.id);
            }}
          />
        {/if}
        <MenuItem
          label={`Archive Threads (${memberCount})`}
          disabled={memberCount === 0}
          onSelect={handleArchive}
        />
        <MenuItem
          label="Ungroup All"
          disabled={memberCount === 0}
          onSelect={() => {
            onClose();
            void removeThreadsFromGroupAction(memberThreadIds);
          }}
        />
        <MenuDivider />
        <MenuItem label="Delete Group" variant="danger" onSelect={handleDelete} />
      {/snippet}
    </Menu>
  {/snippet}
</Popover>

<ConfirmDialog
  open={showArchiveConfirm}
  title={`Archive ${memberCount} Thread${memberCount === 1 ? '' : 's'}`}
  description="This will hide the group's threads from the sidebar. Open Settings → Storage to find them later."
  confirmLabel="Archive"
  onConfirm={() => {
    showArchiveConfirm = false;
    void archiveMembers();
  }}
  onCancel={() => {
    showArchiveConfirm = false;
  }}
/>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete Group"
  description={`Remove this group. Its ${memberCount} thread${memberCount === 1 ? '' : 's'} stay in the project.`}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => {
    showDeleteConfirm = false;
    void deleteThreadGroupAction(group.id);
  }}
  onCancel={() => {
    showDeleteConfirm = false;
  }}
/>
