<script lang="ts">
  // The line under a thread row naming its worktree, on a page with
  // several computers the machine it is on, and the status mark and key of
  // its linked ticket (docs/specs/issues.md, "Thread link"). The key is
  // left out when the worktree's name already holds it. Nothing renders
  // when the thread has none of the three.
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
  import StatusMark from '../issues/StatusMark.svelte';
  import { linkedTicketStateNote } from '../../stores/threadTicketLinks.svelte';
  import { linkMarkGroup } from '../../stores/threadLinkMarks.svelte';

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
  let link = $derived(thread.ticketLink ?? null);
  // The mockup's rule: a worktree named after its ticket already shows the
  // key.
  let ticketShown = $derived(link !== null && !(worktreeName && worktreeName.toLowerCase().includes(link.key.toLowerCase())));
  let ticketTitle = $derived(link ? [`${link.key} · ${link.title}`, linkedTicketStateNote(link)].filter(Boolean).join('\n') : '');
  let show = $derived(Boolean(thread.worktreePath && worktreeName) || machineName !== '' || ticketShown);
  const rightPaddingPx = 52;
</script>

{#if show}
  <div
    class="thread-row-line relative flex h-3.5 items-center text-[0.625rem] leading-none text-fg-hint"
    style="padding-left: {indentPx}px; padding-right: {rightPaddingPx}px"
    title={worktreeName ? `Worktree: ${thread.worktreePath}` : machineName ? machineTitle : ticketTitle}
    aria-label={worktreeName ? `Worktree ${worktreeName}` : machineName ? `Computer ${machineName}` : `Ticket ${link?.key ?? ''}`}
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
    {#if ticketShown && link}
      {#if worktreeName || machineName}<span class="shrink-0 opacity-50" aria-hidden="true">·</span>{/if}
      <span
        class="inline-flex shrink-0 items-center gap-1 px-1 py-0 font-mono text-[0.625rem] text-fg-muted"
        title={ticketTitle}
        data-testid="thread-row-ticket"
        data-link-state={link.state}
      >
        <span class={link.state === 'current' ? '' : 'opacity-50'}><StatusMark group={linkMarkGroup(thread)} size={9} /></span>
        {link.key}
      </span>
    {/if}
  </div>
{/if}
