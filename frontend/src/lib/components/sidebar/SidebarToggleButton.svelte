<script lang="ts">
  // The visible half of the sidebar collapse feature. One component
  // serves both states so the control keeps its size, variant, and
  // focus ring across the transition — only the glyph and the verb
  // flip. The chord in the label is read live (same idiom as
  // AgentModeToggle / SidebarSearch's hint pill), so a rebind in
  // Settings shows up here without a reload — and the label loses its
  // suffix entirely when the command is unbound.

  import PanelLeftClose from '@lucide/svelte/icons/panel-left-close';
  import PanelLeftOpen from '@lucide/svelte/icons/panel-left-open';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';
  import {
    isSidebarCollapsed,
    toggleSidebarCollapsed,
  } from '../../stores/sidebarLayout.svelte';

  let collapsed = $derived(isSidebarCollapsed());
  let label = $derived(
    `${collapsed ? 'Expand' : 'Collapse'} Sidebar${chordHintSuffix('sidebar.toggle')}`,
  );
</script>

<IconButton
  {label}
  size="sm"
  ariaExpanded={!collapsed}
  testId="sidebar-collapse-toggle"
  onClick={toggleSidebarCollapsed}
>
  {#snippet children()}
    <Icon icon={collapsed ? PanelLeftOpen : PanelLeftClose} size={14} strokeWidth={2} />
  {/snippet}
</IconButton>
