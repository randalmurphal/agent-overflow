<script lang="ts">
  import Menu from '../primitives/Menu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../primitives/MenuSectionHeader.svelte';
  import MenuSubmenuItem from '../primitives/MenuSubmenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  // The "Open with agent" menu on an MR opened from the page (the toolbar
  // button and each row's menu): a new thread in a linked worktree on the
  // MR's branch, the threads already on this MR, and sending the review's
  // drafts to a thread of the project. Every row names what it does; the
  // page runs it.

  export interface AgentThreadChoice {
    id: string;
    title: string;
    /** "running", "idle · 3d": the thread's live status and age. */
    detail: string;
  }

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: (reason?: PopoverCloseReason) => void;
    /** The branch the new thread checks out: the MR's source branch, or
     * the local `pr/N` branch for an MR from a fork. */
    branch: string;
    crossRepository: boolean;
    /** Threads already tracking this MR. */
    threadsOnPR: readonly AgentThreadChoice[];
    /** The project's other threads, for sending drafts. */
    projectThreads: readonly AgentThreadChoice[];
    draftsCount: number;
    busy?: boolean;
    onNewThread: () => void;
    onOpenThread: (threadId: string) => void;
    onSendDrafts: (threadId: string) => void;
  }

  let {
    anchor,
    open,
    onClose,
    branch,
    crossRepository,
    threadsOnPR,
    projectThreads,
    draftsCount,
    busy = false,
    onNewThread,
    onOpenThread,
    onSendDrafts,
  }: Props = $props();

  const newThreadDescription = $derived(
    crossRepository
      ? `Fetches the fork's head into ${branch} in a linked worktree; the draft opens with the MR linked.`
      : "Checks out the MR's branch in a linked worktree; the draft opens with the MR linked.",
  );
  const sendTargets = $derived([
    ...threadsOnPR,
    ...projectThreads.filter((thread) => !threadsOnPR.some((onPR) => onPR.id === thread.id)),
  ]);
  const sendLabel = $derived(
    draftsCount === 0 ? 'Send review comments to a thread…' : `Send ${draftsCount} review ${draftsCount === 1 ? 'comment' : 'comments'} to a thread…`,
  );
</script>

<Popover {anchor} {open} {onClose} placement="bottom-start" ariaLabel="Open with agent">
  <Menu ariaLabel="Open with agent" onClose={() => onClose()} minWidthClass="min-w-[300px]">
    <MenuItem
      label="New thread on {branch}"
      description={newThreadDescription}
      suffix="worktree"
      disabled={busy || !branch}
      onSelect={onNewThread}
      testId="open-with-agent-new-thread"
    />
    <MenuDivider />
    <MenuSectionHeader label="Threads already on this MR" />
    {#if threadsOnPR.length === 0}
      <MenuItem label="No threads on this MR yet" disabled testId="open-with-agent-no-threads" />
    {:else}
      {#each threadsOnPR as thread (thread.id)}
        <MenuItem label={thread.title} suffix={thread.detail} onSelect={() => onOpenThread(thread.id)} testId="open-with-agent-thread" />
      {/each}
    {/if}
    <MenuDivider />
    <MenuSubmenuItem label={sendLabel} disabled={draftsCount === 0 || sendTargets.length === 0}>
      {#each sendTargets as thread (thread.id)}
        <MenuItem label={thread.title} suffix={thread.detail} onSelect={() => onSendDrafts(thread.id)} testId="open-with-agent-send-target" />
      {/each}
    </MenuSubmenuItem>
  </Menu>
</Popover>
