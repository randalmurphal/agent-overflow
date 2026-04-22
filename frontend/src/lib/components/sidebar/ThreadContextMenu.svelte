<script lang="ts">
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
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import {
    copyThreadIdAction,
    copyThreadPathAction,
    deleteThreadAction,
    markThreadUnreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';

  interface Props {
    thread: Thread;
    pane: ThreadPane;
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: () => void;
    /** Fires when the user selects Rename — the parent row owns the
     *  inline input so focus + blur save happen in place. */
    onRename: () => void;
    /** Whether the row is currently the pane's active thread. Delete
     *  clears the pane when true. */
    isActive: boolean;
  }

  let {
    thread,
    pane,
    anchor,
    open,
    onClose,
    onRename,
    isActive,
  }: Props = $props();

  let showDeleteConfirm = $state(false);

  function ctx(): ThreadActionCtx {
    return {
      thread,
      isActive,
      clearPane: () => pane.clear(),
      switchPane: (t) => pane.switchThread(t),
      reportError: (msg) => pane.setGeneralError(msg),
    };
  }
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="bottom-start"
  role="menu"
  ariaLabel="Thread actions"
>
  {#snippet children()}
    <Menu ariaLabel="Thread actions" {onClose}>
      {#snippet children()}
        <MenuItem
          label="Rename thread"
          onSelect={() => {
            onClose();
            onRename();
          }}
        />
        <MenuItem
          label="Mark unread"
          onSelect={() => {
            onClose();
            void markThreadUnreadAction(ctx());
          }}
        />
        <MenuItem
          label="Copy path"
          onSelect={() => {
            onClose();
            void copyThreadPathAction(ctx());
          }}
        />
        <MenuItem
          label="Copy thread ID"
          onSelect={() => {
            onClose();
            void copyThreadIdAction(ctx());
          }}
        />
        <MenuDivider />
        <MenuItem
          label="Delete"
          variant="danger"
          onSelect={() => {
            onClose();
            showDeleteConfirm = true;
          }}
        />
      {/snippet}
    </Menu>
  {/snippet}
</Popover>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete thread"
  description="This will permanently delete this thread and all its messages. This action cannot be undone."
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => {
    showDeleteConfirm = false;
    void deleteThreadAction(ctx());
  }}
  onCancel={() => {
    showDeleteConfirm = false;
  }}
/>
