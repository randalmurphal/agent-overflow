<script lang="ts">
  // The header strip every tool pane and ticket pane wears: the status
  // line, a Back control, the pane's title as its drag handle (a double-click
  // maximizes it, as the maximize control does), the body's own segments,
  // and the maximize and close controls. A body renders it as its first row
  // so it can fill the segments (a breadcrumb, a filter summary, a ticket's
  // controls); ToolPane and TicketPane render it bare while the body chunk
  // loads. A tool's title is its kind's; a ticket pane passes its own.
  // Under compact the title is no handle, and a tool whose first segment
  // already names where the person is leaves it out (`compactTitle`), so
  // that segment keeps the phone's width.
  //
  // Back has one meaning at a time. With a detail step open the tool passes
  // `onBack` and the control steps back to its list; otherwise the compact
  // layout's control takes the phone's one step back (`utils/stepBack.ts`):
  // what is open on the page (a find bar, a menu) closes first, then the
  // tool returns to the thread list.
  import type { Snippet } from 'svelte';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderLine from './PaneHeaderLine.svelte';
  import PaneHeaderIconButton from './PaneHeaderIconButton.svelte';
  import PaneCloseButton from './PaneCloseButton.svelte';
  import PanePromoteButton from './PanePromoteButton.svelte';
  import { togglePanePromotion } from '../../stores/panes.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { stepBack } from '../../utils/stepBack';
  import type { ToolPaneKind } from '../../stores/paneLayout.svelte';
  import { toolPaneMeta } from './toolPaneMeta';

  type Props = {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
    /** Step back out of the tool's detail. Omit at the tool's list. */
    onBack?: () => void;
    backLabel?: string;
    backTestId?: string;
    titleTestId?: string;
    /** False leaves the title out under compact. */
    compactTitle?: boolean;
    children?: Snippet;
  } & ({ kind: ToolPaneKind; title?: never } | { kind?: never; title: string });

  let {
    paneId,
    kind,
    title: ownTitle,
    titleTestId = 'tool-pane-title',
    compactTitle = true,
    onPaneDragStart,
    onBack,
    backLabel = 'Back',
    backTestId = 'tool-pane-back',
    children,
  }: Props = $props();

  let compact = $derived(isCompactLayout());
  let title = $derived(ownTitle ?? toolPaneMeta(kind!).title);
</script>

<div
  data-testid="tool-pane-header"
  class="relative z-10 flex min-w-0 shrink-0 items-center gap-1.5 border-b border-border-subtle bg-transparent px-3 py-2 compact:pt-[max(0.5rem,env(safe-area-inset-top))]"
>
  <PaneHeaderLine {paneId} />
  {#if onBack}
    <PaneHeaderIconButton label={backLabel} testId={backTestId} onclick={onBack}>
      <Icon icon={ChevronLeft} size={14} strokeWidth={2} />
    </PaneHeaderIconButton>
  {:else if compact}
    <PaneHeaderIconButton label="Back to threads" testId="compact-back" onclick={() => { stepBack(); }}>
      <Icon icon={ArrowLeft} size={14} strokeWidth={2} />
    </PaneHeaderIconButton>
  {/if}
  <!-- The title is the pane drag handle, as a thread pane's is. A tool has
       no name of its own to edit, so there is no rename. -->
  {#if compactTitle || !compact}
  <button
    type="button"
    draggable={onPaneDragStart != null && !compact}
    ondragstart={(event) => onPaneDragStart?.(event)}
    ondblclick={() => { if (!compact) togglePanePromotion(paneId); }}
    data-testid={titleTestId}
    class={[
      'shrink-0 truncate rounded-[var(--radius-field)] border-none bg-transparent px-1.5 py-0.5 text-left text-sm font-medium text-fg transition-colors hover:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:select-none compact:py-1.5 compact:text-base',
      onPaneDragStart && !compact ? 'cursor-grab active:cursor-grabbing' : 'cursor-default',
    ].join(' ')}
  >{title}</button>
  {/if}
  <div class="flex min-w-0 flex-1 items-center">
    {@render children?.()}
  </div>
  {#if !compact}
    <PanePromoteButton {paneId} />
    <PaneCloseButton {paneId} />
  {/if}
</div>
