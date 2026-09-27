import { setFlagsFromString } from 'node:v8';
import { runInNewContext } from 'node:vm';
import { describe, expect, it } from 'vitest';
import { initSyntaxClassNames } from '../../../utils/syntaxSpans';
import { CodeLines } from './codeLines.svelte';

describe('CodeLines', () => {
  it('appends to the last line and adds only the lines the delta starts', () => {
    const code = new CodeLines('alpha\npartial');
    const [alpha, partial] = code.lines;
    code.append(' tail\nbeta\n');
    expect(code.texts()).toEqual(['alpha', 'partial tail', 'beta', '']);
    expect(code.lines[0]).toBe(alpha);
    expect(code.lines[1]).toBe(partial);

    const before = code.lines;
    code.append('x');
    expect(code.lines).toBe(before);
    expect(code.texts()).toEqual(['alpha', 'partial tail', 'beta', 'x']);
  });

  it('replaces the source keeping the rendered lines', () => {
    const code = new CodeLines('a\nb\nc');
    const [a, b] = code.lines;
    code.replace('a\nB');
    expect(code.texts()).toEqual(['a', 'B']);
    expect(code.lines[0]).toBe(a);
    expect(code.lines[1]).toBe(b);
    code.replace('a\nB\nc\nd');
    expect(code.texts()).toEqual(['a', 'B', 'c', 'd']);
    expect(code.lines[1]).toBe(b);
  });

  it('re-renders only the lines whose text or colors change', () => {
    initSyntaxClassNames(['none', 'keyword']);
    const code = new CodeLines('if x\nfoo');
    const keyword = { r: [2, 1] };
    code.paint((i) => (i === 0 ? keyword : null));
    const [first, second] = code.lines.map((line) => line.segments);
    expect(first).toEqual([
      { text: 'if', className: 'syntax-keyword' },
      { text: ' x', className: '' },
    ]);

    // A new object with the same runs, and every plain encoding
    // spanSegments treats alike, change nothing.
    for (const plain of [null, {}, { r: [] }, { r: [3] }]) {
      code.paint((i) => (i === 0 ? { r: [2, 1] } : plain));
      expect(code.lines[0].segments).toBe(first);
      expect(code.lines[1].segments).toBe(second);
    }

    // Setting a line to the text it holds changes nothing either.
    code.replace('if x\nfoo');
    expect(code.lines[0].segments).toBe(first);
    expect(code.lines[1].segments).toBe(second);

    code.append('d');
    expect(code.lines[0].segments).toBe(first);
    expect(code.lines[1].segments).toEqual([{ text: 'food', className: '' }]);
    const food = code.lines[1].segments;
    code.append('\nbar');
    expect(code.lines[1].segments).toBe(food);
    expect(code.lines[2].segments).toEqual([{ text: 'bar', className: '' }]);

    // A line keeps its colors when only its text changes.
    code.replace('if y\nfood\nbar');
    expect(code.lines[0].segments).toEqual([
      { text: 'if', className: 'syntax-keyword' },
      { text: ' y', className: '' },
    ]);

    code.paint((i) => (i === 0 ? { r: [1, 1] } : null));
    expect(code.lines[0].segments).toEqual([
      { text: 'i', className: 'syntax-keyword' },
      { text: 'f y', className: '' },
    ]);

    code.paint((i) => (i === 0 ? { r: [1, 1, 2, 1] } : null));
    expect(code.lines[0].segments).toEqual([
      { text: 'if ', className: 'syntax-keyword' },
      { text: 'y', className: '' },
    ]);

    code.paint(() => null);
    expect(code.lines[0].segments).toEqual([{ text: 'if y', className: '' }]);
  });

  it('holds only the newest response for a line whose colors do not change', async () => {
    setFlagsFromString('--expose-gc');
    const gc = runInNewContext('gc') as () => void;
    initSyntaxClassNames(['none', 'keyword']);
    const code = new CodeLines('if x');
    const older = ((): WeakRef<object> => {
      const line = { r: [2, 1] };
      code.paint(() => line);
      return new WeakRef(line);
    })();
    const segments = code.lines[0].segments;

    code.paint(() => ({ r: [2, 1] }));
    expect(code.lines[0].segments).toBe(segments);
    // A WeakRef keeps its target until the current job ends.
    await new Promise((resolve) => setTimeout(resolve, 0));
    gc();
    expect(older.deref()).toBeUndefined();
  });
});
