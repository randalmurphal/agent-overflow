<script lang="ts">
  // One long text field of a ticket, its description among them: a
  // section with its name, folded or open (`open`, remembered by the
  // pane), the text rendered as Markdown, and an Edit control on its
  // header, so a link in the text still opens on a click. The editor is
  // the Markdown as the ticket holds it; Save sends it with the text it
  // was edited from, so the tracker refuses an edit that would lose a
  // change made since. Ctrl or Cmd+Enter saves and Escape cancels. A
  // refusal keeps the editor and its text.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { isEnterKey } from '../../utils/localEnter';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { ticketWrite } from '../../stores/trackerTicketWrites.svelte';
  import type { WriteOutcome } from '../../stores/trackerTicketModel';

  interface Props {
    /** The field's ID. */
    field: string;
    title: string;
    markdown: string;
    editable: boolean;
    /** Null for a section that does not fold. */
    open: boolean | null;
    onFold?: (open: boolean) => void;
    onSave: (markdown: string, from: string) => Promise<WriteOutcome | null>;
    ticketKey: string;
    target: string;
    fieldName: (id: string) => string;
  }

  let { field, title, markdown, editable, open, onFold, onSave, ticketKey, target, fieldName }: Props = $props();

  const pending = $derived(ticketWrite(ticketKey, target)?.pending ?? false);
  let editing = $state(false);
  let draft = $state('');
  let from = '';
  let textarea: HTMLTextAreaElement | undefined = $state();

  /** Opens the editor, as the header's menu row does. */
  export function edit(): void {
    if (!editable || editing) return;
    if (open === false) onFold?.(true);
    from = markdown;
    draft = markdown;
    editing = true;
    queueMicrotask(() => textarea?.focus({ preventScroll: true }));
  }

  async function save(): Promise<void> {
    if (pending) return;
    if ((await onSave(draft, from))?.kind === 'applied') editing = false;
  }

  function onKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (isEnterKey(event) && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (!event.repeat) void save();
    } else if (event.key === 'Escape' && !pending) {
      event.preventDefault();
      event.stopPropagation();
      editing = false;
    }
  }
</script>

<section class="flex flex-col gap-1.5" data-testid="ticket-section" data-field={field}>
  <header class="flex min-w-0 items-center gap-1 text-sm font-medium text-fg">
    {#if open !== null}
      <button
        type="button"
        class="inline-flex min-w-0 items-center gap-1 rounded-[var(--radius-control)] px-1 hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
        aria-expanded={open}
        data-testid="ticket-section-fold"
        onclick={() => onFold?.(!open)}
      >
        <Icon icon={open ? ChevronDown : ChevronRight} size={13} strokeWidth={2} />
        <span class="truncate">{title}</span>
      </button>
    {:else}
      <span class="truncate px-1">{title}</span>
    {/if}
    <span class="flex-1"></span>
    {#if editable && !editing && open !== false}
      <button type="button" class="shrink-0 rounded-[var(--radius-control)] px-1.5 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg compact:min-h-9 compact:select-none" data-testid="ticket-section-edit" onclick={edit}>Edit</button>
    {/if}
  </header>
  {#if open !== false}
    {#if editing}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div class="flex flex-col gap-1.5" onkeydown={onKeydown}>
        <textarea
          bind:this={textarea}
          bind:value={draft}
          rows="8"
          class="min-h-24 w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1.5 font-mono text-[0.8125rem] text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          aria-label="{title}, as Markdown"
          data-testid="ticket-section-editor"
          disabled={pending}
        ></textarea>
        <div class="flex items-center gap-1.5">
          <Button size="sm" variant="primary" disabled={pending} testId="ticket-section-save" onclick={() => { void save(); }}>Save</Button>
          <Button size="sm" variant="ghost" disabled={pending} testId="ticket-section-cancel" onclick={() => { editing = false; }}>Cancel</Button>
        </div>
      </div>
    {:else if markdown.trim()}
      <div class="min-w-0 px-1 text-sm" data-testid="ticket-section-text">
        <ChatMarkdown source={markdown} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
      </div>
    {:else}
      <p class="px-1 text-sm text-fg-muted" data-testid="ticket-section-empty">Nothing written yet.</p>
    {/if}
    <TicketWriteNote {ticketKey} {target} {fieldName} onApplied={() => { editing = false; }} />
  {/if}
</section>
