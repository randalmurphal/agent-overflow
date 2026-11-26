<script lang="ts">
  // A ticket's attachments (docs/specs/issues.md, "Ticket"): a folding
  // section, newest first, each row its name, size, author and date, and a
  // menu (also on a right-click or long press of the row): Open for an
  // image the app previews (`previewsInApp`), Save, Open in the tracker
  // and Copy name. No thumbnail is read.
  // Attach on the header picks files; while files go, one line says which
  // one and its place ("Uploading report.pdf (4.1 MB)… 2 of 3"), and each
  // file that was not attached plainly keeps what became of it until
  // dismissed. A tracker that stores no files (Linear) offers no Attach.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import Paperclip from '@lucide/svelte/icons/paperclip';
  import type { AttachmentView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { ticketWriteSurface } from './ticketWriteSurface';
  import { openImageLightbox } from '../../stores/imageLightbox.svelte';
  import { previewsInApp } from '../../stores/trackerTicketModel';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import {
    attachTicketFiles,
    drawTicketWrite,
    ticketAttachOutcomes,
    ticketUploads,
    type TicketWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import { UploadNotSentError } from '../../transport/attachmentTransfer';
  import { TransportError } from '../../transport/wsClient';
  import { copyToClipboard } from '../../utils/clipboard';
  import { errString } from '../../utils/errors';
  import { handleExternalURL, safeExternalURL } from '../../utils/externalLinks';
  import { relativeTime } from '../../utils/format';
  import { formatBytes } from '../../utils/formatBytes';
  import {
    saveTrackerAttachment,
    trackerAttachmentSave,
    trackerAttachmentSaveLabel,
    trackerMarkdownImageSource,
    type TrackerAttachmentTicket,
  } from '../../utils/trackerAttachments';

  interface Props {
    ctx: TicketWriteContext;
    ticket: TrackerAttachmentTicket;
    attachments: readonly AttachmentView[];
    /** The tracker stores files: Attach is offered. */
    canAttach: boolean;
    open: boolean;
    onFold: (open: boolean) => void;
  }

  let { ctx, ticket, attachments, canAttach, open, onFold }: Props = $props();

  const newest = $derived([...attachments].sort((a, b) => b.created - a.created));
  const uploading = $derived(ticketUploads(ctx.key));
  // The row of the file being sent draws its write.
  const shown = ticketWriteSurface();
  $effect(() => {
    const target = uploading?.target;
    if (target) return drawTicketWrite(ctx.key, target, shown);
  });
  const outcomes = $derived(ticketAttachOutcomes(ctx.key));
  const trackerURL = $derived(safeExternalURL(ticket.url));

  let fileInput: HTMLInputElement | undefined = $state();

  /** Opens the file picker, the section first. */
  export function pick(): void {
    if (!open) onFold(true);
    fileInput?.click();
  }

  function picked(event: Event): void {
    const input = event.currentTarget as HTMLInputElement;
    const files = Array.from(input.files ?? []);
    input.value = '';
    if (!open) onFold(true);
    attachTicketFiles(ctx, files);
  }

  // A mint that refused the file before any byte crossed: the tracker
  // cannot take it here, and the tracker's own page may.
  const FILE_REFUSALS: ReadonlySet<string> = new Set(['tracker_attachment_too_large', 'tracker_attachments_unsupported']);
  function refusedByTracker(error: unknown): boolean {
    const mint = error instanceof UploadNotSentError ? error.cause : null;
    return mint instanceof TransportError && FILE_REFUSALS.has(mint.code);
  }

  async function openImage(a: AttachmentView): Promise<void> {
    const source = trackerMarkdownImageSource(ticket, a);
    const hold = source.acquire(0);
    try {
      const value = await hold.value;
      openImageLightbox({
        images: [{ id: source.key, filename: a.name, mimeType: value.mimeType, url: value.url, width: 0, height: 0, originalBytes: a.size, menuTag: source.menuTag }],
        index: 0,
        dispose: () => hold.release(),
      });
    } catch (err) {
      hold.release();
      addErrorToast(`Could not open ${a.name}: ${errString(err)}`, err);
    }
  }

  async function openInTracker(): Promise<void> {
    if (!trackerURL || !(await handleExternalURL(trackerURL))) addToast('error', `Could not open ${ctx.label} in the tracker: it has no web address.`);
  }

  async function copyName(name: string): Promise<void> {
    if (!(await copyToClipboard(name))) addToast('error', 'Could not copy the name.');
  }

  // The row the menu was last opened on stays after it closes, so nothing
  // the closing menu still reads finds it gone.
  let menu = $state<{ x: number; y: number; attachment: AttachmentView } | null>(null);
  let menuOpen = $state(false);
  function openMenu(event: MouseEvent, attachment: AttachmentView): void {
    event.preventDefault();
    menu = { x: event.clientX, y: event.clientY, attachment };
    menuOpen = true;
  }
  function openMenuFrom(button: HTMLElement, attachment: AttachmentView): void {
    const rect = button.getBoundingClientRect();
    menu = { x: rect.left, y: rect.bottom + 4, attachment };
    menuOpen = true;
  }
  function run(action: (a: AttachmentView) => void | Promise<void>): void {
    menuOpen = false;
    if (menu) void action(menu.attachment);
  }
</script>

<section class="flex flex-col gap-1.5" data-testid="ticket-attachments">
  <div class="flex items-center gap-1">
    <button
      type="button"
      class="inline-flex min-w-0 items-center gap-1 rounded-[var(--radius-control)] px-1 text-sm font-medium text-fg hover:bg-surface-1 compact:min-h-9 compact:select-none"
      aria-expanded={open}
      data-testid="ticket-attachments-fold"
      onclick={() => onFold(!open)}
    >
      <Icon icon={open ? ChevronDown : ChevronRight} size={13} strokeWidth={2} />
      Attachments
      {#if attachments.length > 0}<span class="text-xs font-normal text-fg-muted">{attachments.length}</span>{/if}
    </button>
    <span class="flex-1"></span>
    {#if canAttach}
      <IconButton label="Attach files" size="sm" testId="ticket-attach" onClick={pick}>
        {#snippet children()}<Icon icon={Paperclip} size={13} strokeWidth={2} />{/snippet}
      </IconButton>
      <input bind:this={fileInput} type="file" multiple hidden aria-label="Choose files to attach" data-testid="ticket-attach-input" onchange={picked} />
    {/if}
  </div>
  {#if uploading}
    <p class="flex items-center gap-1.5 px-1 text-xs text-fg-muted" role="status" data-testid="ticket-uploading">
      <SteppedSpinner size={12} />
      <span class="min-w-0 truncate">Uploading {uploading.name} ({formatBytes(uploading.size)})…{uploading.total > 1 ? ` ${uploading.index} of ${uploading.total}` : ''}</span>
    </p>
  {/if}
  {#each outcomes as o (o.target)}
    <div class="flex flex-col px-1" data-testid="ticket-attach-outcome" data-name={o.name}>
      <span class="text-xs font-medium text-fg">{o.name}</span>
      <TicketWriteNote ticketKey={ctx.key} target={o.target} fieldName={(id) => id} />
      {#if o.outcome.kind === 'failed' && refusedByTracker(o.outcome.error) && trackerURL}
        <button type="button" class="self-start text-xs text-accent hover:underline" data-testid="ticket-attach-open-tracker" onclick={() => { void openInTracker(); }}>Open in the tracker</button>
      {/if}
    </div>
  {/each}
  {#if open}
    <ul class="flex flex-col">
      {#each newest as a (a.id)}
        <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
        <li
          class="group flex min-w-0 items-center gap-2 rounded-[var(--radius-control)] px-1 py-0.5 text-sm hover:bg-surface-1 compact:min-h-9 compact:select-none"
          data-testid="ticket-attachment"
          data-name={a.name}
          oncontextmenu={(event) => openMenu(event, a)}
        >
          <span class="min-w-0 flex-1 truncate text-fg" title={a.name}>{a.name}</span>
          <span class="shrink-0 text-xs tabular-nums text-fg-muted">{formatBytes(a.size)}</span>
          <span class="min-w-0 max-w-[40%] shrink truncate text-xs text-fg-muted" data-testid="ticket-attachment-author">{a.author?.name || 'Unknown user'}</span>
          <span class="shrink-0 text-xs text-fg-muted" title={new Date(a.created).toLocaleString()}>{relativeTime(a.created, 'locale', ctx.ref.backend)}</span>
          <IconButton label="Actions for {a.name}" size="xs" testId="ticket-attachment-menu" ariaHaspopup="menu" onClick={(event) => openMenuFrom(event.currentTarget as HTMLElement, a)}>
            {#snippet children()}<Icon icon={Ellipsis} size={12} strokeWidth={2} />{/snippet}
          </IconButton>
        </li>
      {:else}
        <li class="px-1 text-sm text-fg-muted">No attachments.</li>
      {/each}
    </ul>
  {/if}
</section>

{#if menuOpen && menu}
  {@const a = menu.attachment}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Actions for {a.name}" onDismiss={() => { menuOpen = false; }}>
    {#if previewsInApp(a)}
      <MenuItem label="Open" testId="ticket-attachment-open" onSelect={() => run(openImage)} />
    {/if}
    {@const save = trackerAttachmentSave(ticket, a)}
    <MenuItem label={trackerAttachmentSaveLabel(save)} testId="ticket-attachment-save" onSelect={() => run((of) => saveTrackerAttachment(ticket, of))} />
    <!-- Save that opens the tracker is already this row. -->
    {#if trackerURL && save !== 'tracker'}
      <MenuItem label="Open in the tracker" testId="ticket-attachment-open-tracker" onSelect={() => run(openInTracker)} />
    {/if}
    <MenuItem label="Copy name" testId="ticket-attachment-copy-name" onSelect={() => run((of) => copyName(of.name))} />
  </ContextMenu>
{/if}
