<script lang="ts">
  /**
   * Top-level host for mermaid diagram interactions: right-click
   * context menu + full-viewport expand modal + clipboard actions.
   *
   * Mounted once from `App.svelte`; owns all interaction state so the
   * mermaid painter (DOM-only, called from
   * `lazyCompleteSourceRenderer`) doesn't need to know about Svelte.
   * A single delegated `contextmenu` listener on `document` handles
   * right-clicks on any rendered mermaid block — matches the
   * `codeCopy.ts` delegation pattern and costs zero memory per
   * diagram.
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
  import { getCachedSource } from '../../utils/lazyCompleteSourceRenderer';
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
  // that lands on a rendered mermaid block. The painter writes
  // `data-rendered-mermaid` on successfully-rendered `<pre class="mermaid">`
  // elements; the attribute value is the cached source hash.
  function handleInlineContextMenu(e: MouseEvent): void {
    if (!(e.target instanceof Element)) return;
    const pre = e.target.closest<HTMLElement>('pre.mermaid[data-rendered-mermaid]');
    if (!pre) return;
    const svg = pre.querySelector<SVGSVGElement>('svg');
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
        // The inline painter caches source by hash. The modal copy of
        // the SVG doesn't carry the idempotency attribute, so we fall
        // back to serialising the SVG source when the hash lookup
        // can't find the original mermaid text (e.g. user copies from
        // inside the modal).
        const pre = current.svg.closest<HTMLElement>('pre.mermaid[data-rendered-mermaid]');
        const raw = pre ? getCachedSource('mermaid', pre) : null;
        if (raw) {
          result = await copySource(raw);
        } else {
          result = await copyAsSVG(current.svg);
        }
        break;
      }
      case 'expand': {
        const pre = current.svg.closest<HTMLElement>('pre.mermaid');
        if (pre) modalHtml = pre.innerHTML;
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
    setTimeout(() => {
      announcement = '';
    }, 1500);
  }

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
