import { beforeEach, describe, expect, it } from 'vitest';
import { CodeDomRenderer } from './codeDomRenderer';
import { initSyntaxClassNames, type EncodedLine } from '../../../utils/syntaxSpans';

function spans(runs: number[]): EncodedLine {
  return { r: runs };
}

beforeEach(() => {
  initSyntaxClassNames(['none', 'keyword', 'string']);
});

describe('CodeDomRenderer', () => {
  it('mounts initial text with exact newline textContent contract', () => {
    const code = document.createElement('code');
    const renderer = new CodeDomRenderer('alpha\nbeta\ngamma');
    renderer.mount(code);

    expect(code.textContent).toBe('alpha\nbeta\ngamma');
    expect(renderer.texts()).toEqual(['alpha', 'beta', 'gamma']);
    expect(code.childNodes.length).toBe(5);
    expect(code.childNodes[1].textContent).toBe('\n');
    expect(code.childNodes[3].textContent).toBe('\n');
  });

  it('appends in-line plain text in place without replacing the text node', () => {
    const code = document.createElement('code');
    const renderer = new CodeDomRenderer('const x');
    renderer.mount(code);

    const firstNode = code.firstChild;
    renderer.append(' = 42');
    expect(code.firstChild).toBe(firstNode);
    expect(code.textContent).toBe('const x = 42');
  });

  it('appends multi-line deltas preserving existing line nodes and exact textContent', () => {
    const code = document.createElement('code');
    const renderer = new CodeDomRenderer('line1');
    renderer.mount(code);

    const line1Node = code.firstChild;
    renderer.append(' tail\nline2\n\nline4');

    expect(code.firstChild).toBe(line1Node);
    expect(code.textContent).toBe('line1 tail\nline2\n\nline4');
    expect(renderer.texts()).toEqual(['line1 tail', 'line2', '', 'line4']);
  });

  it('patches only lines whose spans change and preserves DOM nodes for unchanged runs', () => {
    const code = document.createElement('code');
    const renderer = new CodeDomRenderer('func main() {\n  return 1\n}');
    renderer.mount(code);

    // Line 0: "func" (4 chars) class 1, rest plain (class 0)
    const initialSpans: (EncodedLine | null)[] = [
      spans([4, 1, 9, 0]),
      spans([2, 0, 6, 1, 2, 0]),
      null,
    ];
    renderer.paint((i) => initialSpans[i] ?? null);

    expect(code.textContent).toBe('func main() {\n  return 1\n}');
    const line0Span = code.querySelector('span');
    expect(line0Span?.textContent).toBe('func');

    // Re-painting with equal-valued fresh EncodedLine keeps existing span node intact.
    const equalCopy: (EncodedLine | null)[] = [
      spans([4, 1, 9, 0]),
      spans([2, 0, 6, 1, 2, 0]),
      null,
    ];
    renderer.paint((i) => equalCopy[i] ?? null);
    expect(code.querySelector('span')).toBe(line0Span);

    // Clearing line 1 spans reverts line 1 to plain text while keeping line 0 span intact.
    renderer.paint((i) => (i === 0 ? equalCopy[0] : null));
    expect(code.querySelector('span')).toBe(line0Span);
    expect(code.textContent).toBe('func main() {\n  return 1\n}');
  });

  it('handles replace growth, shrink, and pre-mount paint/append', () => {
    const code = document.createElement('code');
    const renderer = new CodeDomRenderer('a');
    renderer.append('b\ncd');
    renderer.paint((i) => (i === 0 ? spans([2, 1]) : null));
    renderer.mount(code);

    expect(code.textContent).toBe('ab\ncd');
    expect(code.querySelectorAll('span')).toHaveLength(1);

    renderer.replace('ab\nxy\nzz');
    expect(code.textContent).toBe('ab\nxy\nzz');
    expect(code.querySelectorAll('span')).toHaveLength(1);

    renderer.replace('only');
    expect(code.textContent).toBe('only');
    expect(renderer.texts()).toEqual(['only']);
  });

  it('appends trailing plain text in O(1) after paint without rebuilding prior spans, while still repatching pre-pushed spans', () => {
    const code = document.createElement('code');
    const renderer = new CodeDomRenderer('func ');
    renderer.mount(code);

    // Highlight "func" (4 bytes, class 1) + " " (1 byte, plain).
    renderer.paint((i) => (i === 0 ? spans([4, 1, 1, 0]) : null));
    const kwSpan = code.querySelector('span');
    expect(kwSpan?.textContent).toBe('func');
    const tailText = code.lastChild;
    expect(tailText?.nodeType).toBe(Node.TEXT_NODE);

    // Streaming beyond spansByteLen (5) extends the trailing text node in place.
    renderer.append('main');
    renderer.append('() {}');
    expect(code.querySelector('span')).toBe(kwSpan);
    expect(code.lastChild).toBe(tailText);
    expect(code.textContent).toBe('func main() {}');

    // When spans ended on a <span> with no trailing text node, first append adds one and subsequent appends mutate it.
    const code2 = document.createElement('code');
    const r2 = new CodeDomRenderer('func');
    r2.mount(code2);
    r2.paint((i) => (i === 0 ? spans([4, 1]) : null));
    const kw2 = code2.firstChild;
    r2.append(' foo');
    const newTail = code2.lastChild;
    r2.append('()');
    expect(code2.firstChild).toBe(kw2);
    expect(code2.lastChild).toBe(newTail);
    expect(code2.textContent).toBe('func foo()');

    // First-push-before-text: spans already cover bytes beyond current text length, so append must repatch to color new chars.
    const code3 = document.createElement('code');
    const r3 = new CodeDomRenderer('fu');
    r3.mount(code3);
    r3.paint((i) => (i === 0 ? spans([4, 1, 4, 2]) : null));
    r3.append('nc"hi"');
    expect(code3.textContent).toBe('func"hi"');
    const spans3 = code3.querySelectorAll('span');
    expect(spans3).toHaveLength(2);
    expect(spans3[0].textContent).toBe('func');
    expect(spans3[1].textContent).toBe('"hi"');
  });

  it('preserves live DOM Range selections inside the trailing text node across streaming appends', () => {
    const code = document.createElement('code');
    document.body.appendChild(code);
    try {
      const r = new CodeDomRenderer('const hello');
      r.mount(code);
      const tail = code.firstChild;
      expect(tail?.nodeType).toBe(Node.TEXT_NODE);
      const range = document.createRange();
      range.setStart(tail!, 6);
      range.setEnd(tail!, 11);
      expect(range.toString()).toBe('hello');

      r.append(' = "world";');
      expect(range.startContainer).toBe(tail);
      expect(range.startOffset).toBe(6);
      expect(range.endOffset).toBe(11);
      expect(range.toString()).toBe('hello');
    } finally {
      code.remove();
    }
  });
});
