// Imperative DOM line renderer for a streaming StreamdownCodeHost <code> block.
//
// Keeps the exact DOM contract of the block (`<code>` holding only Text nodes
// and `<span class="syntax-...">` elements joined by real `\n` Text nodes, so
// native selection copy matches `token.text` byte-for-byte) while eliminating
// per-line and per-token Svelte keyed-each reactions, branch effects, comment
// anchor nodes, and `anchor.before()` churn on every streamed chunk or pushed
// highlight patch.

import { spanSegments, type EncodedLine } from '../../../utils/syntaxSpans';

/** The runs `spanSegments` colors with, or undefined for a plain line. */
function colorRuns(line: EncodedLine | null): number[] | undefined {
  const runs = line?.r;
  return runs && runs.length >= 2 ? runs : undefined;
}

/** Whether two span lines have the same color runs. */
function sameSpans(a: EncodedLine | null, b: EncodedLine | null): boolean {
  const x = colorRuns(a);
  const y = colorRuns(b);
  if (x === y) return true;
  if (!x || !y || x.length !== y.length) return false;
  for (let i = 0; i < x.length; i++) {
    if (x[i] !== y[i]) return false;
  }
  return true;
}

/** Total UTF-8 byte length covered by `line`'s color runs (0 when plain). */
function spanCoveredBytes(line: EncodedLine | null): number {
  const runs = colorRuns(line);
  if (!runs) return 0;
  let total = 0;
  for (let i = 0; i < runs.length; i += 2) {
    total += runs[i];
  }
  return total;
}

interface RenderedLine {
  text: string;
  spans: EncodedLine | null;
  /** Total UTF-8 bytes covered by `spans` (0 when plain). Because UTF-8 byte
   * length is always >= UTF-16 `.length`, any `prevLen >= spansByteLen` proves
   * an appended suffix sits strictly after all colored runs and can append to
   * the trailing plain Text node in O(1). */
  spansByteLen: number;
  /** The real `\n` Text node immediately preceding this line (null for line 0). */
  newlineNode: Text | null;
  /** Live child nodes of `<code>` belonging to this line, in document order. */
  nodes: (Text | HTMLSpanElement)[];
  dirty: boolean;
}

function makePlainLineNodes(doc: Document, text: string): (Text | HTMLSpanElement)[] {
  if (text.length === 0) return [];
  return [doc.createTextNode(text)];
}

function makeLineNodes(doc: Document, text: string, spans: EncodedLine | null): (Text | HTMLSpanElement)[] {
  if (text.length === 0) return [];
  if (!colorRuns(spans)) return [doc.createTextNode(text)];
  const segs = spanSegments(text, spans);
  const out: (Text | HTMLSpanElement)[] = [];
  for (let i = 0; i < segs.length; i++) {
    const seg = segs[i];
    if (seg.className) {
      const span = doc.createElement('span');
      span.className = seg.className;
      span.textContent = seg.text;
      out.push(span);
    } else {
      out.push(doc.createTextNode(seg.text));
    }
  }
  return out;
}

export class CodeDomRenderer {
  #el: HTMLElement | null = null;
  #lines: RenderedLine[] = [];

  constructor(initialText: string) {
    const parts = initialText.split('\n');
    const lines: RenderedLine[] = [];
    for (let i = 0; i < parts.length; i++) {
      lines.push({
        text: parts[i],
        spans: null,
        spansByteLen: 0,
        newlineNode: null,
        nodes: [],
        dirty: true,
      });
    }
    this.#lines = lines;
  }

