<script lang="ts">
  /*
   * Shared text input primitive. Standardizes focus ring, disabled
   * state, and optional label/description so settings forms and
   * inline editors stop redeclaring the same Tailwind string.
   *
   * Supports <input> and <textarea> via the `multiline` prop. The
   * typed value stays a string — callers wanting numbers cast at
   * bind-time.
   */

  interface Props {
    value: string;
    placeholder?: string;
    disabled?: boolean;
    readonly?: boolean;
    multiline?: boolean;
    rows?: number;
    type?: 'text' | 'email' | 'password' | 'search' | 'url';
    label?: string;
    description?: string;
    error?: string;
    id?: string;
    name?: string;
    autocomplete?: HTMLInputElement['autocomplete'];
    class?: string;
    oninput?: (e: Event) => void;
    onkeydown?: (e: KeyboardEvent) => void;
    onfocus?: (e: FocusEvent) => void;
    onblur?: (e: FocusEvent) => void;
  }

  let {
    value = $bindable(''),
    placeholder,
    disabled = false,
    readonly = false,
    multiline = false,
    rows = 3,
    type = 'text',
    label,
    description,
    error,
    id,
    name,
    autocomplete,
    class: className = '',
    oninput,
    onkeydown,
    onfocus,
    onblur,
  }: Props = $props();

  // Generate a stable id when none is provided so label+input stay
  // associated even when the parent doesn't care. crypto.randomUUID is
  // available under happy-dom, so tests don't need a polyfill. We
  // compute the generated id once so later prop changes don't rotate
  // the association.
  const generatedId = `input-${crypto.randomUUID().slice(0, 8)}`;
  const resolvedId = $derived(id ?? generatedId);

  const BASE =
    'w-full rounded-[var(--radius-control)] border bg-surface-0 ' +
    'text-text-primary placeholder:text-fg-hint ' +
    'transition-[color,border-color,background-color] duration-150 ' +
    'focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 ' +
    'disabled:opacity-50 disabled:cursor-not-allowed';

  const PAD_INPUT = 'px-3 py-1.5 text-sm';
  const PAD_TEXTAREA = 'px-3 py-2 text-sm resize-none';

  const borderClass = $derived(
    error ? 'border-error/70 focus:border-error' : 'border-border-subtle focus:border-accent',
  );
</script>

<div class="flex flex-col gap-1 {className}">
  {#if label}
    <label for={resolvedId} class="text-[0.75rem] font-medium text-fg">
      {label}
    </label>
  {/if}
  {#if description}
    <p class="text-[0.75rem] text-fg-muted">{description}</p>
  {/if}
  {#if multiline}
    <textarea
      id={resolvedId}
      {name}
      {placeholder}
      {disabled}
      {readonly}
      {rows}
      bind:value
      {oninput}
      {onkeydown}
      {onfocus}
      {onblur}
      class={[BASE, PAD_TEXTAREA, borderClass].join(' ')}
    ></textarea>
  {:else}
    <input
      id={resolvedId}
      {name}
      {type}
      {placeholder}
      {disabled}
      {readonly}
      {autocomplete}
      bind:value
      {oninput}
      {onkeydown}
      {onfocus}
      {onblur}
      class={[BASE, PAD_INPUT, borderClass].join(' ')}
    />
  {/if}
  {#if error}
    <p class="text-[0.6875rem] text-error">{error}</p>
  {/if}
</div>
