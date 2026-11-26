<script lang="ts">
  // One long text field of a ticket, its description among them: a
  // section with its name, folded or open (`open`, remembered by the
  // pane), the text rendered as Markdown, and an Edit control on its
  // header, so a link in the text still opens on a click. The editor is
  // the Markdown as the ticket holds it; Save sends it with the text it
  // was edited from, so the tracker refuses an edit that would lose a
  // change made since. Ctrl or Cmd+Enter saves and Escape cancels. The
  // text and its base are the ticket's draft (`ticketDraft`), so the
  // editor is open while one is held and comes back with it after a
  // remount; a refusal keeps it.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import TicketMarkdown from './TicketMarkdown.svelte';
  import TicketPin, { type PinControl } from './TicketPin.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { isEnterKey } from '../../utils/localEnter';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { dropTicketDraft, setTicketDraft, ticketDraft, ticketWrite } from '../../stores/trackerTicketWrites.svelte';
  import type { WriteOutcome } from '../../stores/trackerWriteOutcome';

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
    /** The section's pin for the ticket's project; absent where it takes none. */
    pin?: PinControl;
  }

  let { field, title, markdown, editable, open, onFold, onSave, ticketKey, target, fieldName, pin }: Props = $props();

  const pending = $derived(ticketWrite(ticketKey, target)?.pending ?? false);
  const draft = $derived(ticketDraft(ticketKey, target));
  let textarea: HTMLTextAreaElement | undefined = $state();

  /** Opens the editor, as the header's menu row does. */
  export function edit(): void {
    if (!editable || draft) return;
    if (open === false) onFold?.(true);
    setTicketDraft(ticketKey, target, markdown, markdown);
    queueMicrotask(() => textarea?.focus({ preventScroll: true }));
  }

  function cancel(): void {
    dropTicketDraft(ticketKey, target);
  }

  // An applied save drops the draft, which closes the editor.
  async function save(): Promise<void> {
    if (pending || !draft) return;
    await onSave(draft.text, draft.from);
  }

  function onKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (isEnterKey(event) && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (!event.repeat) void save();
    } else if (event.key === 'Escape' && !pending) {
      event.preventDefault();
      event.stopPropagation();
      cancel();
    }
  }
</script>

<section class="flex flex-col gap-1.5" data-testid="ticket-section" data-field={field}>
  <header class="group/pin flex min-w-0 items-center gap-1 text-sm font-medium text-fg">
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
    {#if pin}<TicketPin {pin} testId="ticket-section-pin" />{/if}
    <span class="flex-1"></span>
    {#if editable && !draft && open !== false}
      <button type="button" class="shrink-0 rounded-[var(--radius-control)] px-1.5 text-xs text-fg-muted hover:bg-surface-1 hover:text-fg compact:min-h-9 compact:select-none" data-testid="ticket-section-edit" onclick={edit}>Edit</button>
    {/if}
  </header>
  {#if open !== false}
    {#if draft}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div class="flex flex-col gap-1.5" onkeydown={onKeydown}>
        <textarea
          bind:this={textarea}
          value={draft.text}
          oninput={(event) => setTicketDraft(ticketKey, target, event.currentTarget.value, draft.from)}
          rows="8"
          class="min-h-24 w-full resize-y rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1.5 font-mono text-[0.8125rem] text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          aria-label="{title}, as Markdown"
          data-testid="ticket-section-editor"
          disabled={pending}
        ></textarea>
        <div class="flex items-center gap-1.5">
          <Button size="sm" variant="primary" disabled={pending} testId="ticket-section-save" onclick={() => { void save(); }}>Save</Button>
          <Button size="sm" variant="ghost" disabled={pending} testId="ticket-section-cancel" onclick={cancel}>Cancel</Button>
        </div>
      </div>
    {:else if markdown.trim()}
      <div class="min-w-0 px-1 text-sm" data-testid="ticket-section-text">
        <TicketMarkdown source={markdown} />
      </div>
    {:else}
      <p class="px-1 text-sm text-fg-muted" data-testid="ticket-section-empty">Nothing written yet.</p>
    {/if}
    <TicketWriteNote {ticketKey} {target} {fieldName} />
  {/if}
</section>
