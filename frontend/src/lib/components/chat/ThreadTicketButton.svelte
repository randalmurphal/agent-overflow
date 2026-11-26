<script lang="ts">
  // The thread header's ticket (docs/specs/issues.md, "Thread link"): the
  // linked ticket's status mark and key, before the pull request badge, on
  // the desktop. A click opens the ticket in the Issues pane; a right-click
  // opens its menu. Under compact the ticket is a row of the Thread actions
  // menu instead (ChatHeaderMenu). Every action is stores/threadTicketLinks.svelte.ts's.
  import type { Thread } from '../../types/models';
  import { threadScopeGrant } from '../../transport/entityScopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import {
    copyLinkedTicketKey,
    findThreadTicket,
    linkedTicketStateNote,
    openLinkedTicket,
    removeThreadTicketLink,
    threadTicketLinkGrant,
  } from '../../stores/threadTicketLinks.svelte';
  import { buttonClasses } from '../primitives/buttonClasses';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import StatusMark from '../issues/StatusMark.svelte';
  import { holdThreadLinkProject, linkMarkGroup } from '../../stores/threadLinkMarks.svelte';

  let { thread }: { thread: Thread } = $props();

  const link = $derived(thread.ticketLink ?? null);
  const note = $derived(link ? linkedTicketStateNote(link) : '');
  const title = $derived(link ? [`${link.key} · ${link.title}`, note, 'Open it in Issues'].filter(Boolean).join('\n') : '');
  // ClearThreadTicket rides threads:operate on the thread's computer.
  const operateGrant = $derived(threadScopeGrant('threads:operate', thread.id, thread.projectId));
  const linkGrant = $derived(threadTicketLinkGrant(thread));
  // The mark reads the thread's project's status groups, held while the
  // link shows.
  $effect(() => holdThreadLinkProject(link ? thread.projectId : null));

  let buttonEl: HTMLButtonElement | undefined = $state();
  let menu = $state<{ x: number; y: number } | null>(null);

  function run(action: () => void): void {
    menu = null;
    action();
  }
</script>

{#if link}
  <button
    bind:this={buttonEl}
    type="button"
    {title}
    aria-label="Open ticket {link.key}"
    data-testid="chat-header-ticket"
    data-link-state={link.state}
    onclick={() => void openLinkedTicket(thread)}
    oncontextmenu={(event) => {
      event.preventDefault();
      menu = { x: event.clientX, y: event.clientY };
    }}
    class={buttonClasses({ variant: 'secondary', size: 'xs', class: 'shrink-0 gap-1.5 px-2 font-mono text-[0.6875rem]' })}
  >
    <span class={link.state === 'current' ? '' : 'opacity-50'}><StatusMark group={linkMarkGroup(thread)} size={10} /></span>
    {link.key}
  </button>
{/if}

{#if menu && link}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Ticket {link.key}" onDismiss={() => { menu = null; }}>
    <MenuItem label="Open ticket" testId="chat-header-ticket-open" onSelect={() => run(() => void openLinkedTicket(thread))} />
    <MenuDivider />
    <MenuItem label="Copy key" testId="chat-header-ticket-copy-key" onSelect={() => run(() => void copyLinkedTicketKey(thread))} />
    <MenuDivider />
    <MenuItem
      label="Change ticket…"
      testId="chat-header-ticket-change"
      disabled={linkGrant !== 'granted'}
      title={grantBlockedTitle(linkGrant)}
      onSelect={() => run(() => findThreadTicket(thread, buttonEl))}
    />
    <MenuItem
      label="Remove link"
      testId="chat-header-ticket-remove"
      disabled={operateGrant !== 'granted'}
      title={grantBlockedTitle(operateGrant)}
      onSelect={() => run(() => void removeThreadTicketLink(thread))}
    />
  </ContextMenu>
{/if}
