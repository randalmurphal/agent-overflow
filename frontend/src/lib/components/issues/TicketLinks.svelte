<script lang="ts">
  // A ticket's linked tickets by relation (docs/specs/issues.md, "Ticket"):
  // each opens in this ticket's place. A row's menu (also on a right-click
  // or long press of it) opens it, removes the link or copies its key; a
  // removal sends at once, with no question, since Add link puts it back,
  // and the row says so until the ticket's view drops it. Add link opens
  // the link form at the section's header.
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import Link from '@lucide/svelte/icons/link';
  import X from '@lucide/svelte/icons/x';
  import type { LinkView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import StatusMark from './StatusMark.svelte';
  import TicketLinkForm from './TicketLinkForm.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import type { TicketSeed } from '../../stores/issuesPane.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { linkGroups } from '../../stores/trackerTicketModel';
  import { linkTarget, ticketWrite, unlinkTicket, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import { copyToClipboard } from '../../utils/clipboard';

  interface Props {
    ctx: TicketWriteContext;
    links: readonly LinkView[];
    onOpenIssue: (issue: string, seed: TicketSeed) => void;
  }

  let { ctx, links, onOpenIssue }: Props = $props();

  const groups = $derived(linkGroups(links));
  let addButton = $state<HTMLElement | undefined>();
  let adding = $state(false);

  /** Opens the link form, as the ticket's menu does. */
  export function add(): void {
    addButton?.scrollIntoView({ block: 'nearest' });
    adding = true;
  }

  function open(link: LinkView): void {
    onOpenIssue(link.issue.id, { key: link.issue.key, title: link.issue.title, card: null });
  }

  function remove(link: LinkView): void {
    void unlinkTicket(ctx, link);
  }

  async function copyKey(key: string): Promise<void> {
    if (!(await copyToClipboard(key))) addToast('error', 'Could not copy the key.');
  }

  // The row the menu was last opened on stays after it closes, so nothing
  // the closing menu still reads finds it gone.
  let menu = $state<{ x: number; y: number; link: LinkView } | null>(null);
  let menuOpen = $state(false);
  function openMenu(event: MouseEvent, link: LinkView): void {
    event.preventDefault();
    menu = { x: event.clientX, y: event.clientY, link };
    menuOpen = true;
  }
  function openMenuFrom(button: HTMLElement, link: LinkView): void {
    const rect = button.getBoundingClientRect();
    menu = { x: rect.left, y: rect.bottom + 4, link };
    menuOpen = true;
  }
  function run(action: (link: LinkView) => void | Promise<void>): void {
    menuOpen = false;
    if (menu) void action(menu.link);
  }
</script>

<section class="flex flex-col gap-1.5" data-testid="ticket-links">
  <div class="flex items-center gap-1">
    <span class="px-1 text-sm font-medium text-fg">Linked tickets</span>
    <span class="flex-1"></span>
    <span bind:this={addButton} class="inline-flex">
      <IconButton label="Add link" size="sm" testId="ticket-link-add" onClick={() => { adding = true; }}>
        {#snippet children()}<Icon icon={Link} size={13} strokeWidth={2} />{/snippet}
      </IconButton>
    </span>
  </div>
  {#if !adding}
    <TicketWriteNote ticketKey={ctx.key} target="new-link" fieldName={(id) => id} />
  {/if}
  {#each groups as group (group.relation)}
    <div class="flex flex-col" data-testid="ticket-link-group">
      <span class="px-1 text-xs text-fg-muted" data-testid="ticket-link-relation">{group.relation}</span>
      {#each group.links as link (link.id)}
        {@const issue = link.issue}
        {@const removing = ticketWrite(ctx.key, linkTarget(link.id))?.pending ?? false}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div class="group flex min-w-0 items-center gap-0.5" data-testid="ticket-link-row" data-link={link.id} oncontextmenu={(event) => openMenu(event, link)}>
          <button
            type="button"
            class="flex min-w-0 flex-1 items-center gap-1.5 rounded-[var(--radius-control)] px-1 py-0.5 text-left text-sm text-fg hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
            title="Open {issue.key || issue.title}"
            data-testid="ticket-link"
            onclick={() => open(link)}
          >
            <StatusMark group={issue.status.group} />
            <span class="shrink-0 tabular-nums text-fg-muted">{issue.key}</span>
            <span class="min-w-0 truncate">{issue.title}</span>
            {#if removing}<span class="shrink-0 text-xs text-fg-muted" data-testid="ticket-link-removing">Removing…</span>{/if}
          </button>
          <span class="opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 compact:opacity-100">
            <IconButton label="Remove the link to {issue.key || issue.title}" size="xs" testId="ticket-link-remove" disabled={removing} onClick={() => remove(link)}>
              {#snippet children()}<Icon icon={X} size={12} strokeWidth={2} />{/snippet}
            </IconButton>
          </span>
          <IconButton label="Actions for {issue.key || issue.title}" size="xs" testId="ticket-link-menu" ariaHaspopup="menu" onClick={(event) => openMenuFrom(event.currentTarget as HTMLElement, link)}>
            {#snippet children()}<Icon icon={Ellipsis} size={12} strokeWidth={2} />{/snippet}
          </IconButton>
        </div>
        <div class="px-1"><TicketWriteNote ticketKey={ctx.key} target={linkTarget(link.id)} fieldName={(id) => id} /></div>
      {/each}
    </div>
  {:else}
    <p class="px-1 text-sm text-fg-muted" data-testid="ticket-links-none">No linked tickets.</p>
  {/each}
</section>

<Popover anchor={addButton} open={adding} role="dialog" ariaLabel="Link {ctx.label} to a ticket" onClose={() => { adding = false; }}>
  <TicketLinkForm {ctx} onDone={() => { adding = false; }} />
</Popover>

{#if menuOpen && menu}
  {@const link = menu.link}
  {@const removing = ticketWrite(ctx.key, linkTarget(link.id))?.pending ?? false}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Actions for {link.issue.key || link.issue.title}" onDismiss={() => { menuOpen = false; }}>
    <MenuItem label="Open" testId="ticket-link-menu-open" onSelect={() => run(open)} />
    <MenuItem label="Remove link" variant="danger" disabled={removing} testId="ticket-link-menu-remove" onSelect={() => run(remove)} />
    {#if link.issue.key}
      <MenuItem label="Copy key" testId="ticket-link-menu-copy-key" onSelect={() => run((of) => copyKey(of.issue.key))} />
    {/if}
  </ContextMenu>
{/if}
