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
  import { addErrorToast } from '../../stores/toast.svelte';
  import {
    deleteThreadGroupAction,
    newThreadInGroupAction,
    removeThreadsFromGroupAction,
  } from './threadGroupActions';
  import { archiveThreadAction, markAllReadAction } from './threadRowActions';
  import { projectScopeGrant } from '../../transport/entityScopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';

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
   */
  function currentMemberThreadIds(): string[] {
    return getThreads()
      .filter((t) => t.groupId === group.id && !t.archived)
      .map((t) => t.id);
  }

  let memberThreadIds = $derived(
    open || showArchiveConfirm || showDeleteConfirm ? currentMemberThreadIds() : [],
  );
  let memberCount = $derived(memberThreadIds.length);
  // Every row writes the group or its members on the project's computer;
  // all of it rides `threads:operate`. Inert with a reason, never hidden.
  let operateGrant = $derived(projectScopeGrant('threads:operate', group.projectId));
  let operateUngranted = $derived(operateGrant !== 'granted');
  let ungrantedTitle = $derived(grantBlockedTitle(operateGrant));

  /**
   * Archive every member, sequentially and for the same reason
   * ThreadContextMenu's bulk walk is sequential: parallel writes race each
   * other against one SQLite store, and a pile of simultaneous toasts hides
   * whichever failure actually mattered.
   */
  async function archiveMembers(ids: readonly string[] = currentMemberThreadIds()): Promise<void> {
    for (const id of ids) {
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
          reportError: (msg, err) => addErrorToast(msg, err),
        });
      } catch (err) {
        addErrorToast(err instanceof Error ? err.message : String(err), err);
      }
    }
  }

  function handleArchive(): void {
    const ids = currentMemberThreadIds();
    onClose();
    if (getSettings().confirmArchive) showArchiveConfirm = true;
    else void archiveMembers(ids);
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
          label="New Thread"
          disabled={operateUngranted}
          title={ungrantedTitle}
          onSelect={() => {
            onClose();
            void newThreadInGroupAction(group, pane);
          }}
        />
        <MenuItem
          label="Rename Group"
          disabled={operateUngranted}
          title={ungrantedTitle}
          onSelect={() => {
            onClose();
            onRename();
          }}
        />
        <MenuItem
          label="Mark All Read"
          disabled={memberCount === 0 || operateUngranted}
          title={ungrantedTitle}
          onSelect={() => {
            const ids = currentMemberThreadIds();
            onClose();
            void markAllReadAction(ids.flatMap((id) => getThreadById(id) ?? []));
          }}
        />
        <MenuItem
          label={`Archive Threads (${memberCount})`}
          disabled={memberCount === 0 || operateUngranted}
          title={ungrantedTitle}
          onSelect={handleArchive}
        />
        <MenuItem
          label="Ungroup All"
          disabled={memberCount === 0 || operateUngranted}
          title={ungrantedTitle}
          onSelect={() => {
            const ids = currentMemberThreadIds();
            onClose();
            void removeThreadsFromGroupAction(ids);
          }}
        />
        <MenuDivider />
        <MenuItem
          label="Delete Group"
          variant="danger"
          disabled={operateUngranted}
          title={ungrantedTitle}
          onSelect={handleDelete}
        />
      {/snippet}
    </Menu>
  {/snippet}
</Popover>

{#if showArchiveConfirm}
  <ConfirmDialog
    open={true}
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
{/if}

{#if showDeleteConfirm}
  <ConfirmDialog
    open={true}
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
{/if}
