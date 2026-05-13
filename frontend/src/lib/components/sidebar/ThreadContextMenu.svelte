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
    archiveThreadAction,
    copyThreadIdAction,
    copyThreadPathAction,
    deleteThreadAction,
    forkThreadAction,
    markThreadUnreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';
  import {
    clearThreadSelection,
    getSelectedThreadIds,
    isThreadSelected,
  } from '../../stores/threadFilter.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';

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
  let showBulkDeleteConfirm = $state(false);

  function ctx(): ThreadActionCtx {
    return {
      thread,
      isActive,
      clearPane: () => pane.clear(),
      switchPane: async (t) => { await openThreadFromNavigation(t, pane); },
      reportError: (msg) => pane.setGeneralError(msg),
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
  // thread has at least one turn and the provider has been started."
  let canFork = $derived(Boolean(thread.sessionRef));
  // Discussion children (threads with a parentThreadId) cannot be
  // deleted in isolation — the parent thread owns the subtree's
  // lifecycle. Matches forge's right-click visibility.
  let canDelete = $derived(!thread.parentThreadId);
  let selectedThreads = $derived.by(() => {
    if (!inBulkContext) return [] as Thread[];
    const out: Thread[] = [];
    for (const id of selectedIds) {
      const t = getThreadById(id);
      if (t) out.push(t);
    }
    return out;
  });

  /**
   * Run an async per-thread action across the selection sequentially —
   * a Promise.all() would race writes against the same SQLite store and
   * the toast pile-up on errors would mask the underlying failure. The
   * sequential walk also makes the optimistic-removal animation read as
   * an intentional cascade.
   *
   * Selection is cleared AFTER the loop, not before — if a mid-batch
   * failure throws, the user keeps the selection and can retry the
   * unaffected rows. Per-action error reporting is owned by each
   * individual action (toast + `pane.setGeneralError`); we surface a
   * lightweight aggregate signal via the action's own `reportError`
   * channel by counting failures.
   */
  async function runBulk(
    action: (perThreadCtx: ThreadActionCtx) => Promise<void>,
  ): Promise<void> {
    const targets = selectedThreads.slice();
    let failures = 0;
    let lastError: string | null = null;
    for (const t of targets) {
      try {
        await action({
          thread: t,
          isActive: pane.threadId === t.id,
          clearPane: () => pane.clear(),
          switchPane: async (next) => { await openThreadFromNavigation(next, pane); },
          reportError: (msg) => {
            lastError = msg;
          },
        });
      } catch (err) {
        failures += 1;
        lastError = err instanceof Error ? err.message : String(err);
      }
    }
    if (failures === 0) {
      clearThreadSelection();
    } else {
      pane.setGeneralError(
        `${targets.length - failures}/${targets.length} succeeded` +
          (lastError ? ` — ${lastError}` : ''),
      );
    }
  }
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="bottom-start"
  role="menu"
  ariaLabel="Thread Actions"
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
            onSelect={() => {
              onClose();
              void runBulk(markThreadUnreadAction);
            }}
          />
          <MenuItem
            label={`Archive (${selectedIds.size})`}
            onSelect={() => {
              onClose();
              void runBulk(archiveThreadAction);
            }}
          />
          <MenuDivider />
          <MenuItem
            label={`Delete (${selectedIds.size})`}
            variant="danger"
            onSelect={() => {
              onClose();
              showBulkDeleteConfirm = true;
            }}
          />
        {:else}
          <MenuItem
            label="Rename Thread"
            onSelect={() => {
              onClose();
              onRename();
            }}
          />
          {#if canFork}
            <MenuItem
              label="Fork Thread"
              onSelect={() => {
                onClose();
                void forkThreadAction(ctx());
              }}
            />
          {/if}
          <MenuItem
            label="Mark Unread"
            onSelect={() => {
              onClose();
              void markThreadUnreadAction(ctx());
            }}
          />
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
          {#if canDelete}
            <MenuDivider />
            <MenuItem
              label="Delete"
              variant="danger"
              onSelect={() => {
                onClose();
                showDeleteConfirm = true;
              }}
            />
          {/if}
        {/if}
      {/snippet}
    </Menu>
  {/snippet}
</Popover>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete Thread"
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

<ConfirmDialog
  open={showBulkDeleteConfirm}
  title={`Delete ${selectedIds.size} Threads`}
  description={`This will permanently delete ${selectedIds.size} threads and all their messages. This action cannot be undone.`}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => {
    showBulkDeleteConfirm = false;
    void runBulk(deleteThreadAction);
  }}
  onCancel={() => {
    showBulkDeleteConfirm = false;
  }}
/>
