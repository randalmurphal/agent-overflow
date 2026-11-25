<script lang="ts">
  // The Display menu: which card fields this board shows, remembered per
  // board by this client. Under compact it also holds Only mine, which the
  // desktop header shows as its own toggle.
  import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { CARD_FIELDS, type CardField } from '../../stores/issuesPane.svelte';

  interface Props {
    hidden: ReadonlySet<CardField>;
    onField: (field: CardField, shown: boolean) => void;
    /** Only mine, in the menu; omitted where the header shows it. */
    onlyMine?: { on: boolean; set: (on: boolean) => void };
  }

  let { hidden, onField, onlyMine }: Props = $props();

  let open = $state(false);
  let anchor: HTMLElement | undefined = $state();

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl: anchor?.querySelector('button') ?? undefined });
  }
</script>

<span class="inline-flex" bind:this={anchor}>
  <PaneHeaderIconButton label="Display" testId="issues-display" onclick={() => (open = !open)}>
    <Icon icon={SlidersHorizontal} size={14} strokeWidth={2} />
  </PaneHeaderIconButton>
</span>
<Popover {anchor} {open} onClose={close} placement="bottom-end" ariaLabel="Display">
  <Menu ariaLabel="Display" onClose={close} minWidthClass="min-w-[200px]">
    {#if onlyMine}
      <MenuItem label="Only mine" checkbox checked={onlyMine.on} onSelect={() => onlyMine.set(!onlyMine.on)} testId="issues-only-mine-item" />
      <MenuDivider />
    {/if}
    {#each CARD_FIELDS as entry (entry.field)}
      <MenuItem
        label={entry.label}
        checkbox
        checked={!hidden.has(entry.field)}
        onSelect={() => onField(entry.field, hidden.has(entry.field))}
        testId="issues-display-{entry.field}"
      />
    {/each}
  </Menu>
</Popover>
