<script lang="ts">
  // A priority's mark, picked by its name (`boardFormat.ts`); nothing for
  // no priority.
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronUp from '@lucide/svelte/icons/chevron-up';
  import ChevronsDown from '@lucide/svelte/icons/chevrons-down';
  import ChevronsUp from '@lucide/svelte/icons/chevrons-up';
  import Equal from '@lucide/svelte/icons/equal';
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import Icon from '../primitives/Icon.svelte';
  import type { ChoiceView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { priorityMark, type PriorityMark } from './boardFormat';

  let { priority, testId }: { priority: ChoiceView | null | undefined; testId: string } = $props();

  const PRIORITY_ICONS = {
    urgent: { icon: TriangleAlert, tone: 'text-error' },
    highest: { icon: ChevronsUp, tone: 'text-error' },
    high: { icon: ChevronUp, tone: 'text-warning' },
    medium: { icon: Equal, tone: 'text-warning' },
    low: { icon: ChevronDown, tone: 'text-info' },
    lowest: { icon: ChevronsDown, tone: 'text-info' },
    other: { icon: Equal, tone: 'text-fg-muted' },
  } satisfies Record<Exclude<PriorityMark, 'none'>, unknown>;

  const named = $derived(priorityMark(priority));
</script>

{#if named !== 'none'}
  {@const mark = PRIORITY_ICONS[named]}
  <span class="inline-flex shrink-0 {mark.tone}" title="{priority?.name} priority" data-testid={testId}>
    <Icon icon={mark.icon} size={13} strokeWidth={2} />
  </span>
{/if}
