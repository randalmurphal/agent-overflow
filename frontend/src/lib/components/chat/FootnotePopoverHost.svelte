<script lang="ts">
  /**
   * Singleton footnote-body popup.
   *
   * `[^1]` renders as a chip inline with the prose; the `[^1]: body`
   * definition renders nowhere (the parser drops the block — see
   * `vendor/svelte-streamdown/DIVERGENCE.md` entry 28). This host is the
   * only place the body is shown: one delegated `click` listener on
   * `document` picks up every footnote chip in every mounted markdown
   * surface, so rows allocate no popup instances and no per-chip
   * handlers — the same memory model as `DiagramInteractionHost`.
   *
   * The seam is `data-footnote-label` on the chip (divergence 29): the
   * vendored renderer publishes which footnote the chip refers to and
   * stops there. Resolving that label to a body is document-level work
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
   * the row, not the viewport — the failure that killed the vendored
   * floating-ui popover). It also inherits the app's clip-boundary
   * resolution, so a popup whose chip scrolls behind the sidebar is cut
   * at the same edge its chip is, and closes when the chip is gone.
   */

  import { onMount } from 'svelte';
  import Popover from '../primitives/Popover.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import { resolveFootnoteBody } from './markdown/footnoteDefinitions';
  import {
    popoverCloseRestoresFocus,
    type PopoverCloseReason,
  } from '../../utils/popoverOwnership';

  let anchor: HTMLElement | undefined = $state(undefined);
  let body = $state('');
  let label = $state('');

  function close(reason?: PopoverCloseReason): void {
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
    // The chip declares `aria-haspopup` for every reference (the renderer
    // cannot know which ones have a definition); the OPEN state is only
    // knowable here, so this host owns that half of the trigger contract.
    chip?.removeAttribute('aria-expanded');
    if (stranded && popoverCloseRestoresFocus(reason) && chip?.isConnected) {
      chip.focus({ preventScroll: true });
    }
  }

  function handleClick(e: MouseEvent): void {
    if (!(e.target instanceof Element)) return;
    const chip = e.target.closest<HTMLElement>('[data-streamdown-footnote-ref]');
    if (!chip) return;
    const definition = resolveFootnoteBody(chip);
    // No definition for this label: the chip stays the inert marker it
    // has always been rather than opening an empty popup.
    if (!definition) return;
    // A second click on the open chip closes it. Popover's outside-
    // mousedown handler deliberately ignores clicks inside the anchor,
    // so the toggle has to live here.
    if (anchor === chip) {
      close();
      return;
    }
    // Opening a second chip while one is open: the first chip's own
    // `aria-expanded` has to come off, and `close()` is where that lives.
    if (anchor !== undefined) close();
    anchor = chip;
    body = definition;
    label = chip.dataset.footnoteLabel ?? '';
    chip.setAttribute('aria-expanded', 'true');
  }

  onMount(() => {
    document.addEventListener('click', handleClick);
    return () => {
      document.removeEventListener('click', handleClick);
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
