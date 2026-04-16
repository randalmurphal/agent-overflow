<script lang="ts">
  let { checked, disabled = false, ariaLabel, onToggle }: {
    checked: boolean;
    disabled?: boolean;
    ariaLabel?: string;
    onToggle: (value: boolean) => void;
  } = $props();

  function handleToggle() {
    if (!disabled) {
      onToggle(!checked);
    }
  }
</script>

<button
  type="button"
  role="switch"
  aria-checked={checked}
  aria-label={ariaLabel}
  disabled={disabled}
  onclick={handleToggle}
  class="relative inline-flex h-6 w-11 shrink-0 items-center rounded-full border transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-not-allowed disabled:opacity-50
    {checked
      ? 'border-accent/40 bg-accent/85'
      : 'border-border bg-surface-2/80'}"
>
  <span
    class="block h-[18px] w-[18px] rounded-full bg-white shadow-sm transition-transform duration-200 {checked ? 'translate-x-[21px]' : 'translate-x-[3px]'}"
  ></span>
</button>
