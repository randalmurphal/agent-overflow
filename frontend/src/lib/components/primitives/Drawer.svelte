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
    /**
     * Drop the inline size and the resize handle, so the drawer is sized
     * entirely by the caller's classes. What the compact (phone) layout needs:
     * the terminal drawer becomes the whole screen there, and an inline
     * `height` from a persisted desktop size would win over any class.
     */
    fill?: boolean;
    class?: string;
    children: Snippet;
    onResize?: (size: number) => void;
    /**
     * Called on pointer-down at the resize handle. Should return a
     * disposer that releases any acquired lease, or `null` for a no-op.
     * The bottom (terminal) drawer threads the active pane's
     * `pauseAutoScroll()` here so an in-flight chat stream doesn't
     * yank the timeline as the drawer height changes.
     */
    acquireResizeLease?: () => (() => void) | null;
  }

  let {
    position = 'bottom',
    size = $bindable(320),
    minSize = 120,
    maxSize,
    resizable = true,
    fill = false,
    class: className = '',
    children,
    onResize,
    acquireResizeLease,
  }: Props = $props();

  let dragging = $state(false);
  let startPointer = 0;
  let startSize = 0;
  let releaseResizeLease: (() => void) | null = null;

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
    releaseResizeLease = acquireResizeLease?.() ?? null;
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
    releaseResizeLease?.();
    releaseResizeLease = null;
    onResize?.(size);
  }

  const sizeStyle = $derived(
    fill ? undefined : position === 'bottom' ? `height: ${size}px;` : `width: ${size}px;`,
  );

  // A filled drawer has nothing to resize against — it is already the whole
  // available box — so the handle would only be a dead grab target.
  const showHandle = $derived(resizable && !fill);

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
  {#if showHandle}
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
