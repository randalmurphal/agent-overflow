<script lang="ts">
  // An editable list of single-line strings: add, edit in place, remove.
  //
  // Rows are addressed by index rather than by value because duplicates and
  // blanks are legal intermediate states while typing — keying on the value
  // would remount a row on every keystroke and lose the caret.

  import { GHOST_BUTTON_CLASS, INPUT_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  let {
    label,
    hint,
    placeholder = '',
    testid,
    addLabel = 'Add',
    disabled = false,
    errors = [],
    values = $bindable<string[]>([]),
  }: {
    label: string;
    hint?: string;
    placeholder?: string;
    testid: string;
    addLabel?: string;
    disabled?: boolean;
    errors?: (string | null)[];
    values: string[];
  } = $props();

  function setValue(index: number, next: string): void {
    values = values.map((value, i) => (i === index ? next : value));
  }

  function removeValue(index: number): void {
    values = values.filter((_, i) => i !== index);
  }
</script>

<div class="flex flex-col gap-2" data-testid={testid}>
  <div class="flex flex-col gap-0.5">
    <p class="text-[0.75rem] font-medium text-fg">{label}</p>
    {#if hint}
      <p class="text-[0.71875rem] leading-snug text-fg-muted">{hint}</p>
    {/if}
  </div>

  {#if values.length > 0}
    <ul class="flex flex-col gap-1.5">
      {#each values as value, index (index)}
        <li class="flex flex-col gap-1">
          <div class="flex items-center gap-2">
            <input
              type="text"
              data-testid="{testid}-input-{index}"
              value={value}
              {placeholder}
              autocomplete="off"
              spellcheck="false"
              {disabled}
              aria-label="{label} {index + 1}"
              aria-invalid={errors[index] != null}
              oninput={(e) => setValue(index, (e.target as HTMLInputElement).value)}
              class="{INPUT_CLASS} font-mono"
            />
            <button
              type="button"
              data-testid="{testid}-remove-{index}"
              class={GHOST_BUTTON_CLASS}
              {disabled}
              aria-label="Remove {label} {index + 1}"
              onclick={() => removeValue(index)}
            >
              Remove
            </button>
          </div>
          {#if errors[index]}
            <p class="text-[0.71875rem] text-error" role="alert" data-testid="{testid}-error-{index}">
              {errors[index]}
            </p>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}

  <div>
    <button
      type="button"
      data-testid="{testid}-add"
      class={SECONDARY_BUTTON_CLASS}
      {disabled}
      onclick={() => (values = [...values, ''])}
    >
      {addLabel}
    </button>
  </div>
</div>
