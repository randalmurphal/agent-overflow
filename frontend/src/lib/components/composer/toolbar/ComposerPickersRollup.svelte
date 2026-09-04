<script lang="ts">
  // The toolbar's minimal rung: one trigger standing in for every picker
  // but the model, so the model, the read-only meters (rate limits,
  // context) and Send keep their place on a phone-width composer instead
  // of yielding to the pickers. Each row hands off to the picker it
  // names — the picker components stay mounted (CSS-hidden by the rung)
  // and keep their registry handles, so a row opens exactly the sheet
  // the chord would. The two toggles (agent mode, plan sidebar) act in
  // place, the way their buttons do.
  import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import { openComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { currentAgentMode, cycleAgentMode } from './agentModeCycle';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import Icon from '../../primitives/Icon.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';

  interface Props {
    pane: ThreadPane;
    showMode: boolean;
    showAccess: boolean;
    showMcp: boolean;
    showPlan: boolean;
  }

  let { pane, showMode, showAccess, showMcp, showPlan }: Props = $props();

  let open = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let modeLabel = $derived(currentAgentMode(pane) === 'plan' ? 'Plan' : 'Build');

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl });
  }

  function pick(action: () => void): void {
    open = false;
    action();
  }
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={() => (open = !open)}
  data-testid="composer-pickers-rollup"
  aria-label="Composer options"
  aria-haspopup="menu"
  aria-expanded={open}
  title="Composer options"
  class={[
    'relative inline-flex items-center rounded-[var(--radius-field)] px-1.5 py-1',
    'text-[0.6875rem] transition-colors cursor-pointer',
    open
      ? 'bg-surface-2/60 text-fg ring-1 ring-inset ring-border-subtle'
      : 'text-fg-muted hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
  ].join(' ')}
>
  <Icon icon={SlidersHorizontal} size={13} strokeWidth={1.75} class="opacity-80" />
</button>

<Popover anchor={triggerEl} {open} onClose={close} placement="top-start" role="none">
  {#snippet children()}
    <Menu ariaLabel="Composer options" onClose={close}>
      <MenuItem label="Effort…" onSelect={() => pick(() => openComposerPicker(pane.paneId, 'effort'))} />
      {#if showMode}
        <MenuItem
          label="Agent mode"
          suffix={modeLabel}
          onSelect={() => pick(() => void cycleAgentMode(pane))}
        />
      {/if}
      {#if showAccess}
        <MenuItem label="Access…" onSelect={() => pick(() => openComposerPicker(pane.paneId, 'access'))} />
      {/if}
      {#if showMcp}
        <MenuItem label="MCP servers…" onSelect={() => pick(() => openComposerPicker(pane.paneId, 'mcp'))} />
      {/if}
      {#if showPlan}
        <MenuItem
          label="Plan sidebar"
          checked={pane.showPlanSidebar}
          onSelect={() => pick(() => pane.togglePlanSidebar())}
        />
      {/if}
    </Menu>
  {/snippet}
</Popover>
