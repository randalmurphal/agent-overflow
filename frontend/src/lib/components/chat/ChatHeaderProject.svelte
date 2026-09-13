<script lang="ts">
  // The chat header's project segment: the crumb before the title on
  // desktop (`project / title`), the second segment of the compact facts
  // line. The header draws the trigger; ProjectPicker mounts trigger-less
  // behind it and anchors its menu here. Pickable on a draft, a plain
  // label once the thread has messages (ProjectPicker's lock). The host
  // passes the rest color with `class` (see headerSegmentClasses).
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ProjectPicker from '../composer/workspace/ProjectPicker.svelte';
  import { headerSegmentClasses } from './headerSegmentClasses';

  interface Props {
    pane: ThreadPane;
    class?: string;
  }

  let { pane, class: className = '' }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let picker: { openPicker(): void; label(): string; locked(): boolean } | undefined = $state();
  let projectId = $derived(pane.thread?.projectId ?? null);
  let label = $derived(picker?.label() || 'Project');
  let locked = $derived(picker?.locked() ?? true);
</script>

{#if pane.thread && projectId}
  <button
    bind:this={triggerEl}
    type="button"
    onclick={() => picker?.openPicker()}
    disabled={locked}
    aria-haspopup={locked ? undefined : 'menu'}
    title={locked ? `Project: ${label}` : `Project: ${label} (change)`}
    data-testid="chat-header-project"
    data-locked={locked || undefined}
    class="{headerSegmentClasses} {className}"
  >
    <span class="truncate">{label}</span>
    {#if !locked}
      <Icon icon={ChevronDown} size={11} strokeWidth={2} class="shrink-0 opacity-60" />
    {/if}
  </button>
  <ProjectPicker bind:this={picker} {pane} anchor={triggerEl} placement="bottom-start" />
{/if}
