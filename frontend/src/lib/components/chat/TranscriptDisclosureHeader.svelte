<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
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
    children,
    icon,
    label,
    body,
    actions,
    onToggle,
  }: Props = $props();

  function handleToggle(event: MouseEvent): void {
    if (!expandable) {
      event.preventDefault();
      return;
    }
    void onToggle?.(event);
  }
</script>

<div
  class={[
    'flex w-full items-center gap-2 text-left transition-colors',
    className,
  ].join(' ')}
  data-testid={headerTestId}
>
  <button
    type="button"
    class={[
      'flex min-w-0 items-center gap-2 bg-transparent p-0 text-left',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
      expandable ? 'cursor-pointer' : 'cursor-default',
      interactiveBody ? 'shrink-0' : 'flex-1',
      buttonClass,
    ].join(' ')}
    onclick={handleToggle}
    tabindex={expandable ? undefined : -1}
    aria-disabled={!expandable}
    aria-expanded={expandable ? expanded : false}
    aria-controls={expandable ? controls : undefined}
    aria-label={ariaLabel}
    data-testid={testId}
  >
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
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
      <span class="w-12 shrink-0 truncate text-[0.6875rem] text-fg-hint" data-testid="{testId}-label-slot">
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

  {#if actions}
    {@render actions()}
  {/if}
</div>
