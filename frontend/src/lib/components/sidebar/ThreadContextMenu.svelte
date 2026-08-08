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
  import { providerSupports } from '../../providers/catalog';
  import { openThreadFromNavigation, openThreadInNewPane } from '../../stores/panes.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { isViewOnlySession } from '../../transport/runMode';
  import { countNoun } from '../../utils/format';

  interface Props {
    thread: Thread;
    pane: ThreadPane | null;
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
      reportError: (msg) => {
        if (pane) pane.setGeneralError(msg);
        else addToast('error', msg);
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
  let importUpdatesViewOnly = $derived(isViewOnlySession());
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
          isActive: pane?.threadId === t.id,
          clearPane: () => pane?.clear(),
          switchPane: async (next) => {
            if (pane) await openThreadFromNavigation(next, pane);
            else await openThreadFromNavigation(next);
          },
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
      const message = `${targets.length - failures}/${targets.length} succeeded` +
        (lastError ? ` — ${lastError}` : '');
      if (pane) pane.setGeneralError(message);
      else addToast('error', message);
    }
  }

  // Single-row Delete gate. Mirrors ThreadRow's `handleDelete` (the
  // terminal row-X): honor the global confirmDelete setting — off →
  // delete immediately, on → confirm first. Bulk delete (runBulk via
  // showBulkDeleteConfirm) keeps its own always-on confirm; a
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
    if (getSettings().confirmDelete) {
      showDeleteConfirm = true;
    } else {
      void deleteThreadAction(ctx());
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
            label="Open in New Pane"
            onSelect={() => {
              onClose();
              void openThreadInNewPane(thread);
            }}
          />
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
          {#if canCheckImportUpdates}
            <MenuItem
              label={checkingUpdates ? 'Checking for Provider Updates…' : 'Check for Provider Updates'}
              disabled={checkingUpdates || importUpdatesViewOnly}
              title={importUpdatesViewOnly ? 'Local only' : undefined}
              onSelect={() => void handleCheckImportUpdates()}
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
            <MenuItem label="Delete" variant="danger" onSelect={handleDelete} />
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

<!-- Non-destructive, so Enter accepts: appending the session file's newer
     messages adds history, it never rewrites what is already there. -->
<ConfirmDialog
  open={pendingUpdate !== null}
  title="Import New Items"
  description={pendingUpdateDescription}
  confirmLabel="Import"
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
