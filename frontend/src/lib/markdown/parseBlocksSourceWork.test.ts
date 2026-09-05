import { expect, it, vi } from 'vitest';
import { StreamingBoundarySplitter } from './boundary';
import { createParseBlocksCache, createProvenAppend, parseBlocks } from './index';

it('streams committed blocks without inspecting or slicing the growing document', () => {
  const splitter = new StreamingBoundarySplitter();
  const cache = createParseBlocksCache();
  const slice = String.prototype.slice;
  const startsWith = String.prototype.startsWith;
  let inspectedLargeSource = 0;
  const sliceSpy = vi.spyOn(String.prototype, 'slice').mockImplementation(function (this: string, start, end) {
    if (this.length > 4096) inspectedLargeSource += this.length;
    return slice.call(this, start, end);
  });
  const prefixSpy = vi.spyOn(String.prototype, 'startsWith').mockImplementation(function (this: string, prefix, position) {
    if (this.length > 4096) inspectedLargeSource += this.length;
    return startsWith.call(this, prefix, position);
  });
  let source = '';
  let committed = '';
  try {
    for (let i = 0; i < 800; i++) {
      const append = createProvenAppend(source, `Paragraph ${i} with **bold** and enough text to allocate a backing string.\n\n`);
      source = append.next;
      const split = splitter.split(source, append);
      if (split.prefix !== committed) {
        parseBlocks(split.prefix, [], cache, splitter.prefixAppend);
        committed = split.prefix;
      }
    }
  } finally {
    sliceSpy.mockRestore();
    prefixSpy.mockRestore();
  }
  expect(source.length).toBeGreaterThan(50_000);
  expect(cache.blocks).toEqual(parseBlocks(committed));
  expect(inspectedLargeSource).toBe(0);
});

it('does not inspect a growing open fence while maintaining its source window', () => {
  const cache = createParseBlocksCache();
  let source = '```ts\n' + 'const value = 1;\n'.repeat(3000);
  parseBlocks(source, [], cache);
  const slice = String.prototype.slice;
  let slicedLargeSource = 0;
  const spy = vi.spyOn(String.prototype, 'slice').mockImplementation(function (this: string, start, end) {
    if (this.length > 4096) slicedLargeSource += this.length;
    return slice.call(this, start, end);
  });
  try {
    for (let i = 0; i < 100; i++) {
      const append = createProvenAppend(source, `const next${i} = ${i};\n`);
      source = append.next;
      parseBlocks(source, [], cache, append);
    }
  } finally {
    spy.mockRestore();
  }
  expect(cache.blocks).toEqual(parseBlocks(source));
  expect(slicedLargeSource).toBe(0);
});
