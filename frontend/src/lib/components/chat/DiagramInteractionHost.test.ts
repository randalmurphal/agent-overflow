// The host's job on a copy action: pick the right module entry point for
// the menu row, and turn its outcome into a VISIBLE toast. Silent failure
// is the defect this covers — a success toast may only fire when the copy
// actually resolved.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import DiagramInteractionHost from './DiagramInteractionHost.svelte';
import { copyAsPNG, copyAsSVG, copySource } from '../../utils/diagramClipboard';
import { getToasts, removeToast } from '../../stores/toast.svelte';

vi.mock('../../utils/diagramClipboard', () => ({
  copyAsPNG: vi.fn(async () => {}),
  copyAsSVG: vi.fn(async () => {}),
  copySource: vi.fn(async () => {}),
}));

const NS = 'http://www.w3.org/2000/svg';

function mountDiagram(source: string): SVGSVGElement {
  const wrapper = document.createElement('div');
  wrapper.setAttribute('data-mermaid-source', source);
  const svg = document.createElementNS(NS, 'svg') as SVGSVGElement;
  svg.setAttribute('data-mermaid-svg', '');
  svg.setAttribute('viewBox', '0 0 100 50');
  wrapper.appendChild(svg);
  document.body.appendChild(wrapper);
  return svg;
}

async function openMenuOn(svg: SVGSVGElement): Promise<void> {
  await fireEvent(svg, new MouseEvent('contextmenu', { bubbles: true, cancelable: true }));
  await tick();
}

async function pick(label: string): Promise<void> {
  const item = Array.from(document.querySelectorAll('[role="menuitem"]')).find(
    (el) => el.textContent?.trim() === label,
  );
  if (!item) throw new Error(`no menu item labelled "${label}"`);
  await fireEvent.click(item);
  // Two flushes: the copy promise settles, then the toast state renders.
  await tick();
  await tick();
}

function toastMessages(): string[] {
  return getToasts().map((t) => `${t.type}: ${t.message}`);
}

describe('<DiagramInteractionHost> copy actions', () => {
  beforeEach(() => {
    vi.mocked(copyAsPNG).mockReset().mockResolvedValue(undefined);
    vi.mocked(copyAsSVG).mockReset().mockResolvedValue(undefined);
    vi.mocked(copySource).mockReset().mockResolvedValue(undefined);
    for (const t of [...getToasts()]) removeToast(t.id);
    document.body.innerHTML = '';
  });

  it('routes each row to its own entry point and confirms with a toast', async () => {
    render(DiagramInteractionHost);
    const svg = mountDiagram('graph TD\nA-->B');

    await openMenuOn(svg);
    await pick('Copy as PNG');
    expect(copyAsPNG).toHaveBeenCalledWith(svg);
    expect(toastMessages()).toEqual(['success: Diagram copied as PNG']);

    await openMenuOn(svg);
    await pick('Copy as SVG');
    expect(copyAsSVG).toHaveBeenCalledWith(svg);
    expect(toastMessages()).toContain('success: Diagram copied as SVG');

    await openMenuOn(svg);
    await pick('Copy Source');
    expect(copySource).toHaveBeenCalledWith('graph TD\nA-->B');
    expect(toastMessages()).toContain('success: Diagram source copied');
  });

  it('surfaces a copy failure as an error toast and no success toast', async () => {
    vi.mocked(copyAsPNG).mockRejectedValue(
      new Error('Could not copy the diagram as PNG: Document is not focused.'),
    );
    render(DiagramInteractionHost);

    await openMenuOn(mountDiagram('graph TD'));
    await pick('Copy as PNG');

    expect(toastMessages()).toEqual([
      'error: Could not copy the diagram as PNG: Document is not focused.',
    ]);
  });

  it('names the substitute when the expanded view has no source to copy', async () => {
    // The modal holds only the rendered SVG, so "Copy Source" there falls
    // back to the markup — and the toast says so rather than claiming the
    // reader got the mermaid source.
    render(DiagramInteractionHost);
    await openMenuOn(mountDiagram('graph TD'));
    await pick('Expand');

    const modalSvg = document
      .querySelector('[data-diagram-modal-backdrop]')
      ?.querySelector('svg');
    expect(modalSvg).toBeTruthy();
    await openMenuOn(modalSvg as SVGSVGElement);
    await pick('Copy Source');

    expect(copySource).not.toHaveBeenCalled();
    expect(copyAsSVG).toHaveBeenCalledTimes(1);
    expect(toastMessages()).toEqual(['success: Diagram copied as SVG']);
  });

  it('calls the clipboard entry point synchronously with the click', async () => {
    // Nothing may be awaited between the click and the clipboard call:
    // WebKit drops the user gesture across an await and Chromium's
    // transient activation can expire.
    render(DiagramInteractionHost);
    const svg = mountDiagram('graph TD');
    await openMenuOn(svg);

    const item = Array.from(document.querySelectorAll('[role="menuitem"]')).find(
      (el) => el.textContent?.trim() === 'Copy as PNG',
    ) as HTMLElement;
    item.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(copyAsPNG).toHaveBeenCalledTimes(1);
  });
});
