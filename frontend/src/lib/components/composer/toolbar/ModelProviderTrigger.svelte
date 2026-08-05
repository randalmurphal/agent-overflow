<script lang="ts">
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import ProviderIcon from '../../shared/ProviderIcon.svelte';
  import { chordHintSuffix } from '../../../stores/keybindings.svelte';

  interface Props {
    buttonEl: HTMLButtonElement | undefined;
    open: boolean;
    disabled: boolean;
    provider: string;
    modelLabel: string;
    onClick: () => void;
  }

  let {
    buttonEl = $bindable(),
    open,
    disabled,
    provider,
    modelLabel,
    onClick,
  }: Props = $props();

  let pickerChordSuffix = $derived(chordHintSuffix('composer.picker.model'));
</script>

<button
  bind:this={buttonEl}
  type="button"
  onclick={onClick}
  {disabled}
  aria-haspopup="menu"
  aria-expanded={open}
  title={`Model: ${modelLabel}${pickerChordSuffix}`}
  data-provider={provider}
  data-testid="composer-model-menu-trigger"
  class={composerTriggerClasses}
>
  <ProviderIcon {provider} size={13} />
  <span class="truncate max-w-[200px] text-fg">{modelLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>
