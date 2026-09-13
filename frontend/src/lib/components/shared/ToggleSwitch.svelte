<script lang="ts">
  let { checked, disabled = false, ariaLabel, title, onToggle }: {
    checked: boolean;
    disabled?: boolean;
    ariaLabel?: string;
    /** Hover text; a disabled switch uses it to say why. */
    title?: string;
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
  {title}
  disabled={disabled}
  onclick={handleToggle}
  class="relative inline-flex h-6 w-11 compact:h-8 compact:w-14 shrink-0 items-center rounded-full border transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-not-allowed disabled:opacity-50
    {checked
      ? 'border-accent/40 bg-accent/85'
      : 'border-border bg-surface-2/80'}"
>
  <span
    class="block h-[18px] w-[18px] compact:h-6 compact:w-6 rounded-full bg-text-primary shadow-sheet transition-transform duration-200 {checked ? 'translate-x-[21px] compact:translate-x-[27px]' : 'translate-x-[3px]'}"
  ></span>
</button>
