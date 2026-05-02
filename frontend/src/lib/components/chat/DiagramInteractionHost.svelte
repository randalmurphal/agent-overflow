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

  import { onMount, onDestroy } from 'svelte';
  import DiagramModal from './DiagramModal.svelte';
  import DiagramContextMenu, {
    type DiagramAction,
  } from './DiagramContextMenu.svelte';
  import {
    copyAsPNG,
    copyAsSVG,
    copySource,
    type CopyResult,
  } from '../../utils/diagramClipboard';

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
  // a few levels in (Streamdown's Mermaid creates its own container +
  // panzoom svg).
  function handleInlineContextMenu(e: MouseEvent): void {
    if (!(e.target instanceof Element)) return;
    const host = e.target.closest<HTMLElement>('[data-mermaid-source]');
    if (!host) return;
    const svg = host.querySelector<SVGSVGElement>('svg');
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

  async function runAction(action: DiagramAction): Promise<void> {
    if (!menu) return;
    const current = menu;
    menu = null;

    let result: CopyResult | null = null;
    switch (action) {
      case 'copy-png':
        result = await copyAsPNG(current.svg);
        break;
      case 'copy-svg':
        result = await copyAsSVG(current.svg);
        break;
      case 'copy-source': {
        // The inline wrapper carries the original Mermaid source on
        // a `data-mermaid-source` attribute (see StreamdownMermaidHost).
        // The modal copy doesn't have this — it only holds the
        // already-rendered SVG — so it falls back to SVG.
        const host = current.svg.closest<HTMLElement>('[data-mermaid-source]');
        const raw = host?.dataset.mermaidSource ?? null;
        if (raw) {
          result = await copySource(raw);
        } else {
          result = await copyAsSVG(current.svg);
        }
        break;
      }
      case 'expand': {
        const host = current.svg.closest<HTMLElement>('[data-mermaid-source]');
        if (host) modalHtml = host.innerHTML;
        break;
      }
      case 'close':
        modalHtml = '';
        break;
    }
    if (result !== null) announce(result);
  }

  // Lightweight toast substitute — the app doesn't have a global
  // toast store today, so we surface success/failure via a hidden
  // aria-live region so screen readers still get feedback. Upgrade
  // to a real toast store in one place if/when we add it.
  let announcement: string = $state('');
  // Tracked handle so repeat announcements don't leave a second timer
  // firing after unmount. The host is a singleton in the app shell, so
  // the leak window is small — but still worth the five extra lines.
  let announceTimer: ReturnType<typeof setTimeout> | null = null;
  function announce(result: CopyResult): void {
    announcement = (
      result === 'png'
        ? 'Diagram copied as PNG'
        : result === 'svg'
          ? 'Diagram copied as SVG'
          : result === 'text'
            ? 'Diagram copied as text'
            : 'Copy failed'
    );
    // Clear after a short delay so repeated announcements re-fire.
    if (announceTimer) clearTimeout(announceTimer);
    announceTimer = setTimeout(() => {
      announcement = '';
      announceTimer = null;
    }, 1500);
  }

  onDestroy(() => {
    if (announceTimer) clearTimeout(announceTimer);
  });

  function dismissMenu(): void {
    menu = null;
  }

  function closeModal(): void {
    modalHtml = '';
  }

  onMount(() => {
    document.addEventListener('contextmenu', handleInlineContextMenu);
    return () => {
      document.removeEventListener('contextmenu', handleInlineContextMenu);
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

<!-- Screen-reader-only live region for copy outcome. -->
<div class="sr-only" aria-live="polite" role="status">{announcement}</div>
