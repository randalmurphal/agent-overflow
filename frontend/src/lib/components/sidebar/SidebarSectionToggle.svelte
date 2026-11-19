<script lang="ts">
  // The chevron that folds a whole sidebar section (docs/specs/
  // standing-agents.md, "Sidebar"), persisted per client. It shows whether
  // the section's rows are shown (isSidebarSectionShown), which a search
  // does through a fold.
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import { toggleSidebarSection, type SidebarSectionId } from '../../stores/sidebar.svelte';

  interface Props {
    section: SidebarSectionId;
    label: string;
    /** Whether the section's rows are shown. */
    shown: boolean;
  }

  let { section, label, shown }: Props = $props();
</script>

<span class="-ml-1 flex shrink-0">
  <IconButton
    label={shown ? `Collapse ${label}` : `Expand ${label}`}
    size="xs"
    ariaExpanded={shown}
    testId="sidebar-section-toggle-{section}"
    onClick={() => toggleSidebarSection(section)}
  >
    {#snippet children()}
      <Icon icon={ChevronRight} size={12} strokeWidth={2.4} class={'transition-transform ' + (shown ? 'rotate-90' : '')} />
    {/snippet}
  </IconButton>
</span>
