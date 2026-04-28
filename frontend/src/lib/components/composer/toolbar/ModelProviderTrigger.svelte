<script lang="ts">
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Icon from '../../primitives/Icon.svelte';
  import ClaudeIcon from '../../primitives/brand/ClaudeIcon.svelte';
  import OpenAIIcon from '../../primitives/brand/OpenAIIcon.svelte';

  interface Props {
    buttonEl: HTMLButtonElement | undefined;
    open: boolean;
    disabled: boolean;
    isCodex: boolean;
    provider: string;
    modelLabel: string;
    onClick: () => void;
  }

  let {
    buttonEl = $bindable(),
    open,
    disabled,
    isCodex,
    provider,
    modelLabel,
    onClick,
  }: Props = $props();
</script>

<button
  bind:this={buttonEl}
  type="button"
  onclick={onClick}
  {disabled}
  aria-haspopup="menu"
  aria-expanded={open}
  data-provider={provider}
  data-testid="composer-model-menu-trigger"
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[11px] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  {#if isCodex}
    <OpenAIIcon size={13} class="opacity-95" />
  {:else}
    <ClaudeIcon size={13} class="text-[#d97757] opacity-95" />
  {/if}
  <span class="truncate max-w-[200px] text-fg">{modelLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>
