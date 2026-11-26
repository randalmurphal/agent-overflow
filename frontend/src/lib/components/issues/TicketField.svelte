<script lang="ts" module>
  import type { PickItem, PickList } from './TicketPicker.svelte';

  /** How a field is changed in place: picked from a list, or typed. */
  export type FieldEditor =
    | {
      readonly kind: 'pick';
      readonly load: (text: string) => Promise<PickList>;
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
      /** Where the text being typed is held when it must outlive the field
       * (a form's foot leaving the virtual window); absent, it lives with
       * the field. */
      readonly typing?: TypingHold;
    };

  /** The text being typed in a field, held by its form: null while the
   * field is not being typed in. */
  export interface TypingHold {
    readonly text: string | null;
    set(text: string | null): void;
  }
</script>

<script lang="ts">
  // One field of the ticket: its name, its value, and its editor in
  // place. A value is picked from its list or typed and saved with Enter
  // (Escape puts it back); while its write is out the control waits, so
  // nothing is sent twice. What the write answered shows under it.
  //
  // A typed value is typed through TicketTypedValue, which says what the
  // field takes when its text cannot be held.
  //
  // `inline` is a field of the narrow ticket's one row: its value alone
  // (the label when empty), and its write's note left to the row.
  //
  // `disabled` is a field of a form whose write is out (a move's form):
  // it takes no change, and an open picker or typed value closes, so what
  // the form shows is what was sent.
  import { untrack, type Snippet } from 'svelte';
  import TicketPicker from './TicketPicker.svelte';
  import TicketTypedValue from './TicketTypedValue.svelte';
  import TicketPin, { type PinControl } from './TicketPin.svelte';
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
    inline?: boolean;
    disabled?: boolean;
    /** The field's pin for the ticket's project; absent where it takes none. */
    pin?: PinControl;
  }

  let { label, field, value, mark, editor, ticketKey, target, fieldName, inline = false, disabled = false, pin }: Props = $props();
  const empty = $derived(inline ? label : 'None');

  const pending = $derived(disabled || (ticketWrite(ticketKey, target)?.pending ?? false));
  let trigger: HTMLButtonElement | undefined = $state();
  let picking = $state(false);
  // A field typed in when it went is typed in again where it was, taking
  // no focus: focus follows a person's act to edit.
  // svelte-ignore state_referenced_locally
  let typing = $state(editor?.kind === 'type' && editor.typing?.text != null);
  // svelte-ignore state_referenced_locally
  let resumed = $state(typing);
  let invalid = $state('');
  $effect(() => {
    if (!disabled) return;
    picking = false;
    untrack(doneTyping);
  });

  /** Opens the field's editor, as its menu row does. */
  export function edit(): void {
    if (!editor || pending) return;
    if (editor.kind === 'pick') picking = true;
    else {
      editor.typing?.set(editor.text);
      resumed = false;
      typing = true;
    }
  }

  function doneTyping(): void {
    typing = false;
    if (editor?.kind === 'type') editor.typing?.set(null);
  }
</script>

<div class="group/pin flex min-w-0 flex-col gap-0.5 {inline ? 'flex-1' : 'py-1'}" data-testid="ticket-field" data-field={field} title={inline ? label : undefined}>
  <div class="flex min-w-0 items-center gap-2 text-sm">
    {#if !inline}
      <span class="w-24 shrink-0 truncate text-xs text-fg-muted" title={label}>{label}</span>
    {/if}
    {#if typing && editor?.kind === 'type'}
      <TicketTypedValue {label} input={editor.input} text={editor.text} typed={editor.typing?.text ?? undefined} onTyped={editor.typing ? (t) => editor.typing?.set(t) : undefined} {resumed} onSave={editor.onSave} onDone={doneTyping} bind:invalid />
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
        <span class="min-w-0 truncate {value ? '' : 'text-fg-muted'}">{value || empty}</span>
      </button>
    {:else}
      <span class="flex min-w-0 flex-1 items-center gap-1.5 px-1.5 text-fg" data-testid="ticket-field-value">
        {@render mark?.()}
        <span class="min-w-0 truncate {value ? '' : 'text-fg-muted'}">{value || empty}</span>
      </span>
    {/if}
    {#if pin}<TicketPin {pin} testId="ticket-field-pin" />{/if}
  </div>
  {#if typing && invalid}
    <p class="text-xs text-error {inline ? '' : 'pl-26'}" role="alert" data-testid="ticket-field-invalid">{invalid}</p>
  {/if}
  {#if !inline}
    <div class="pl-26"><TicketWriteNote {ticketKey} {target} {fieldName} /></div>
  {/if}
</div>
{#if editor?.kind === 'pick'}
  <TicketPicker
    anchor={trigger}
    open={picking}
    {label}
    load={editor.load}
    checked={editor.checked}
    clearLabel={editor.clearLabel}
    multi={editor.multi}
    onPick={editor.onPick}
    onClose={() => { picking = false; }}
  />
{/if}
