<script lang="ts">
  /*
   * Bottom- or right-edge drawer frame. Provides the border / bg
   * chrome + optional resize handle. Callers own the content and state;
   * this primitive only reports new sizes through `onResize` so
   * parent stores can persist the preferred size across sessions.
   *
   * Size is bindable — use `bind:size` if you want the parent to
   * track the live value during drag rather than only on drag-end.
   */
  import type { Snippet } from 'svelte';

  type Position = 'bottom' | 'right';

  interface Props {
    position?: Position;
    size: number;
    minSize?: number;
    maxSize?: number;
    resizable?: boolean;
    class?: string;
    children: Snippet;
    onResize?: (size: number) => void;
  }

  let {
    position = 'bottom',
    size = $bindable(320),
    minSize = 120,
    maxSize,
    resizable = true,
    class: className = '',
    children,
    onResize,
  }: Props = $props();

  let dragging = $state(false);
  let startPointer = 0;
  let startSize = 0;

  // Resolve the effective maximum size: explicit cap, or 85% of the
  // viewport on the relevant axis. Computed at drag-start so later
  // viewport resizes don't yank the user's drag mid-gesture.
  function resolveMax(): number {
    if (maxSize !== undefined) return maxSize;
    if (typeof window === 'undefined') return Infinity;
    const axis = position === 'bottom' ? window.innerHeight : window.innerWidth;
    return Math.floor(axis * 0.85);
  }

  function clamp(value: number): number {
    const max = resolveMax();
    return Math.max(minSize, Math.min(max, value));
  }

  function onPointerDown(e: PointerEvent): void {
    if (!resizable) return;
    dragging = true;
    startPointer = position === 'bottom' ? e.clientY : e.clientX;
    startSize = size;
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
  }

  function onPointerMove(e: PointerEvent): void {
    if (!dragging) return;
    const pointer = position === 'bottom' ? e.clientY : e.clientX;
    // Bottom drawer grows when the pointer moves up; right drawer
    // grows when the pointer moves left. Invert the delta in both
    // cases so positive drag-towards-center grows the drawer.
    const delta = startPointer - pointer;
    size = clamp(startSize + delta);
  }

  function onPointerUp(e: PointerEvent): void {
    if (!dragging) return;
    dragging = false;
    (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
    onResize?.(size);
  }

  const sizeStyle = $derived(
    position === 'bottom' ? `height: ${size}px;` : `width: ${size}px;`,
  );

  const chromeClass = $derived(
    position === 'bottom'
      ? 'border-t border-border-subtle'
      : 'border-l border-border-subtle',
  );

  const handleClass = $derived(
    position === 'bottom'
      ? 'absolute top-0 left-0 right-0 h-1.5 cursor-row-resize'
      : 'absolute top-0 bottom-0 left-0 w-1.5 cursor-col-resize',
  );
</script>

<aside
  class={[
    'relative flex shrink-0 bg-surface-0',
    position === 'bottom' ? 'flex-col' : 'flex-row',
    chromeClass,
    className,
  ].join(' ')}
  style={sizeStyle}
  data-drawer-position={position}
>
  {#if resizable}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
      role="separator"
      aria-orientation={position === 'bottom' ? 'horizontal' : 'vertical'}
      class={[
        handleClass,
        'z-10 hover:bg-accent/25 transition-colors',
        dragging ? 'bg-accent/40' : '',
      ].join(' ')}
      onpointerdown={onPointerDown}
      onpointermove={onPointerMove}
      onpointerup={onPointerUp}
      onpointercancel={onPointerUp}
    ></div>
  {/if}
  {@render children()}
</aside>
