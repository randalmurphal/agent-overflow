<script lang="ts">
  // A ticket's comments: oldest or newest first (remembered by the pane),
  // the newest five mounted and the earlier ones behind a control that
  // mounts 20 more a press and reads the tracker's next page once the
  // held ones run out, so a ticket with hundreds of comments mounts a
  // handful. The comment box sits where the order ends and stays in view
  // while the comments are (sticky within this section). A comment's menu
  // (right-click, or a long press under compact) edits, deletes or copies
  // it; a delete asks once in place.
  import Button from '../primitives/Button.svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import TicketMarkdown from './TicketMarkdown.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import type { CommentView, DetailView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { loadEarlierTrackerComments, type EarlierComments } from '../../stores/trackerTicket.svelte';
  import {
    addTicketComment,
    commentTarget,
    deleteTicketComment,
    editTicketComment,
    ticketWrite,
    withdrawTicketLossConsent,
    type TicketWriteContext,
  } from '../../stores/trackerTicketWrites.svelte';
  import { COMMENTS_FIRST, COMMENTS_STEP, commentWindow, heldComments, type CommentOrder } from '../../stores/trackerTicketModel';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import { isEnterKey } from '../../utils/localEnter';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { isMacPlatform } from '../../utils/platform';

  interface Props {
    ctx: TicketWriteContext;
    detail: DetailView;
    earlier: EarlierComments;
    order: CommentOrder;
    onOrder: (order: CommentOrder) => void;
    fieldName: (id: string) => string;
  }

  let { ctx, detail, earlier, order, onOrder, fieldName }: Props = $props();

  let shown = $state(COMMENTS_FIRST);
  const held = $derived(heldComments(detail, earlier.comments));
  const view = $derived(commentWindow(held, shown, order));
  const moreToRead = $derived((earlier.next ?? detail.more_comments ?? '') !== '');
  const revealCount = $derived(Math.min(COMMENTS_STEP, view.hidden));

  async function revealEarlier(): Promise<void> {
    if (view.hidden < COMMENTS_STEP && moreToRead) await loadEarlierTrackerComments(ctx.key);
    shown += COMMENTS_STEP;
  }

  let draft = $state('');
  let box: HTMLTextAreaElement | undefined = $state();
  const boxPending = $derived(ticketWrite(ctx.key, 'new-comment')?.pending ?? false);

  /** Puts the caret in the comment box, as the header's menu row does. */
  export function focusBox(): void {
    box?.focus();
  }

  async function send(): Promise<void> {
    const text = draft.trim();
    if (!text || boxPending) return;
    const outcome = await addTicketComment(ctx, text);
    if (outcome?.kind === 'applied') draft = '';
  }

  function onBoxKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (isEnterKey(event) && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (!event.repeat) void send();
    }
  }

  // The comment being edited, with the body it was edited from.
  let editing = $state<{ id: string; from: string } | null>(null);
  let editDraft = $state('');
  let confirming = $state<string | null>(null);

  /** Closes the comment's editor; a Save anyway it was offered goes with
   * it. */
  function closeEdit(): void {
    if (editing) withdrawTicketLossConsent(ctx.key, commentTarget(editing.id));
    editing = null;
  }

  function startEdit(c: CommentView): void {
    confirming = null;
    closeEdit();
    editing = { id: c.id, from: c.body };
    editDraft = c.body;
  }

  async function saveEdit(): Promise<void> {
    const open = editing;
    if (!open) return;
    const outcome = await editTicketComment(ctx, open.id, editDraft, open.from);
    if (outcome?.kind === 'applied' && editing?.id === open.id) editing = null;
  }

  function onEditKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (isEnterKey(event) && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (!event.repeat) void saveEdit();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      closeEdit();
    }
  }

  async function remove(id: string): Promise<void> {
    confirming = null;
    await deleteTicketComment(ctx, id);
  }

  let menu = $state<{ x: number; y: number; comment: CommentView } | null>(null);
  const menuPending = $derived(menu ? (ticketWrite(ctx.key, commentTarget(menu.comment.id))?.pending ?? false) : false);

  function openMenu(event: MouseEvent, comment: CommentView): void {
    event.preventDefault();
    menu = { x: event.clientX, y: event.clientY, comment };
  }

  // A menu row acts on the comment the menu was opened on, taken before
  // the menu closes.
  function fromMenu(action: (comment: CommentView) => void): void {
    const open = menu;
    menu = null;
    if (open) action(open.comment);
  }

  async function copy(comment: CommentView): Promise<void> {
    if (!(await copyToClipboard(comment.body))) addToast('error', 'Could not copy the comment.');
  }

  const placeholder = `Add a comment (${isMacPlatform() ? '⌘' : 'Ctrl'}+Enter to send)`;
  const earlierError = $derived(earlier.error ? errString(earlier.error) : null);
</script>

