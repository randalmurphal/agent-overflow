import { render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import ChatMarkdown from './ChatMarkdown.svelte';
import FootnotePopoverHost from './FootnotePopoverHost.svelte';

// The reason this popup is not built inside the renderer, pinned with real
// geometry. The deleted floating-ui popover was a `position: fixed`
// element rendered INSIDE the row; a timeline row is containment-scoped, and
// `contain: paint` makes the row a containing block for fixed descendants,
// so the popup positioned against the row instead of the viewport and landed
// off-screen. happy-dom reports zero geometry and cannot see any of that.
//
// `primitives/Popover.svelte` portals its floating element to <body>, which
// is what makes the popup immune to whatever the row contains. What is
// asserted here is the OUTCOME: the popup escapes the containment scope, and
// it lands beside the chip that opened it.

const scopes: HTMLElement[] = [];

afterEach(() => {
  for (const scope of scopes.splice(0)) scope.remove();
});

/** A containment-scoped, scrolled stand-in for a virtualized timeline row. */
function containmentScope(): HTMLElement {
  const scope = document.createElement('div');
  scope.style.contain = 'paint';
  scope.style.overflow = 'hidden';
  scope.style.width = '420px';
  scope.style.height = '160px';
  scope.style.marginTop = '220px';
  document.body.append(scope);
  scopes.push(scope);
  return scope;
}

describe('footnote popup geometry', () => {
  it('escapes the row containment scope and anchors to its chip', async () => {
    render(FootnotePopoverHost);
    const scope = containmentScope();
    const { container } = render(ChatMarkdown, {
      props: {
        source: 'A claim[^note] worth checking.\n\n[^note]: The supporting body.',
      },
      // Render the markdown INSIDE the containment scope, the way a
      // timeline row hosts it.
      target: scope,
    });

    const chip = await waitFor(() => {
      const found = container.querySelector<HTMLElement>(
        '[data-streamdown-footnote-ref]',
      );
      expect(found).not.toBeNull();
      return found!;
    });

    chip.click();

    const popup = await waitFor(() => {
      const found = document.body.querySelector<HTMLElement>(
        '[data-footnote-popover]',
      );
      expect(found).not.toBeNull();
      return found!;
    });

    // Portaled out: the popup is not a descendant of the containment scope,
    // which is what stops the scope from becoming its containing block.
    expect(scope.contains(popup)).toBe(false);

    const chipRect = chip.getBoundingClientRect();
    const floating = popup.closest<HTMLElement>('[data-popover]')!;
    await waitFor(() => {
      expect(floating.style.visibility).not.toBe('hidden');
    });
    const popupRect = floating.getBoundingClientRect();

    // On screen, with real size — the off-screen failure this replaced put
    // the panel at the row's origin or outside the viewport entirely.
    expect(popupRect.width).toBeGreaterThan(0);
    expect(popupRect.height).toBeGreaterThan(0);
    expect(popupRect.top).toBeGreaterThanOrEqual(0);
    expect(popupRect.left).toBeGreaterThanOrEqual(0);
    expect(popupRect.bottom).toBeLessThanOrEqual(window.innerHeight + 1);
    expect(popupRect.right).toBeLessThanOrEqual(window.innerWidth + 1);

    // Anchored to the chip rather than to the row or the viewport corner:
    // the preferred placement is bottom-start, so the panel's left edge
    // tracks the chip's and it sits below it.
    expect(Math.abs(popupRect.left - chipRect.left)).toBeLessThan(2);
    expect(popupRect.top).toBeGreaterThanOrEqual(chipRect.bottom);
    expect(popupRect.top - chipRect.bottom).toBeLessThan(12);

    // And the body is rendered markdown, sized by its own content.
    expect(popup.textContent).toContain('The supporting body.');
  });

  it('navigates a chained footnote ref against the original document', async () => {
    render(FootnotePopoverHost);
    const scope = containmentScope();
    const { container } = render(ChatMarkdown, {
      props: {
        source:
          'A claim[^a] worth checking.\n\n' +
          '[^a]: See also[^b] for details.\n' +
          '[^b]: The chained body.',
      },
      target: scope,
    });

    const chip = await waitFor(() => {
      const found = container.querySelector<HTMLElement>(
        '[data-streamdown-footnote-ref]',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    chip.click();

    const popup = await waitFor(() => {
      const found = document.body.querySelector<HTMLElement>(
        '[data-footnote-popover]',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    expect(popup.textContent).toContain('See also');

    // The `[^b]` chip inside the popup body: its nearest `.markdown-body`
    // is the popup's own, whose registered source is just the body on
    // display — the chained lookup must reach back to the document root.
    const chained = await waitFor(() => {
      const found = popup.querySelector<HTMLElement>(
        '[data-streamdown-footnote-ref]',
      );
      expect(found).not.toBeNull();
      return found!;
    });
    chained.click();

    // Same popup (same anchor), new body.
    await waitFor(() => {
      expect(popup.textContent).toContain('The chained body.');
    });
    expect(chip.getAttribute('aria-expanded')).toBe('true');
  });
});

// The hover contract: resting the pointer on a chip opens the popup after
// a short delay; leaving both the chip and the popup closes it after a
// grace window (long enough to travel from one into the other); a click
// PINS it, after which pointer-leave no longer closes it. Real timers —
// the delays are 200/300ms and the assertions are arrival/departure, not
// slot alignment.

function pointerOver(el: Element, relatedTarget: Element | null = null): void {
  el.dispatchEvent(
    new PointerEvent('pointerover', { bubbles: true, pointerType: 'mouse', relatedTarget }),
  );
}

function pointerOut(el: Element, relatedTarget: Element | null = null): void {
  el.dispatchEvent(
    new PointerEvent('pointerout', { bubbles: true, pointerType: 'mouse', relatedTarget }),
  );
}

async function hoverFixture(): Promise<HTMLElement> {
  render(FootnotePopoverHost);
  const scope = containmentScope();
  const { container } = render(ChatMarkdown, {
    props: {
      source: 'A claim[^note] worth checking.\n\n[^note]: The supporting body.',
    },
    target: scope,
  });
  return waitFor(() => {
    const found = container.querySelector<HTMLElement>(
      '[data-streamdown-footnote-ref]',
    );
    expect(found).not.toBeNull();
    return found!;
  });
}

const openPopup = (): HTMLElement | null =>
  document.body.querySelector<HTMLElement>('[data-footnote-popover]');

describe('footnote popup hover', () => {
  it('opens on hover after the delay, survives travel into the popup, closes on leave', async () => {
    const chip = await hoverFixture();

    pointerOver(chip);
    // The open is delayed — nothing appears synchronously.
    expect(openPopup()).toBeNull();
    const popup = await waitFor(() => {
      const found = openPopup();
      expect(found).not.toBeNull();
      return found!;
    });

    // Travel chip → popup: the departure schedules the grace close and the
    // arrival cancels it.
    pointerOut(chip, document.body);
    pointerOver(popup);
    await new Promise((r) => setTimeout(r, 450));
    expect(openPopup()).not.toBeNull();

    // Leaving the popup for unrelated ground closes after the grace.
    pointerOut(popup, document.body);
    await waitFor(() => {
      expect(openPopup()).toBeNull();
    });
    expect(chip.getAttribute('aria-expanded')).toBeNull();
  });

  it('a leave before the open delay cancels the pending open', async () => {
    const chip = await hoverFixture();
    pointerOver(chip);
    pointerOut(chip, document.body);
    await new Promise((r) => setTimeout(r, 350));
    expect(openPopup()).toBeNull();
  });

  it('a click pins a hover-opened popup so pointer-leave no longer closes it', async () => {
    const chip = await hoverFixture();
    pointerOver(chip);
    await waitFor(() => {
      expect(openPopup()).not.toBeNull();
    });

    chip.click(); // pin, not toggle-close
    expect(openPopup()).not.toBeNull();

    pointerOut(chip, document.body);
    await new Promise((r) => setTimeout(r, 450));
    expect(openPopup()).not.toBeNull();

    chip.click(); // the pinned popup's chip toggle still closes
    await waitFor(() => {
      expect(openPopup()).toBeNull();
    });
  });
});
