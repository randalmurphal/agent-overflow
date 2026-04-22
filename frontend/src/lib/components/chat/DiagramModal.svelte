<script lang="ts">
  /**
   * Full-viewport diagram viewer with pan + zoom.
   *
   * Presentation: 95vw × 95vh (clamped to full-viewport below 480×320)
   * overlay card. The SVG is painted via `{@html}` inside a transform
   * host; all zoom/pan state lives on the host's CSS transform so
   * the render is GPU-accelerated and there's no re-rasterisation at
   * any scale.
   *
   * Memory model: the SVG markup arrives from the primitive's
   * source-hash cache (already allocated once per diagram); we clone
   * the string and rewrite id prefixes locally so modal + inline
   * renderings don't share SVG `url(#id)` references. The modal owns
   * only the four scalars driving the transform — zero per-node
   * state.
   *
   * Performance note: pointer + wheel handlers mutate reactive state
   * directly; Svelte's fine-grained reactivity means the transform
   * style change is a single DOM write per event. There is no
   * rAF throttling because CSS transforms are already coalesced by
   * the compositor.
   */

  import { focusTrap } from '../../utils/focusTrap';

  interface Props {
    open: boolean;
    svgHtml: string;
    onClose: () => void;
    onContextMenu?: (e: MouseEvent, svg: SVGSVGElement) => void;
  }

  let { open, svgHtml, onClose, onContextMenu }: Props = $props();

  let canvasEl: HTMLDivElement | undefined = $state(undefined);
  let transformHostEl: HTMLDivElement | undefined = $state(undefined);

  // Transform scalars. `userZoomed` blocks the auto-fit ResizeObserver
  // from clobbering a manual zoom when the window is resized.
  let scale = $state(1);
  let tx = $state(0);
  let ty = $state(0);
  let userZoomed = $state(false);
  let isPanning = $state(false);

  // Isolate mermaid element ids for the modal's copy of the SVG.
  // Without this, `fill="url(#mermaid-abc-grad)"` inside the modal
  // SVG would resolve to the FIRST matching id in document order —
  // which is the inline diagram's node. Same-content so visually
  // benign most of the time, but semantically incorrect and flaky if
  // the two diagrams ever diverge.
  const displayHtml = $derived.by(() => {
    if (!svgHtml) return '';
    const suffix = Math.random().toString(36).slice(2, 10);
    return svgHtml.replace(/mermaid-[a-z0-9]+/g, `mermaid-${suffix}`);
  });

  const transform = $derived(`matrix(${scale}, 0, 0, ${scale}, ${tx}, ${ty})`);

  function clamp(n: number, lo: number, hi: number): number {
    return Math.max(lo, Math.min(hi, n));
  }

  // Mermaid ships SVGs with `width="100%"` + inline `max-width:<px>`
  // which makes the rendered box depend on its containing block. Our
  // transform host is absolute-positioned with no intrinsic width, so
  // the SVG would collapse to mermaid's max-width while our fit math
  // reads viewBox units — the two disagree and the diagram lands in
  // the wrong spot at the wrong scale. Pin the SVG's CSS size to the
  // viewBox so "scale=1" means "1:1 with the modelling units" and
  // centering math lines up with pixels.
  function normalizeSvg(svg: SVGSVGElement, width: number, height: number): void {
    svg.setAttribute('width', String(width));
    svg.setAttribute('height', String(height));
    svg.style.maxWidth = 'none';
    svg.style.maxHeight = 'none';
  }

  // Recompute the "fit to canvas" transform. Used on initial open and
  // whenever the window resizes (so long as the user hasn't manually
  // panned/zoomed).
  function fitToCanvas(): void {
    if (!canvasEl || !transformHostEl) return;
    const svg = transformHostEl.querySelector<SVGSVGElement>('svg');
    if (!svg) return;
    const canvasRect = canvasEl.getBoundingClientRect();

    const vb = svg.viewBox?.baseVal;
    const width = vb && vb.width > 0 ? vb.width : svg.getBBox().width;
    const height = vb && vb.height > 0 ? vb.height : svg.getBBox().height;
    if (width <= 0 || height <= 0) return;

    normalizeSvg(svg, width, height);

    // No 1.0 clamp: let small diagrams scale up to fill the canvas.
    // The 10× cap keeps a 20×10 diagram from ballooning to 2000×1000 —
    // readable scale-up without absurd overshoot. Larger diagrams are
    // always shrunk by `min()` regardless.
    const newScale = Math.min(canvasRect.width / width, canvasRect.height / height, 10);
    scale = newScale;
    tx = (canvasRect.width - width * newScale) / 2;
    ty = (canvasRect.height - height * newScale) / 2;
  }

  function reset(): void {
    userZoomed = false;
    fitToCanvas();
  }

  // Zoom around a canvas-local point so the pixel under that point
  // stays anchored as the scale changes.
  function zoomAt(clientX: number, clientY: number, factor: number): void {
    if (!canvasEl) return;
    const rect = canvasEl.getBoundingClientRect();
    const cx = clientX - rect.left;
    const cy = clientY - rect.top;
    const newScale = clamp(scale * factor, 0.1, 20);
    tx = cx - (cx - tx) * (newScale / scale);
    ty = cy - (cy - ty) * (newScale / scale);
    scale = newScale;
    userZoomed = true;
  }

  function zoomCenter(factor: number): void {
    if (!canvasEl) return;
    const rect = canvasEl.getBoundingClientRect();
    zoomAt(rect.left + rect.width / 2, rect.top + rect.height / 2, factor);
  }

  function handleWheel(e: WheelEvent): void {
    e.preventDefault();
    // Exponential curve keeps the zoom feel consistent across
    // trackpad and wheel devices (large deltaY from wheel, small
    // from trackpad). 0.002 is tuned to match macOS natural scrolling.
    const factor = Math.exp(-e.deltaY * 0.002);
    zoomAt(e.clientX, e.clientY, factor);
  }

  let panStart: { cx: number; cy: number; tx: number; ty: number } | null = null;

  function handlePointerDown(e: PointerEvent): void {
    if (e.button !== 0 || !canvasEl) return;
    panStart = { cx: e.clientX, cy: e.clientY, tx, ty };
    isPanning = true;
    canvasEl.setPointerCapture(e.pointerId);
  }

  function handlePointerMove(e: PointerEvent): void {
    if (!panStart) return;
    tx = panStart.tx + (e.clientX - panStart.cx);
    ty = panStart.ty + (e.clientY - panStart.cy);
    userZoomed = true;
  }

  function handlePointerUp(e: PointerEvent): void {
    if (!panStart) return;
    panStart = null;
    isPanning = false;
    canvasEl?.releasePointerCapture(e.pointerId);
  }

  function handleKeydown(e: KeyboardEvent): void {
    switch (e.key) {
      case 'Escape':
        e.preventDefault();
        onClose();
        return;
      case '+':
      case '=':
        e.preventDefault();
        zoomCenter(1.25);
        return;
      case '-':
      case '_':
        e.preventDefault();
        zoomCenter(0.8);
        return;
      case '0':
        e.preventDefault();
        reset();
        return;
      case 'ArrowLeft':
        e.preventDefault();
        tx += 50;
        userZoomed = true;
        return;
      case 'ArrowRight':
        e.preventDefault();
        tx -= 50;
        userZoomed = true;
        return;
      case 'ArrowUp':
        e.preventDefault();
        ty += 50;
        userZoomed = true;
        return;
      case 'ArrowDown':
        e.preventDefault();
        ty -= 50;
        userZoomed = true;
        return;
    }
  }

  function handleBackdropClick(e: MouseEvent): void {
    if (e.target === e.currentTarget) onClose();
  }

  // Reset transform when the modal opens and set up the resize
  // observer that keeps the diagram fit-to-canvas while the user
  // hasn't manually zoomed.
  $effect(() => {
    if (!open || !canvasEl) return;
    userZoomed = false;
    scale = 1;
    tx = 0;
    ty = 0;
    queueMicrotask(() => fitToCanvas());

    const observer = new ResizeObserver(() => {
      if (!userZoomed) fitToCanvas();
    });
    observer.observe(canvasEl);
    return () => observer.disconnect();
  });
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-[70] flex items-center justify-center bg-overlay backdrop-blur-sm"
    data-diagram-modal-backdrop
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
    role="dialog"
    aria-modal="true"
    aria-label="Diagram viewer"
    tabindex="-1"
  >
    <div
      use:focusTrap={{ active: open }}
      class="relative w-[95vw] h-[95vh] bg-surface-1 border border-border rounded-lg shadow-xl overflow-hidden flex flex-col"
      tabindex="-1"
    >
      <div
        bind:this={canvasEl}
        class={[
          'flex-1 relative overflow-hidden select-none',
          isPanning ? 'cursor-grabbing' : 'cursor-grab',
        ].join(' ')}
        oncontextmenu={(e) => {
          // The transform host carries `pointer-events-none` so pans
          // reach the canvas, which means `e.target.closest('svg')`
          // would miss — the target is the canvas div. Query the SVG
          // explicitly from the host and hand it to the caller.
          const svg = transformHostEl?.querySelector<SVGSVGElement>('svg');
          if (svg) onContextMenu?.(e, svg);
        }}
        onwheel={handleWheel}
        onpointerdown={handlePointerDown}
        onpointermove={handlePointerMove}
        onpointerup={handlePointerUp}
        onpointercancel={handlePointerUp}
      >
        <div
          bind:this={transformHostEl}
          class="absolute top-0 left-0 pointer-events-none"
          style:transform={transform}
          style:transform-origin="0 0"
        >
          {@html displayHtml}
        </div>
      </div>

      <div
        class="flex items-center justify-end gap-1 px-3 py-1.5 border-t border-border bg-surface-0 text-xs"
      >
        <span class="mr-2 tabular-nums text-text-secondary" aria-live="polite">
          {Math.round(scale * 100)}%
        </span>
        <button
          class="px-2 py-1 text-text-secondary hover:text-text-primary"
          onclick={() => zoomCenter(0.8)}
          aria-label="Zoom out"
        >
          −
        </button>
        <button
          class="px-2 py-1 text-text-secondary hover:text-text-primary"
          onclick={reset}
          aria-label="Fit to view"
        >
          Fit
        </button>
        <button
          class="px-2 py-1 text-text-secondary hover:text-text-primary"
          onclick={() => zoomCenter(1.25)}
          aria-label="Zoom in"
        >
          +
        </button>
        <span aria-hidden="true" class="mx-2 text-border">|</span>
        <button
          class="px-2 py-1 text-text-secondary hover:text-text-primary"
          onclick={onClose}
          aria-label="Close"
        >
          Close
        </button>
      </div>
    </div>
  </div>
{/if}
