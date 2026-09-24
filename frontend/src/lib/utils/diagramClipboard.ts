/**
 * Clipboard helpers for mermaid diagrams.
 *
 * Three entry points. Each resolves only once the clipboard actually holds
 * the content, and otherwise throws an `Error` whose message is fit for a
 * toast. The PNG write and its engine constraints (synchronous `write`
 * reach, PNG as the only image flavour, the secure-context requirement)
 * live in `pngClipboard.ts`, shared with the attachment image copy.
 *
 * Two diagram-specific constraints shape the rest.
 *
 * 1. SVG goes on the clipboard as TEXT. `image/svg+xml` is not a writable
 *    clipboard flavour on any engine we ship to, and text is also the
 *    flavour anything consuming a pasted SVG (editor, `.svg` file, Figma)
 *    reads.
 *
 * 2. The element handed in is the LIVE diagram: mermaid's own `<svg>`
 *    nested inside the markdown tree's outer `svg[data-mermaid-svg]`
 *    host, emitted with `width="100%"` and an inline `max-width`
 *    (`useMaxWidth`). Serialising that verbatim exports a root with no
 *    intrinsic size, which rasterises blank or cropped.
 *    `exportableDiagram` descends to the diagram root and restores real
 *    dimensions, the same normalisation `DiagramModal.normalizeSvg`
 *    performs for on-screen display.
 */

import { reportCopyFailure } from './clipboard';
import { errString } from './errors';
import { rasteriseSvg, requireClipboard, writePngToClipboard } from './pngClipboard';

const SVG_NS = 'http://www.w3.org/2000/svg';

type ExportableDiagram = { markup: string; width: number; height: number };

export async function copyAsPNG(svg: SVGSVGElement): Promise<void> {
  await writePngToClipboard(() => rasterise(exportableDiagram(svg)), 'Could not copy the diagram as PNG');
}

export async function copyAsSVG(svg: SVGSVGElement): Promise<void> {
  await writeText(() => exportableDiagram(svg).markup, 'Could not copy the diagram as SVG');
}

export async function copySource(source: string): Promise<void> {
  await writeText(() => {
    if (!source) throw new Error('this diagram carries no source text');
    return source;
  }, 'Could not copy the diagram source');
}

/**
 * `clipboard.writeText` with the failure re-thrown under a caller-supplied
 * label. `text` is a thunk so its work also runs inside the gesture task
 * and its throws are reported the same way.
 */
async function writeText(text: () => string, failure: string): Promise<void> {
  try {
    const clipboard = requireClipboard('writeText');
    await clipboard.writeText(text());
  } catch (err) {
    reportCopyFailure(`${failure}:`, err);
    throw new Error(`${failure}: ${errString(err)}`);
  }
}

/**
 * A standalone copy of the diagram: document-independent markup plus the
 * pixel size to rasterise it at. See note 2 for what is being undone.
 */
function exportableDiagram(live: SVGSVGElement): ExportableDiagram {
  // The markdown tree renders mermaid's own `<svg>` INSIDE its outer
  // `<svg data-mermaid-svg>` host, and the host is what the context menu
  // resolves. Descend to the diagram root before exporting.
  const root = Array.from(live.children).find(isSvgElement) ?? live;
  const { width, height } = intrinsicDimensions(root, live);

  const clone = root.cloneNode(true) as SVGSVGElement;
  // Mermaid's root `style` holds only `max-width` (its real styling is in
  // a `<style>` child), so dropping the attribute wholesale is both safe
  // and the whole fix.
  clone.removeAttribute('style');
  clone.setAttribute('width', String(width));
  clone.setAttribute('height', String(height));
  if (!clone.hasAttribute('viewBox')) {
    clone.setAttribute('viewBox', `0 0 ${width} ${height}`);
  }

  // The serialiser emits the SVG namespace declaration itself for an
  // element in that namespace; the guard covers a root that somehow is
  // not, and is a string check because `setAttribute('xmlns', …)` would
  // add a second, non-namespaced attribute of the same name.
  let markup = new XMLSerializer().serializeToString(clone);
  if (!markup.includes(`xmlns="${SVG_NS}"`)) {
    markup = markup.replace('<svg', `<svg xmlns="${SVG_NS}"`);
  }
  return { markup, width, height };
}

function isSvgElement(el: Element): el is SVGSVGElement {
  return el.namespaceURI === SVG_NS && el.localName === 'svg';
}

// Prefer the viewBox: it is in user-space units, so unlike a layout box
// it does not carry any page-side scaling.
function intrinsicDimensions(
  root: SVGSVGElement,
  live: SVGSVGElement,
): { width: number; height: number } {
  const vb = root.viewBox?.baseVal;
  if (vb && vb.width > 0 && vb.height > 0) {
    return { width: vb.width, height: vb.height };
  }
  const rect = live.getBoundingClientRect();
  if (rect.width > 0 && rect.height > 0) {
    return { width: rect.width, height: rect.height };
  }
  return { width: 800, height: 600 };
}

/**
 * Rasterise the exportable markup at its intrinsic size (at 2x or more, see
 * `rasteriseSvg`) so the pasted image stays crisp when scaled in docs,
 * Slack, etc.
 */
function rasterise({ markup, width, height }: ExportableDiagram): Promise<Blob> {
  return rasteriseSvg(markup, { width, height }, {
    decode: 'the diagram SVG could not be decoded as an image',
    encode: 'the diagram could not be encoded as a PNG',
  });
}
