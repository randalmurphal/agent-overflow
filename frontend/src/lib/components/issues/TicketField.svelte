<script lang="ts" module>
  import type { PickItem, PickList } from './TicketPicker.svelte';

  /** How a field is changed in place: picked from a list, or typed. */
  export type FieldEditor =
    | {
      readonly kind: 'pick';
      readonly load: (text: string) => Promise<PickList>;
      readonly searchable: boolean;
      readonly checked: ReadonlySet<string>;
      readonly clearLabel?: string;
      readonly multi?: boolean;
      readonly onPick: (item: PickItem | null) => void;
    }
    | {
      readonly kind: 'type';
      /** The value as typed text now. */
      readonly text: string;
      readonly input: 'text' | 'number' | 'date';
      /** Saves `text`; empty clears the field. */
      readonly onSave: (text: string) => void;
    };
</script>

<script lang="ts">
  // One field of the ticket: its name, its value, and its editor in
  // place. A value is picked from its list or typed and saved with Enter
  // (Escape puts it back); while its write is out the control waits, so
  // nothing is sent twice. What the write answered shows under it.
  import type { Snippet } from 'svelte';
  import TicketPicker from './TicketPicker.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { ticketWrite } from '../../stores/trackerTicketWrites.svelte';

  interface Props {
    label: string;
    /** The field's ID, as the tests and a refusal name it. */
    field: string;
    /** The value as text; empty shows "None". */
    value: string;
    /** Drawn before the value (a mark). */
    mark?: Snippet;
    editor: FieldEditor | null;
    ticketKey: string;
    target: string;
    fieldName: (id: string) => string;
  }

  let { label, field, value, mark, editor, ticketKey, target, fieldName }: Props = $props();

  const pending = $derived(ticketWrite(ticketKey, target)?.pending ?? false);
  let trigger: HTMLButtonElement | undefined = $state();
  let picking = $state(false);
  let typing = $state(false);
  let draft = $state('');
  let input: HTMLInputElement | undefined = $state();

  /** Opens the field's editor, as its menu row does. */
  export function edit(): void {
    if (!editor || pending) return;
    if (editor.kind === 'pick') picking = true;
    else startTyping();
  }

  function startTyping(): void {
    if (editor?.kind !== 'type') return;
    draft = editor.text;
    typing = true;
    queueMicrotask(() => input?.focus());
  }

  function onKeydown(event: KeyboardEvent): void {
    if (editor?.kind !== 'type') return;
    if (event.key === 'Enter') {
      event.preventDefault();
      typing = false;
      if (draft.trim() !== editor.text) editor.onSave(draft.trim());
    } else if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      typing = false;
    }
  }
</script>

<div class="flex flex-col gap-0.5 py-1" data-testid="ticket-field" data-field={field}>
  <div class="flex min-w-0 items-center gap-2 text-sm">
    <span class="w-24 shrink-0 truncate text-xs text-fg-muted" title={label}>{label}</span>
    {#if typing && editor?.kind === 'type'}
      <input
        bind:this={input}
        bind:value={draft}
        type={editor.input}
        class="min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-1.5 py-0.5 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        aria-label={label}
        data-testid="ticket-field-input"
        onkeydown={onKeydown}
        onblur={() => { typing = false; }}
      />
    {:else if editor}
      <button
        bind:this={trigger}
        type="button"
        class="flex min-w-0 flex-1 items-center gap-1.5 rounded-[var(--radius-control)] px-1.5 py-0.5 text-left text-fg hover:bg-surface-1 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60 compact:min-h-9 compact:select-none"
        aria-haspopup={editor.kind === 'pick' ? 'menu' : undefined}
        aria-label="{label}: {value || 'None'}"
        disabled={pending}
        data-testid="ticket-field-value"
        onclick={edit}
      >
        {@render mark?.()}
        <span class="min-w-0 truncate {value ? '' : 'text-fg-muted'}">{value || 'None'}</span>
      </button>
    {:else}
      <span class="flex min-w-0 flex-1 items-center gap-1.5 px-1.5 text-fg" data-testid="ticket-field-value">
        {@render mark?.()}
        <span class="min-w-0 truncate {value ? '' : 'text-fg-muted'}">{value || 'None'}</span>
      </span>
    {/if}
  </div>
  <div class="pl-26"><TicketWriteNote {ticketKey} {target} {fieldName} /></div>
</div>
{#if editor?.kind === 'pick'}
  <TicketPicker
    anchor={trigger}
    open={picking}
    {label}
    load={editor.load}
    searchable={editor.searchable}
    checked={editor.checked}
    clearLabel={editor.clearLabel}
    multi={editor.multi}
    onPick={editor.onPick}
    onClose={() => { picking = false; }}
  />
{/if}
