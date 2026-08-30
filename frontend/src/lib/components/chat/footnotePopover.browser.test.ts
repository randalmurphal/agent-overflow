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
});