  /** Attaches to the `<code>` element and renders all current lines in one pass. */
  mount(el: HTMLElement | undefined): void {
    const next = el ?? null;
    if (this.#el === next) return;
    this.#el = next;
    if (!next) {
      for (const line of this.#lines) {
        line.newlineNode = null;
        line.nodes = [];
        line.dirty = true;
      }
      return;
    }
    const doc = next.ownerDocument;
    next.textContent = '';
    const frag = doc.createDocumentFragment();
    for (let i = 0; i < this.#lines.length; i++) {
      const line = this.#lines[i];
      if (i > 0) {
        line.newlineNode = doc.createTextNode('\n');
        frag.appendChild(line.newlineNode);
      } else {
        line.newlineNode = null;
      }
      const nodes = makeLineNodes(doc, line.text, line.spans);
      line.nodes = nodes;
      for (let j = 0; j < nodes.length; j++) frag.appendChild(nodes[j]);
      line.dirty = false;
    }
    next.appendChild(frag);
  }

  /** Appends `delta` onto the trailing plain text of the last line when
   * `prevLen >= last.spansByteLen`, avoiding `spanSegments` and full-line DOM
   * patching on every streamed token after a live highlight push. */
  #appendLastLinePlainTail(el: HTMLElement, last: RenderedLine, delta: string): void {
    const nodes = last.nodes;
    const count = nodes.length;
    if (count > 0) {
      const tail = nodes[count - 1];
      if (tail.nodeType === 3) {
        (tail as Text).data += delta;
        last.dirty = false;
        return;
      }
    }
    const textNode = el.ownerDocument.createTextNode(delta);
    nodes.push(textNode);
    el.appendChild(textNode);
    last.dirty = false;
  }

  /** Extends the source by delta, touching only its last line and added lines. */
  append(delta: string): void {
    if (delta.length === 0) return;
    const el = this.#el;
    const lines = this.#lines;
    const firstNewline = delta.indexOf('\n');
    if (firstNewline === -1) {
      const last = lines[lines.length - 1];
      const prevLen = last.text.length;
      last.text += delta;
      if (!el) {
        last.dirty = true;
        return;
      }
      if (!last.dirty && prevLen >= last.spansByteLen) {
        this.#appendLastLinePlainTail(el, last, delta);
        return;
      }
      this.#patchLine(lines.length - 1);
      return;
    }

    const parts = delta.split('\n');
    const lastIdx = lines.length - 1;
    const last = lines[lastIdx];
    const head = parts[0];
    if (head.length > 0) {
      const prevLen = last.text.length;
      last.text += head;
      if (!el) {
        last.dirty = true;
      } else if (!last.dirty && prevLen >= last.spansByteLen) {
        this.#appendLastLinePlainTail(el, last, head);
      } else {
        this.#patchLine(lastIdx);
      }
    }

    if (!el) {
      for (let i = 1; i < parts.length; i++) {
        lines.push({ text: parts[i], spans: null, spansByteLen: 0, newlineNode: null, nodes: [], dirty: true });
      }
      return;
    }

    const doc = el.ownerDocument;
    const addedCount = parts.length - 1;
    const target: Node = addedCount > 2 ? doc.createDocumentFragment() : el;
    for (let i = 1; i < parts.length; i++) {
      const nl = doc.createTextNode('\n');
      target.appendChild(nl);
      const nodes = makePlainLineNodes(doc, parts[i]);
      for (let j = 0; j < nodes.length; j++) target.appendChild(nodes[j]);
      lines.push({
        text: parts[i],
        spans: null,
        spansByteLen: 0,
        newlineNode: nl,
        nodes,
        dirty: false,
      });
    }
    if (target !== el) el.appendChild(target);
  }

  /** Replaces the source, updating only lines whose text changed. */
  replace(text: string): void {
    const parts = text.split('\n');
    const lines = this.#lines;
    const el = this.#el;
    const kept = Math.min(parts.length, lines.length);

    for (let i = 0; i < kept; i++) {
      if (lines[i].text === parts[i]) continue;
      lines[i].text = parts[i];
      if (el) {
        this.#patchLine(i);
      } else {
        lines[i].dirty = true;
      }
    }

    if (parts.length < lines.length) {
      if (el) {
        for (let i = parts.length; i < lines.length; i++) {
          const line = lines[i];
          line.newlineNode?.remove();
          for (let j = 0; j < line.nodes.length; j++) line.nodes[j].remove();
        }
      }
      lines.length = parts.length;
    } else if (parts.length > lines.length) {
      if (!el) {
        for (let i = kept; i < parts.length; i++) {
          lines.push({ text: parts[i], spans: null, spansByteLen: 0, newlineNode: null, nodes: [], dirty: true });
        }
      } else {
        const doc = el.ownerDocument;
        const added = parts.length - kept;
        const target: Node = added > 2 ? doc.createDocumentFragment() : el;
        for (let i = kept; i < parts.length; i++) {
          const nl = i > 0 ? doc.createTextNode('\n') : null;
          if (nl) target.appendChild(nl);
          const nodes = makePlainLineNodes(doc, parts[i]);
          for (let j = 0; j < nodes.length; j++) target.appendChild(nodes[j]);
          lines.push({
            text: parts[i],
            spans: null,
            spansByteLen: 0,
            newlineNode: nl,
            nodes,
            dirty: false,
          });
        }
        if (target !== el) el.appendChild(target);
      }
    }
  }

