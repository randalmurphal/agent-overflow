<script lang="ts">
  /**
   * Singleton footnote-body popup.
   *
   * `[^1]` renders as a chip inline with the prose; the `[^1]: body`
   * definition renders nowhere (the parser drops the block). This host
   * is the only place the body is shown: delegated `click` and
   * `pointerover`/`pointerout` listeners on `document` pick up every
   * footnote chip in every mounted markdown surface, so rows allocate no
   * popup instances and no per-chip handlers — the same memory model as
   * `DiagramInteractionHost`. Hover previews, click pins (the contract is
   * documented at the hover-state fields below).
   *
   * The seam is `data-footnote-label` on the chip (markdown/AGENTS.md
   * § Host seams): the renderer publishes which footnote the chip refers
   * to and stops there. Resolving that label to a body is document-level work
   * the render path cannot do, and `markdown/footnoteDefinitions.ts`
   * owns it — lazily, on this click, never during render. A label the
   * document never defines resolves to nothing and the chip stays inert.
   *
   * Mounted once from `App.svelte`. State is three fields, and the body
   * markdown is only re-rendered while the popup is open.
   *
   * Positioning is `primitives/Popover.svelte`, not a row-local overlay:
   * it portals the floating element to `<body>`, which is what keeps the
   * popup out of the virtualizer row's containment scope (a
   * `position: fixed` overlay inside `contain: paint` positions against
   * the row, not the viewport — the failure that killed the library's
   * floating-ui popover). It also inherits the app's clip-boundary
   * resolution, so a popup whose chip scrolls behind the sidebar is cut
   * at the same edge its chip is, and closes when the chip is gone.
   */

  import { onMount } from 'svelte';
  import Popover from '../primitives/Popover.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import {
    resolveFootnoteBody,
    resolveFootnoteBodyAt,
  } from './markdown/footnoteDefinitions';
  import {
    popoverCloseRestoresFocus,
    type PopoverCloseReason,
  } from '../../utils/popoverOwnership';

  let anchor: HTMLElement | undefined = $state(undefined);
  let body = $state('');
  let label = $state('');
  // The document root the OPEN popup's chip came from. A footnote body can
  // itself reference another footnote; that chained chip's nearest
  // `.markdown-body` is the popup's own, whose registered source is just
  // the body on display, so chained refs resolve against this root instead.
  let documentRoot: HTMLElement | undefined;

  // Hover preview (the Wikipedia reference-preview contract): resting the
  // pointer on a chip opens the popup after a short delay, and leaving both
  // the chip and the popup closes it after a grace window long enough to
  // travel from one into the other. A CLICK pins the popup — pointer-leave
  // then stops closing it and only the usual dialog exits apply (chip
  // toggle, outside click, Escape). Touch and keyboard never enter the
  // hover path: their pointerover carries `pointerType: 'touch'` or does
  // not fire at all, and the click path below serves both.
  let pinned = false;
  let openTimer: ReturnType<typeof setTimeout> | undefined;
  let closeTimer: ReturnType<typeof setTimeout> | undefined;
  let pendingChip: HTMLElement | undefined;
  const HOVER_OPEN_DELAY_MS = 200;
  const HOVER_CLOSE_GRACE_MS = 300;

  function cancelPendingOpen(): void {
    if (openTimer !== undefined) {
      clearTimeout(openTimer);
      openTimer = undefined;
    }
    pendingChip = undefined;
  }

  function cancelPendingClose(): void {
    if (closeTimer !== undefined) {
      clearTimeout(closeTimer);
      closeTimer = undefined;
    }
  }

  function close(reason?: PopoverCloseReason): void {
    pinned = false;
    cancelPendingClose();
    const chip = anchor;
    // Read focus BEFORE the state clear, while the floating element is
    // still mounted: this is the one moment `activeElement` can say
    // whether the close is about to strand focus on a removed node.
    // Popover's own `restoreFocusTo` cannot serve us, because it reads
    // the prop after the close and we drop the anchor reference on every
    // close — a closed popup must never pin a detached virtualizer row.
    const stranded =
      document.activeElement instanceof HTMLElement &&
      document.activeElement.closest('[data-popover]') !== null;
    anchor = undefined;
    body = '';
    label = '';
    documentRoot = undefined;
    // The chip declares `aria-haspopup` for every reference (the renderer
    // cannot know which ones have a definition); the OPEN state is only
    // knowable here, so this host owns that half of the trigger contract.
    chip?.removeAttribute('aria-expanded');
    if (stranded && popoverCloseRestoresFocus(reason) && chip?.isConnected) {
      chip.focus({ preventScroll: true });
    }
  }

  function openFor(chip: HTMLElement): boolean {
    const definition = resolveFootnoteBody(chip);
    // No definition for this label: the chip stays the inert marker it
    // has always been rather than opening an empty popup.
    if (!definition) return false;
    // Opening a second chip while one is open: the first chip's own
    // `aria-expanded` has to come off, and `close()` is where that lives.
    if (anchor !== undefined) close();
    anchor = chip;
    body = definition;
    label = chip.dataset.footnoteLabel ?? '';
    documentRoot = chip.closest<HTMLElement>('.markdown-body') ?? undefined;
    chip.setAttribute('aria-expanded', 'true');
    return true;
  }

  function handleClick(e: MouseEvent): void {
    if (!(e.target instanceof Element)) return;
    const chip = e.target.closest<HTMLElement>('[data-streamdown-footnote-ref]');
    if (!chip) return;
    // A chained ref inside the open popup navigates the popup in place:
    // same anchor (the popup must not re-anchor to a chip inside itself —
    // swapping the body unmounts that chip and Popover closes on a gone
    // anchor), new body resolved against the original document. Navigating
    // is engagement, so it also pins a hover-opened popup.
    if (documentRoot !== undefined && chip.closest('[data-footnote-popover]')) {
      const chained = chip.dataset.footnoteLabel;
      const chainedBody = chained
        ? resolveFootnoteBodyAt(documentRoot, chained)
        : null;
      if (chainedBody) {
        body = chainedBody;
        label = chained!;
        pinned = true;
      }
      return;
    }
    // A click on the open chip: a hover-opened popup gets pinned (the
    // click is the "keep it" gesture), a pinned one closes. The toggle
    // runs BEFORE the resolve — Popover's outside-mousedown handler
    // deliberately ignores clicks inside the anchor, and resolving first
    // would leave the dialog unclosable from its own chip if a source
    // rewrite removed the open popup's definition.
    if (anchor === chip) {
      if (!pinned) {
        pinned = true;
        cancelPendingClose();
        return;
      }
      close();
      return;
    }
    // Only a click that actually OPENED gets to pin: a definition-less
    // chip must not demote an already-pinned popup to hover lifetime.
    if (openFor(chip)) pinned = true;
  }

  function handlePointerOver(e: PointerEvent): void {
    if (e.pointerType === 'touch') return;
    if (!(e.target instanceof Element)) return;
    // Inside the popup (including a chained chip — hover never navigates,
    // only keeps): the pointer arrived, so any pending grace-close stops.
    // Gated on an open popup so the idle-app path pays one `closest` below,
    // not two.
    if (anchor !== undefined && e.target.closest('[data-footnote-popover]')) {
      cancelPendingClose();
      return;
    }
    const chip = e.target.closest<HTMLElement>('[data-streamdown-footnote-ref]');
    if (!chip) return;
    if (anchor === chip) {
      cancelPendingClose();
      return;
    }
    // A pinned popup ignores stray hovers over other chips.
    if (pinned || pendingChip === chip) return;
    cancelPendingOpen();
    pendingChip = chip;
    openTimer = setTimeout(() => {
      openTimer = undefined;
      const target = pendingChip;
      pendingChip = undefined;
      if (!target?.isConnected || pinned || anchor === target) return;
      openFor(target);
    }, HOVER_OPEN_DELAY_MS);
  }

  function handlePointerOut(e: PointerEvent): void {
    // Nothing pending and nothing hover-closable: the idle path exits
    // before any DOM walk.
    if (pendingChip === undefined && (anchor === undefined || pinned)) return;
    if (e.pointerType === 'touch') return;
    if (!(e.target instanceof Element)) return;
    const to = e.relatedTarget instanceof Element ? e.relatedTarget : null;
    // Leaving the chip a hover-open is pending on cancels it.
    if (
      pendingChip !== undefined &&
      e.target.closest('[data-streamdown-footnote-ref]') === pendingChip &&
      (!to || to.closest('[data-streamdown-footnote-ref]') !== pendingChip)
    ) {
      cancelPendingOpen();
    }
    if (anchor === undefined || pinned) return;
    const inHoverSurface = (el: Element | null): boolean =>
      el !== null &&
      (el.closest('[data-footnote-popover]') !== null ||
        el.closest('[data-streamdown-footnote-ref]') === anchor);
    // Only a departure FROM the chip or the popup starts the grace clock,
    // and only when the pointer is not landing back inside either.
    if (!inHoverSurface(e.target) || inHoverSurface(to)) return;
    cancelPendingClose();
    closeTimer = setTimeout(() => {
      closeTimer = undefined;
      if (!pinned) close();
    }, HOVER_CLOSE_GRACE_MS);
  }

  onMount(() => {
    document.addEventListener('click', handleClick);
    document.addEventListener('pointerover', handlePointerOver);
    document.addEventListener('pointerout', handlePointerOut);
    return () => {
      document.removeEventListener('click', handleClick);
      document.removeEventListener('pointerover', handlePointerOver);
      document.removeEventListener('pointerout', handlePointerOut);
      cancelPendingOpen();
      cancelPendingClose();
    };
  });
</script>

<Popover
  {anchor}
  open={anchor !== undefined}
  onClose={close}
  placement="bottom-start"
  role="dialog"
  ariaLabel={label ? `Footnote ${label}` : 'Footnote'}
>
  {#snippet children()}
    <div
      data-footnote-popover
      class="bg-surface-1 border border-border-subtle rounded-[var(--radius-control)] shadow-menu px-3 py-2 max-w-[min(32rem,80vw)]"
    >
      <ChatMarkdown source={body} />
    </div>
  {/snippet}
</Popover>
