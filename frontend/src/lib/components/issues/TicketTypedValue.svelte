<script lang="ts">
  // A ticket value typed in place: Enter saves the trimmed text when it
  // changed (empty clears the field), Escape and leaving put it back
  // (`onDone`). A number or date input whose text is not a number or not a
  // whole date holds an empty value, as a cleared one does; Enter on it
  // saves nothing and says what the field takes (`invalid`), so a typo
  // never clears the field. The ticket's field rows and a Board card's
  // estimate both type through it.
  import { isLocalEnter } from '../../utils/localEnter';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  interface Props {
    label: string;
    input: 'text' | 'number' | 'date';
    /** The value as typed text now. */
    text: string;
    onSave: (text: string) => void;
    /** Typing ended: saved, put back or left. */
    onDone: () => void;
    /** What the field takes, after Enter on text it cannot hold; empty
     * otherwise. */
    invalid?: string;
  }

  let { label, input, text, onSave, onDone, invalid = $bindable('') }: Props = $props();

  // svelte-ignore state_referenced_locally
  let draft = $state(text);
  let el: HTMLInputElement | undefined = $state();
  $effect(() => {
    invalid = '';
    el?.focus();
  });

  const TAKES: Readonly<Record<'number' | 'date', string>> = {
    number: 'Type a number, or clear the field.',
    date: 'Pick a whole date, or clear the field.',
  };

  function onKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event)) return;
    if (isLocalEnter(event)) {
      event.preventDefault();
      if (input !== 'text' && el?.validity.badInput) {
        invalid = TAKES[input];
        return;
      }
      // Read before `onDone`, which may close the editor that gave them.
      const next = draft.trim();
      const changed = next !== text;
      const save = onSave;
      onDone();
      if (changed) save(next);
    } else if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      onDone();
    }
  }
</script>

<input
  bind:this={el}
  value={draft}
  type={input}
  class="min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-1.5 py-0.5 text-sm text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  aria-label={label}
  data-testid="ticket-field-input"
  aria-invalid={invalid ? true : undefined}
  oninput={(event) => { draft = event.currentTarget.value; invalid = ''; }}
  onkeydown={onKeydown}
  onblur={onDone}
/>
