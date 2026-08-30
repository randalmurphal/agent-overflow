<script lang="ts">
  /**
   * Top-level host for mermaid diagram interactions: right-click
   * context menu + full-viewport expand modal + clipboard actions.
   *
   * Mounted once from `App.svelte`; owns all interaction state for
   * row-local Mermaid SVGs. A single delegated `contextmenu` listener
   * on `document` handles right-clicks on any rendered mermaid block,
   * so rows do not allocate per-diagram interaction handlers.
   *
   * Memory model: the menu holds four scalars (coords + context);
   * the modal holds the cached SVG string only while open. Clipboard
   * actions allocate one Canvas per PNG copy and release immediately.
   */

  import { onMount } from 'svelte';
  import DiagramModal from './DiagramModal.svelte';
  import DiagramContextMenu, {
    type DiagramAction,
  } from './DiagramContextMenu.svelte';
  import { copyAsPNG, copyAsSVG, copySource } from '../../utils/diagramClipboard';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';

  type MenuState = {
    x: number;
    y: number;
    svg: SVGSVGElement;
    context: 'inline' | 'modal';
  } | null;

  let menu: MenuState = $state(null);
  let modalHtml: string = $state('');
  let modalOpen: boolean = $derived(modalHtml.length > 0);

  // Inline delegated listener: filter every contextmenu event for one
  // that lands on a rendered mermaid block. The selector matches the
  // wrapper emitted by `StreamdownMermaidHost.svelte`, which stamps
  // `data-mermaid-source` on a `<div class="mermaid streamdown-mermaid-host">`
  // sitting around svelte-streamdown's Mermaid renderer. The SVG lives
  // a few levels in (Streamdown's Mermaid creates its own container and
  // an outer `svg[data-mermaid-svg]` host).
  function handleInlineContextMenu(e: MouseEvent): void {
    if (!(e.target instanceof Element)) return;
    const host = e.target.closest<HTMLElement>('[data-mermaid-source]');
    if (!host) return;
    const svg =
      host.querySelector<SVGSVGElement>('svg[data-mermaid-svg]') ??
      host.querySelector<SVGSVGElement>('svg');
    if (!svg) return;
    e.preventDefault();
    menu = { x: e.clientX, y: e.clientY, svg, context: 'inline' };
  }

  // Context menu raised from within the modal. The modal resolves its
  // own SVG reference (the event target is the canvas div because the
  // transform host is pointer-events-none) and hands it to us.
  function handleModalContextMenu(e: MouseEvent, svg: SVGSVGElement): void {
    e.preventDefault();
    menu = { x: e.clientX, y: e.clientY, svg, context: 'modal' };
  }

  function runAction(action: DiagramAction): void {
    if (!menu) return;
    const current = menu;
    menu = null;

    switch (action) {
      case 'copy-png':
        void report(copyAsPNG(current.svg), 'Diagram copied as PNG');
        return;
      case 'copy-svg':
        void report(copyAsSVG(current.svg), 'Diagram copied as SVG');
        return;
      case 'copy-source': {
        // The inline wrapper carries the original Mermaid source on a
        // `data-mermaid-source` attribute (see StreamdownMermaidHost).
        // The modal copy doesn't have it — it only holds the rendered
        // SVG — so it copies the markup instead, and says which it did.
        const host = current.svg.closest<HTMLElement>('[data-mermaid-source]');
        const raw = host?.dataset.mermaidSource;
        if (raw) void report(copySource(raw), 'Diagram source copied');
        else void report(copyAsSVG(current.svg), 'Diagram copied as SVG');
        return;
      }
      case 'expand': {
        const host = current.svg.closest<HTMLElement>('[data-mermaid-source]');
        const diagramSvg =
          host?.querySelector<SVGSVGElement>('svg[data-mermaid-svg]') ?? current.svg;
        modalHtml = diagramSvg.outerHTML;
        return;
      }
      case 'close':
        modalHtml = '';
        return;
    }
  }

  // Report a copy's outcome. It takes the in-flight promise rather than a
  // callback so the clipboard call is unmistakably made at the call site,
  // inside the click's user-gesture task — a thunk invoked here would be
  // one refactor away from being awaited first, which is what makes
  // WebKit reject the write. A copy never resolves without the clipboard
  // holding the content, so the success toast can't lie.
  async function report(copy: Promise<void>, success: string): Promise<void> {
    try {
      await copy;
      addToast('success', success);
    } catch (err) {
      addToast('error', errString(err));
    }
  }

  function dismissMenu(): void {
    menu = null;
  }

  function closeModal(): void {
    modalHtml = '';
  }

  function handleExpandEvent(e: Event): void {
    const detail = (e as CustomEvent<{ html: string }>).detail;
    if (detail?.html) modalHtml = detail.html;
  }

  onMount(() => {
    document.addEventListener('contextmenu', handleInlineContextMenu);
    document.addEventListener('diagram-expand', handleExpandEvent);
    return () => {
      document.removeEventListener('contextmenu', handleInlineContextMenu);
      document.removeEventListener('diagram-expand', handleExpandEvent);
    };
  });
</script>

<DiagramModal
  open={modalOpen}
  svgHtml={modalHtml}
  onClose={closeModal}
  onContextMenu={handleModalContextMenu}
/>

{#if menu}
  <DiagramContextMenu
    x={menu.x}
    y={menu.y}
    context={menu.context}
    onAction={runAction}
    onDismiss={dismissMenu}
  />
{/if}
