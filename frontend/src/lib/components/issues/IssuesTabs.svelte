<script lang="ts">
  // The pane's screens in its top bar (docs/specs/issues.md, "Pane"): a
  // tab each, or, folded (a narrow pane, and always under compact), one
  // picker naming the screen shown that lists them all.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { ISSUES_SCREENS, type IssuesScreen } from '../../stores/issuesPane.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  interface Props {
    current: IssuesScreen;
    folded: boolean;
    onPick: (screen: IssuesScreen) => void;
  }

  let { current, folded, onPick }: Props = $props();

  let open = $state(false);
  let trigger = $state<HTMLElement | undefined>();
  const currentLabel = $derived(ISSUES_SCREENS.find((s) => s.screen === current)?.label ?? '');

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl: trigger });
  }

  function pick(screen: IssuesScreen): void {
    close();
    onPick(screen);
  }
</script>

{#if folded}
  <button
    type="button"
    class="inline-flex h-6 shrink-0 items-center gap-1 rounded-[var(--radius-control)] px-2 text-[0.75rem] text-fg hover:bg-surface-1 compact:select-none"
    aria-haspopup="menu"
    aria-expanded={open}
    bind:this={trigger}
    data-testid="issues-tabs-picker"
    onclick={() => { open = !open; }}
  >
    <span>{currentLabel}</span>
    <Icon icon={ChevronDown} size={12} strokeWidth={2} />
  </button>
  <Popover anchor={trigger} {open} ariaLabel="Screens" placement="bottom-start" onClose={close}>
    <Menu ariaLabel="Screens" onClose={close} minWidthClass="min-w-32">
      {#each ISSUES_SCREENS as s (s.screen)}
        <MenuItem label={s.label} checked={s.screen === current} testId="issues-tab-option" onSelect={() => pick(s.screen)} />
      {/each}
    </Menu>
  </Popover>
{:else}
  <div class="flex shrink-0 items-center gap-0.5" role="tablist" aria-label="Screens" data-testid="issues-tabs">
    {#each ISSUES_SCREENS as s (s.screen)}
      <button
        type="button"
        role="tab"
        aria-selected={s.screen === current}
        class="h-6 rounded-[var(--radius-control)] px-2 text-[0.75rem] {s.screen === current ? 'bg-surface-1 text-fg' : 'text-fg-muted hover:bg-surface-1 hover:text-fg'}"
        data-testid="issues-tab"
        data-screen={s.screen}
        onclick={() => onPick(s.screen)}
      >{s.label}</button>
    {/each}
  </div>
{/if}