  /** Applies spans from `spansAt`, patching DOM only for lines whose runs changed. */
  paint(spansAt: (index: number) => EncodedLine | null): void {
    const lines = this.#lines;
    const el = this.#el;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const next = spansAt(i);
      if (line.spans === next) continue;
      const changed = !sameSpans(line.spans, next);
      // Equal runs keep the DOM intact but take the new line object so an
      // older response is never retained.
      line.spans = next;
      if (!changed) continue;
      line.spansByteLen = spanCoveredBytes(next);
      if (el) {
        this.#patchLine(i);
      } else {
        line.dirty = true;
      }
    }
  }

  texts(): string[] {
    const lines = this.#lines;
    const out: string[] = [];
    for (let i = 0; i < lines.length; i++) out.push(lines[i].text);
    return out;
  }

  /** Patches one line's child nodes in place between its newline and the next line's newline. */
  #patchLine(index: number): void {
    const el = this.#el;
    if (!el) return;
    const lines = this.#lines;
    const line = lines[index];
    const anchor: Node | null = index + 1 < lines.length ? lines[index + 1].newlineNode : null;
    const doc = el.ownerDocument;
    const oldNodes = line.nodes;
    if (line.text.length > 0 && !colorRuns(line.spans) && oldNodes.length === 1 && oldNodes[0].nodeType === 3) {
      const textNode = oldNodes[0] as Text;
      if (textNode.data !== line.text) textNode.data = line.text;
      line.dirty = false;
      return;
    }
    const segs = line.text.length === 0 ? [] : spanSegments(line.text, line.spans);

    // Fast path: reuse existing nodes in place when segment count and kinds align.
    const nextNodes: (Text | HTMLSpanElement)[] = [];
    let i = 0;
    const common = Math.min(oldNodes.length, segs.length);
    for (; i < common; i++) {
      const seg = segs[i];
      const existing = oldNodes[i];
      if (seg.className) {
        if (existing.nodeType === 1) {
          const span = existing as HTMLSpanElement;
          if (span.className !== seg.className) span.className = seg.className;
          const first = span.firstChild;
          if (first && first.nodeType === 3 && first === span.lastChild) {
            if ((first as Text).data !== seg.text) (first as Text).data = seg.text;
          } else {
            span.textContent = seg.text;
          }
          nextNodes.push(span);
        } else {
          const span = doc.createElement('span');
          span.className = seg.className;
          span.textContent = seg.text;
          el.replaceChild(span, existing);
          nextNodes.push(span);
        }
      } else if (existing.nodeType === 3) {
        const textNode = existing as Text;
        if (textNode.data !== seg.text) textNode.data = seg.text;
        nextNodes.push(textNode);
      } else {
        const textNode = doc.createTextNode(seg.text);
        el.replaceChild(textNode, existing);
        nextNodes.push(textNode);
      }
    }

    if (segs.length < oldNodes.length) {
      for (let j = segs.length; j < oldNodes.length; j++) {
        oldNodes[j].remove();
      }
    } else if (segs.length > oldNodes.length) {
      const insertCount = segs.length - oldNodes.length;
      const frag = insertCount > 2 ? doc.createDocumentFragment() : null;
      for (let j = oldNodes.length; j < segs.length; j++) {
        const seg = segs[j];
        let node: Text | HTMLSpanElement;
        if (seg.className) {
          const span = doc.createElement('span');
          span.className = seg.className;
          span.textContent = seg.text;
          node = span;
        } else {
          node = doc.createTextNode(seg.text);
        }
        nextNodes.push(node);
        if (frag) frag.appendChild(node);
        else el.insertBefore(node, anchor);
      }
      if (frag) el.insertBefore(frag, anchor);
    }

    line.nodes = nextNodes;
    line.dirty = false;
  }
}
