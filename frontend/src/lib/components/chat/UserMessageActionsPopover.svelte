<script lang="ts">
  // Anchored popover for per-user-message actions: revert (convo-only or
  // convo+workspace) and fork-from-here. Mirrors ThreadContextMenu's
  // Popover + Menu + MenuItem composition so the visual + keyboard
  // affordances match the rest of the app.
  //
  // Mounted from UserMessage.svelte's hover-fade icon button. Visibility
  // gating (no active turn, has checkpoint history) lives at the call
  // site so the trigger button is only present when at least one option
  // is actionable.
  //
  // The "checkpoint turn count" semantics: agent-overflow's convention
  // is that `checkpoint_turn_count = N` means "state right BEFORE turn N
  // starts". The user message at turn_index K is the prompt that opens
  // turn K, so reverting/forking to "before this message" targets
  // checkpointTurnCount = K. (Don't confuse with the assistant-message
  // mapping, which uses K+1 because it's after the turn ran.)

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Checkpoint } from '../../types/checkpoint';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { ForkThread, RevertToCheckpoint } from '../../stores/bindings';
  import { prependThread } from '../../stores/threads.svelte';
  import { expandProject } from '../../stores/sidebar.svelte';
  import type { Thread } from '../../types/models';
  import Popover from '../primitives/Popover.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';

  interface Props {
    pane: ThreadPane;
    /** turn_index (0-based) of the user message that opened this popover. */
    userTurnIndex: number;
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: () => void;
  }

  let { pane, userTurnIndex, anchor, open, onClose }: Props = $props();

  // checkpointTurnCount semantics: revert "to before this user msg" =
  // state right before turn K starts = checkpoint_turn_count = K (where
  // K is the user msg's 0-indexed turn_index). Same target for fork:
  // create a new thread starting from turn K.
  const targetCheckpointTurnCount = $derived(userTurnIndex);

  // Look up the checkpoint at this point so we can decide whether the
  // workspace-revert option should be enabled. The checkpoint at
  // count = K + 1 captures "state after turn K finished" — its `files`
  // list is the diff between turn K-1's end and turn K's end. If empty,
  // turn K had no workspace changes, so reverting workspace at this
  // point is a no-op and we gray out the option.
  const checkpointAfterThisTurn = $derived(
    pane.diffPanel.checkpoints.find(
      (c: Checkpoint) => c.checkpointTurnCount === userTurnIndex + 1,
    ),
  );
  const hasWorkspaceChanges = $derived(
    (checkpointAfterThisTurn?.files?.length ?? 0) > 0,
  );

  let busy = $state(false);

  async function doRevert(mode: 'conversation-only' | 'conversation-and-files'): Promise<void> {
    if (busy || !pane.thread) return;
    const thread = pane.thread;
    busy = true;
    try {
      await RevertToCheckpoint(thread.id, targetCheckpointTurnCount, mode);
      addToast(
        'success',
        mode === 'conversation-only'
          ? 'Reverted conversation to this point'
          : 'Reverted conversation and files to this point',
      );
      // Refresh pane state — switchThread re-pulls items, drafts, and
      // checkpoint list. Same pattern as DiffPanelDrawer's handleRevert.
      await pane.switchThread(thread);
    } catch (err) {
      addToast('error', `Revert failed: ${userFacingError(err)}`);
    } finally {
      busy = false;
    }
  }

  async function doFork(): Promise<void> {
    if (busy || !pane.thread) return;
    busy = true;
    try {
      const forked = (await ForkThread(pane.thread.id, userTurnIndex)) as Thread;
      prependThread(forked);
      if (forked.projectId) expandProject(forked.projectId);
      await pane.switchThread(forked);
      addToast('info', 'Forked from this message into a new thread.');
    } catch (err) {
      addToast('error', `Fork failed: ${userFacingError(err)}`);
    } finally {
      busy = false;
    }
  }
</script>

<Popover
  {anchor}
  {open}
  {onClose}
  placement="bottom-end"
  role="menu"
  ariaLabel="Message actions"
>
  {#snippet children()}
    <Menu ariaLabel="Message Actions" {onClose}>
      {#snippet children()}
        <MenuItem
          label="Revert messages"
          disabled={busy}
          onSelect={() => {
            onClose();
            void doRevert('conversation-only');
          }}
        />
        <MenuItem
          label="Revert messages + workspace"
          disabled={busy || !hasWorkspaceChanges}
          title={hasWorkspaceChanges ? undefined : 'No workspace changes since this message'}
          onSelect={() => {
            onClose();
            void doRevert('conversation-and-files');
          }}
        />
        <MenuDivider />
        <MenuItem
          label="Fork from here"
          disabled={busy}
          title="Conversation fork only — files unchanged"
          onSelect={() => {
            onClose();
            void doFork();
          }}
        />
      {/snippet}
    </Menu>
  {/snippet}
</Popover>
