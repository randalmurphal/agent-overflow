<script lang="ts">
  /**
   * Full-viewport diagram viewer with pan + zoom.
   *
   * Presentation: 95vw × 95vh (clamped to full-viewport below 480×320)
   * overlay card. The SVG is painted via `{@html}` inside a transform
   * host; all zoom/pan state lives on the host's CSS transform
   * (`utils/panZoom.svelte.ts`, shared with the image lightbox) so the
   * render is GPU-accelerated and there's no re-rasterisation at any
   * scale.
   *
   * Memory model: the SVG markup arrives from the primitive's
   * source-hash cache (already allocated once per diagram); we clone
   * the string and rewrite id prefixes locally so modal + inline
   * renderings don't share SVG `url(#id)` references. The modal owns
   * only the scalars driving the transform; zero per-node state.
   */

  import { focusTrap } from '../../utils/focusTrap';
  import { airspaceSurface } from '../../utils/paneAirspace.svelte';
  import { PanZoom, ZOOM_STEP, type ContentSize } from '../../utils/panZoom.svelte';

  interface Props {
    open: boolean;
    svgHtml: string;
    onClose: () => void;
    onContextMenu?: (e: MouseEvent, svg: SVGSVGElement) => void;
  }

  let { open, svgHtml, onClose, onContextMenu }: Props = $props();

  let canvasEl: HTMLDivElement | undefined = $state(undefined);
  let transformHostEl: HTMLDivElement | undefined = $state(undefined);

  // No 1.0 clamp on fit: a small diagram scales up to fill the canvas. The
  // 10× cap keeps a 20×10 diagram from ballooning to 2000×1000; readable
  // scale-up without absurd overshoot.
  const view = new PanZoom({
    canvas: () => canvasEl,
    minScale: 0.1,
    maxScale: 20,
    fitMaxScale: 10,
  });

  // Isolate mermaid element ids for the modal's copy of the SVG.
  // Without this, `fill="url(#mermaid-abc-grad)"` inside the modal
  // SVG would resolve to the FIRST matching id in document order,
  // which is the inline diagram's node. Same-content so visually
  // benign most of the time, but semantically incorrect and flaky if
  // the two diagrams ever diverge.
  const displayHtml = $derived.by(() => {
    if (!svgHtml) return '';
    const suffix = Math.random().toString(36).slice(2, 10);
    return svgHtml.replace(/mermaid-[a-z0-9]+/g, `mermaid-${suffix}`);
  });

  // Mermaid ships SVGs with `width="100%"` + inline `max-width:<px>`
  // which makes the rendered box depend on its containing block. Our
  // transform host is absolute-positioned with no intrinsic width, so
  // the SVG would collapse to mermaid's max-width while our fit math
  // reads viewBox units; the two disagree and the diagram lands in
  // the wrong spot at the wrong scale. Pin the SVG's CSS size to the
  // viewBox so "scale=1" means "1:1 with the modelling units" and
  // centering math lines up with pixels.
  function normalizeSvg(svg: SVGSVGElement, width: number, height: number): void {
    svg.setAttribute('width', String(width));
    svg.setAttribute('height', String(height));
    svg.style.maxWidth = 'none';
    svg.style.maxHeight = 'none';
    svg.style.transform = 'none';
    svg.style.transformOrigin = '';
    svg.style.willChange = '';
    svg.style.transformBox = '';
  }

  function diagramSvg(): SVGSVGElement | null {
    // The preferred selector won't match inside displayHtml because the
    // id-isolation regex (/mermaid-[a-z0-9]+/) rewrites the attribute
    // name to data-mermaid-<suffix>. The fallback is safe here since
    // callers pass only the diagram SVG outerHTML (no toolbar icons).
    return (
      transformHostEl?.querySelector<SVGSVGElement>('svg[data-mermaid-svg]') ??
      transformHostEl?.querySelector<SVGSVGElement>('svg') ??
      null
    );
  }

  /** The diagram's size in modelling units, pinned onto the SVG's CSS box. */
  function contentSize(): ContentSize | null {
    const svg = diagramSvg();
    if (!svg) return null;
    const vb = svg.viewBox?.baseVal;
    const width = vb && vb.width > 0 ? vb.width : svg.getBBox().width;
    const height = vb && vb.height > 0 ? vb.height : svg.getBBox().height;
    if (width <= 0 || height <= 0) return null;
    normalizeSvg(svg, width, height);
    return { width, height };
  }

  function fit(): void {
    const content = contentSize();
    if (content) view.fit(content);
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
      return;
    }
    const content = contentSize();
    if (content) view.onKeydown(e, content);
  }

  function handleBackdropClick(e: MouseEvent): void {
    if (e.target === e.currentTarget) onClose();
  }

  // Fit when the modal opens, and keep it fit while the window resizes
  // until the person zooms or pans by hand.
  $effect(() => {
    if (!open || !canvasEl) return;
    queueMicrotask(fit);
    const observer = new ResizeObserver(() => {
      if (!view.userZoomed) fit();
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
    use:airspaceSurface
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
    role="dialog"
    aria-modal="true"
    aria-label="Diagram Viewer"
    tabindex="-1"
  >
    <div
      use:focusTrap={{ active: open }}
      class="relative w-[95vw] h-[95vh] bg-surface-1 border border-border-subtle rounded-[var(--radius-card)] shadow-modal overflow-hidden flex flex-col"
      tabindex="-1"
    >
      <div
        bind:this={canvasEl}
        class={[
          'flex-1 relative overflow-hidden select-none touch-none',
          view.panning ? 'cursor-grabbing' : 'cursor-grab',
        ].join(' ')}
        oncontextmenu={(e) => {
          // The transform host carries `pointer-events-none` so pans
          // reach the canvas, which means `e.target.closest('svg')`
          // would miss: the target is the canvas div. Query the SVG
          // explicitly from the host and hand it to the caller.
          const svg = diagramSvg();
          if (svg) onContextMenu?.(e, svg);
        }}
        onwheel={(e) => view.onWheel(e)}
        onpointerdown={(e) => view.onPointerDown(e)}
        onpointermove={(e) => view.onPointerMove(e)}
        onpointerup={(e) => view.onPointerUp(e)}
        onpointercancel={(e) => view.onPointerUp(e)}
        ondblclick={(e) => {
          const content = contentSize();
          if (content) view.toggle(e.clientX, e.clientY, content);
        }}
      >
        <div
          bind:this={transformHostEl}
          class="absolute top-0 left-0 pointer-events-none"
          style:transform={view.transform}
          style:transform-origin="0 0"
        >
          {@html displayHtml}
        </div>
      </div>

      <div
        class="flex items-center justify-end gap-1 px-3 py-1.5 border-t border-border bg-surface-0 text-xs"
      >
        <span class="mr-2 tabular-nums text-text-secondary" aria-live="polite">
          {Math.round(view.scale * 100)}%
        </span>
        <button
          class="px-2 py-1 text-text-secondary hover:text-text-primary"
          onclick={() => view.zoomCenter(1 / ZOOM_STEP)}
          aria-label="Zoom Out"
        >
          −
        </button>
        <button
          class="px-2 py-1 text-text-secondary hover:text-text-primary"
          onclick={fit}
          aria-label="Fit to View"
        >
          Fit
        </button>
        <button
          class="px-2 py-1 text-text-secondary hover:text-text-primary"
          onclick={() => view.zoomCenter(ZOOM_STEP)}
          aria-label="Zoom In"
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
