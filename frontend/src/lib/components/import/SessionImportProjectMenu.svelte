<script lang="ts">
  // Project filter for the import catalogue: a trigger button anchoring the
  // shared Popover + Menu composition (ProjectSortMenu is the reference
  // shape). A native <select> cannot carry what these entries need — a count
  // beside the name, a muted path under an unresolved project, a title with
  // the whole path — and a real Claude home lists dozens of projects.
  //
  // This is the worked example for the picker-in-dialog contract at the top
  // of Popover.svelte, and it pays neither cost itself: Modal declines Escape
  // while a popover it owns is open, and `claimTab` + `restoreFocusTo` hand
  // the focus half to the primitive. Everything left here is the filter.

  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import type { ImportProjectGroup } from '../../types/sessionImport';
  import { truncateMiddle } from '../../utils/format';

  interface Props {
    groups: readonly ImportProjectGroup[];
    /** The active `ImportProjectGroup.key`, or null for "All projects". */
    value: string | null;
    disabled: boolean;
    onSelect: (key: string | null) => void;
  }

  let { groups, value, disabled, onSelect }: Props = $props();

  /** Fits the menu's 260px min-width at the description's font size. */
  const PATH_CHARS = 44;

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);

  let active = $derived(groups.find((group) => group.key === value));
  // A project filter whose group vanished still has to name itself. The store
  // clears an orphaned one at both writers that can strand it — a rescan, and
  // the toggle withdrawing the group's last offered row — so this covers the
  // frame in between, never a resting state.
  let triggerLabel = $derived(value === null ? 'All projects' : (active?.label ?? 'Project'));

  function close(): void {
    if (!open) return;
    open = false;
  }

  function handleToggle(): void {
    open = !open;
  }

  // A run freezes the surface, and it can start while the menu is open (Enter
  // runs the import from anywhere on the surface). Re-runs once on the write
  // and settles, because close() is a no-op on a closed menu.
  $effect(() => {
    if (disabled) close();
  });

  function handleSelect(key: string | null): void {
    onSelect(key);
    close();
  }
</script>

<button
  bind:this={buttonEl}
  type="button"
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label="Project filter"
  data-testid="session-import-project-trigger"
  title={active?.path}
  {disabled}
  onclick={handleToggle}
  class="flex max-w-[16rem] shrink-0 items-center gap-1 rounded-[var(--radius-field)] border border-border-subtle
    bg-surface-0 px-2 py-1 text-[0.6875rem] text-fg transition-colors
    disabled:cursor-not-allowed disabled:opacity-50
    hover:border-border-strong focus:border-accent focus:outline-none
    focus-visible:ring-2 focus-visible:ring-accent/40"
>
  <span class="min-w-0 truncate">{triggerLabel}</span>
  <Icon icon={ChevronDown} size={11} class="shrink-0 text-fg-hint" />
</button>

<Popover
  anchor={buttonEl}
  {open}
  onClose={close}
  placement="bottom-start"
  role="none"
  claimTab
  restoreFocusTo={buttonEl}
>
  {#snippet children()}
    <Menu ariaLabel="Project filter" onClose={close} minWidthClass="min-w-[260px]">
      {#snippet children()}
        <MenuItem
          label="All projects"
          checked={value === null}
          onSelect={() => handleSelect(null)}
        />
        {#each groups as group (group.key)}
          <MenuItem
            label={group.label}
            description={group.known ? undefined : truncateMiddle(group.path, PATH_CHARS)}
            title={group.path}
            suffix={String(group.count)}
            checked={group.key === value}
            onSelect={() => handleSelect(group.key)}
          />
        {/each}
      {/snippet}
    </Menu>
  {/snippet}
</Popover>
