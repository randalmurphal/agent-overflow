<script lang="ts">
  import Paperclip from '@lucide/svelte/icons/paperclip';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { isNativeShell } from '../../native/platform';

  let { disabled = false, onChoose }: {
    disabled?: boolean;
    onChoose: (accept: string) => void;
  } = $props();
  let anchor: HTMLButtonElement | undefined = $state();
  let open = $state(false);
  function choose(accept: string): void {
    open = false;
    onChoose(accept);
  }
</script>

<button
  bind:this={anchor}
  type="button"
  {disabled}
  aria-label="Attach files"
  title="Attach files"
  data-testid="composer-attach"
  onclick={() => isNativeShell() ? (open = !open) : choose('')}
  class="inline-flex size-8 compact:size-9 shrink-0 items-center justify-center rounded-[var(--radius-control)] text-fg-muted hover:bg-surface-2/40 hover:text-fg disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
>
  <Icon icon={Paperclip} size={16} />
</button>
<Popover {anchor} {open} onClose={() => (open = false)} placement="top-end" role="none">
  <Menu ariaLabel="Add attachments" onClose={() => (open = false)}>
    <MenuItem label="Photos" onSelect={() => choose('image/*')} />
    <MenuItem label="Files" onSelect={() => choose('')} />
  </Menu>
</Popover>
