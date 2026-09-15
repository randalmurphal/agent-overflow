<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    expanded: boolean;
    expandable?: boolean;
    controls?: string;
    ariaLabel?: string;
    testId: string;
    headerTestId?: string;
    class?: string;
    buttonClass?: string;
    interactiveBody?: boolean;
    /** Agent summaries omit the tool gutter and move metrics below at narrow widths. */
    agentLayout?: boolean;
    metrics?: Snippet;
    details?: Snippet;
    onActivate?: () => void;
    children?: Snippet;
    icon?: Snippet;
    label?: Snippet;
    body?: Snippet;
    actions?: Snippet;
    onToggle?: (event: MouseEvent) => void | Promise<void>;
  }

  let {
    expanded,
    expandable = true,
    controls,
    ariaLabel,
    testId,
    headerTestId,
    class: className = '',
    buttonClass = '',
    interactiveBody = false,
    agentLayout = false,
    metrics,
    details,
    onActivate,
    children,
    icon,
    label,
    body,
    actions,
    onToggle,
  }: Props = $props();

  function handleToggle(event: MouseEvent): void {
    if (onActivate) {
      onActivate();
      return;
    }
    if (!expandable) {
      event.preventDefault();
      return;
    }
    void onToggle?.(event);
  }
</script>

<div
  class={[
    'flex w-full items-center gap-2 compact:gap-1 text-left',
    agentLayout ? '@container/agent-header flex-wrap gap-y-0.5 [--agent-inset:2.625rem] compact:[--agent-inset:2.125rem]' : '',
    className,
  ].join(' ')}
  data-testid={headerTestId}
>
  <button
    type="button"
    class={[
      'flex min-w-0 items-center gap-2 compact:gap-1 bg-transparent p-0 text-left compact:min-h-9 compact:select-none',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
      expandable || onActivate ? 'cursor-pointer' : 'cursor-default',
      interactiveBody ? 'shrink-0' : 'flex-1',
      buttonClass,
    ].join(' ')}
    onclick={handleToggle}
    tabindex={expandable || onActivate ? undefined : -1}
    aria-disabled={!expandable && !onActivate}
    aria-expanded={onActivate ? undefined : expandable ? expanded : false}
    aria-controls={expandable ? controls : undefined}
    aria-label={ariaLabel}
    data-testid={testId}
  >
    <!-- The chevron SNAPS between states — no transition-transform. A CSS
         transition starting in the same commit as a bottom-held toggle puts
         the compositor in animation-priority mode, licensing it to present
         the frame before the toggle's re-rastered tiles are ready (the
         expand "text blinks out below the run" incident, 2026-08-17). The
         timeline-wide kill rule in app.css enforces this for everything in
         the scroller; the class is removed here too so the component tells
         the truth. -->
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none"
      class:rotate-90={expandable && expanded}
      class:opacity-30={!expandable}
      aria-hidden="true"
    >
      <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
    </span>
    {#if icon || label || (body && !interactiveBody)}
      <span class="flex size-3.5 shrink-0 items-center justify-center" data-testid="{testId}-icon-slot">
        {#if icon}{@render icon()}{/if}
      </span>
      <span class={agentLayout ? "sr-only" : "w-12 compact:w-9 shrink-0 truncate text-[0.6875rem] text-fg-hint"} data-testid="{testId}-label-slot">
        {#if label}{@render label()}{/if}
      </span>
      {#if !interactiveBody}
        <!--
          The body slot is a flex container so its child (a single inner
          span across nearly every consumer — see e.g. GenericToolCallRow,
          CommandOutput) is a real flex item with effective `flex-1 min-w-0
          truncate`. Without `flex` here the inner is just nested inline
          content: `flex-1` / `min-w-0` are ignored and `truncate`'s
          overflow:hidden + text-overflow:ellipsis don't apply to inline
          boxes, so a long preview (long Bash command, long file path) ran
          on under the timestamp instead of clipping at the body's column.
        -->
        <span class="flex min-w-0 flex-1" data-testid="{testId}-body-slot">
          {#if body}{@render body()}{/if}
        </span>
      {/if}
    {:else if children}
      {@render children()}
    {/if}
  </button>

  {#if interactiveBody && body}
    <span class="flex min-w-0 flex-1" data-testid="{testId}-body-slot">
      {@render body()}
    </span>
  {/if}

  {#if agentLayout}
    {#if metrics}
      <span class="flex shrink-0 items-center gap-2 text-[0.625rem] text-fg-hint tabular-nums @max-[36rem]/agent-header:order-1 @max-[36rem]/agent-header:w-full @max-[36rem]/agent-header:pl-[var(--agent-inset)]">
        {@render metrics()}
      </span>
    {/if}
    {#if actions}
      <span class="flex shrink-0 items-center gap-2 compact:gap-1">{@render actions()}</span>
    {/if}
    {#if details}
      <span class="order-2 block w-full min-w-0 pl-[var(--agent-inset)]">
        {@render details()}
      </span>
    {/if}
  {:else if actions}
    {@render actions()}
  {/if}
</div>
