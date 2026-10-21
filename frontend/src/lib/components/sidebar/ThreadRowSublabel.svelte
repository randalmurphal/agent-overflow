<script lang="ts">
  // The line under a thread row naming its worktree and, on a page with
  // several computers, the machine it is on. Nothing renders when the
  // thread has neither.
  import type { Thread } from '../../types/models';
  import {
    attachedBackendEntry,
    backendDisplayName,
    hasMultipleBackends,
    threadMachine,
  } from '../../stores/attachedBackends.svelte';
  import { backendHasBrowser } from '../../utils/browserTools';
  import { hasScope } from '../../transport/scopes';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { pathBasename } from '../../utils/pathDisplay';
  import MonitorIcon from '@lucide/svelte/icons/monitor';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    thread: Thread;
    /** Left edge of the line, in px: the row's padding plus a small offset. */
    indentPx: number;
  }

  let { thread, indentPx }: Props = $props();

  let worktreeName = $derived(pathBasename(thread.worktreePath));
  // Remote-only projects need the same host label as merged projects.
  // On a client with several hosts and no local backend, every host is named.
  let machineName = $derived.by(() => {
    if (!hasMultipleBackends()) return '';
    const key = threadMachine(thread.id, thread.projectId);
    if (key === HOME_BACKEND && hasScope('host')) return '';
    const entry = attachedBackendEntry(key);
    return entry ? backendDisplayName(entry) : '';
  });
  // An agent on a machine with no browser tools cannot open a page at all.
  // Said on the chip that already names the machine rather than on a row of
  // its own: it is a property of that machine, and the chip is where the
  // reader is already looking to find out which one this thread is on.
  let machineTitle = $derived(
    machineName === ''
      ? ''
      : backendHasBrowser(threadMachine(thread.id, thread.projectId))
        ? `Computer: ${machineName}`
        : `Computer: ${machineName}. No browser on this computer.`,
  );
  let show = $derived(Boolean(thread.worktreePath && worktreeName) || machineName !== '');
  const rightPaddingPx = 52;
</script>

{#if show}
  <div
    class="relative -mt-1.5 flex h-3.5 items-center text-[0.625rem] leading-none text-fg-hint"
    style="padding-left: {indentPx}px; padding-right: {rightPaddingPx}px"
    title={worktreeName ? `Worktree: ${thread.worktreePath}` : machineTitle}
    aria-label={worktreeName ? `Worktree ${worktreeName}` : `Computer ${machineName}`}
    data-testid="thread-row-worktree"
  >
    {#if machineName}
      <span
        class="inline-flex min-w-0 max-w-full shrink-0 items-center gap-1 px-1 py-0 text-fg-hint"
        data-testid="thread-row-machine"
        title={machineTitle}
      >
        <Icon icon={MonitorIcon} size={10} strokeWidth={1.8} class="shrink-0 opacity-85" />
        <span class="min-w-0 truncate text-[0.625rem]" data-testid="thread-row-machine-name">
          {machineName}
        </span>
      </span>
    {/if}
    {#if worktreeName}
      <span
        class="inline-flex min-w-0 max-w-full items-center gap-1 px-1 py-0 text-fg-hint"
        data-testid="thread-row-worktree-label"
      >
        <Icon icon={FolderGit2} size={10} strokeWidth={1.8} class="shrink-0 opacity-85" />
        <span
          class="min-w-0 truncate font-mono text-[0.625rem]"
          data-testid="thread-row-worktree-name"
        >
          {worktreeName}
        </span>
      </span>
    {/if}
  </div>
{/if}