{#snippet commentBox()}
  <div class="sticky z-[1] flex flex-col gap-1.5 bg-surface-0 py-2 {order === 'oldest' ? 'bottom-0' : 'top-0'}" data-testid="ticket-comment-box">
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <textarea
      bind:this={box}
      bind:value={draft}
      rows="3"
      class="w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      {placeholder}
      aria-label="New comment"
      data-testid="ticket-comment-input"
      disabled={boxPending}
      onkeydown={onBoxKeydown}
    ></textarea>
    <div class="flex items-center gap-2">
      <Button size="sm" variant="primary" disabled={boxPending || !draft.trim()} testId="ticket-comment-send" onclick={() => { void send(); }}>Comment</Button>
      <div class="min-w-0 flex-1"><TicketWriteNote ticketKey={ctx.key} target="new-comment" {fieldName} /></div>
    </div>
  </div>
{/snippet}

{#snippet reveal()}
  {#if view.hidden > 0 || moreToRead}
    <div class="flex items-center gap-2 py-1">
      <button
        type="button"
        class="text-xs text-accent hover:underline disabled:opacity-60 compact:min-h-9"
        disabled={earlier.loading}
        data-testid="ticket-comments-earlier"
        onclick={() => { void revealEarlier(); }}
      >{earlier.loading ? 'Reading earlier comments…' : revealCount > 0 ? `Show ${revealCount} earlier ${revealCount === 1 ? 'comment' : 'comments'}` : 'Show earlier comments'}</button>
      {#if earlierError}
        <span class="text-xs text-error" role="alert" data-testid="ticket-comments-earlier-error">Could not read earlier comments: {earlierError}</span>
      {/if}
    </div>
  {/if}
{/snippet}

<section class="flex flex-col gap-2" data-testid="ticket-comments">
  <div class="flex items-center gap-2">
    <span class="text-xs text-fg-muted">{held.length}{moreToRead ? '+' : ''} {held.length === 1 && !moreToRead ? 'comment' : 'comments'}</span>
    <span class="flex-1"></span>
    <Segmented
      ariaLabel="Comment order"
      size="sm"
      value={order}
      onChange={onOrder}
      options={[{ value: 'oldest', label: 'Oldest first' }, { value: 'newest', label: 'Newest first' }]}
    />
  </div>
  {#if order === 'newest'}{@render commentBox()}{:else}{@render reveal()}{/if}
  <ol class="flex flex-col gap-3" data-testid="ticket-comment-list">
    {#each view.comments as c (c.id)}
      <li
        class="flex flex-col gap-1 rounded-[var(--radius-field)] border border-border-subtle px-2.5 py-2"
        data-testid="ticket-comment"
        data-comment={c.id}
        oncontextmenu={(event) => openMenu(event, c)}
      >
        <div class="flex min-w-0 items-center gap-1.5 text-xs text-fg-muted">
          <span class="truncate font-medium text-fg">{c.author.name || 'Unknown user'}</span>
          <span class="shrink-0" title={new Date(c.created).toLocaleString()}>{relativeTime(c.created, 'locale', ctx.ref.backend)}</span>
          {#if c.updated && c.updated !== c.created}<span class="shrink-0">edited</span>{/if}
          {#if c.internal}<span class="shrink-0 rounded-sm bg-warning/15 px-1 text-warning">Internal</span>{/if}
        </div>
        {#if editing?.id === c.id}
          {@const pending = ticketWrite(ctx.key, commentTarget(c.id))?.pending ?? false}
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div class="flex flex-col gap-1.5" onkeydown={onEditKeydown}>
            <textarea bind:value={editDraft} oninput={() => withdrawTicketLossConsent(ctx.key, commentTarget(c.id))} rows="4" class="w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40" aria-label="Comment, as Markdown" data-testid="ticket-comment-editor" disabled={pending}></textarea>
            <div class="flex items-center gap-1.5">
              <Button size="sm" variant="primary" disabled={pending} testId="ticket-comment-save" onclick={() => { void saveEdit(); }}>Save</Button>
              <Button size="sm" variant="ghost" disabled={pending} testId="ticket-comment-cancel" onclick={closeEdit}>Cancel</Button>
            </div>
          </div>
        {:else}
          <div class="min-w-0 text-sm" data-testid="ticket-comment-body">
            <TicketMarkdown source={c.body} />
          </div>
        {/if}
        {#if confirming === c.id}
          <div class="flex items-center gap-1.5 text-xs" data-testid="ticket-comment-confirm-delete">
            <span class="text-fg">Delete this comment from the tracker?</span>
            <Button size="xs" variant="danger" testId="ticket-comment-delete" onclick={() => { void remove(c.id); }}>Delete</Button>
            <Button size="xs" variant="ghost" onclick={() => { confirming = null; }}>Keep</Button>
          </div>
        {/if}
        <TicketWriteNote ticketKey={ctx.key} target={commentTarget(c.id)} {fieldName} onApplied={() => { if (editing?.id === c.id) editing = null; }} />
      </li>
    {:else}
      <li class="text-sm text-fg-muted" data-testid="ticket-comments-none">No comments yet.</li>
    {/each}
  </ol>
  {#if order === 'newest'}{@render reveal()}{:else}{@render commentBox()}{/if}
</section>

{#if menu}
  <ContextMenu x={menu.x} y={menu.y} ariaLabel="Comment actions" onDismiss={() => { menu = null; }}>
    <MenuItem label="Edit" disabled={menuPending} testId="ticket-comment-menu-edit" onSelect={() => fromMenu(startEdit)} />
    <MenuItem label="Delete" variant="danger" disabled={menuPending} testId="ticket-comment-menu-delete" onSelect={() => fromMenu((c) => { closeEdit(); confirming = c.id; })} />
    <MenuDivider />
    <MenuItem label="Copy text" testId="ticket-comment-menu-copy" onSelect={() => fromMenu((c) => { void copy(c); })} />
  </ContextMenu>
{/if}
