import { describe, expect, it } from 'vitest';

import { clipboardFiles, hasFilePayload } from './filePayload';

describe('hasFilePayload', () => {
  it.each([
    { case: 'true for drag events carrying files', event: { dataTransfer: { types: ['Files'] } }, expected: true },
    { case: 'true for image mime types in the drag payload', event: { dataTransfer: { types: ['image/png'] } }, expected: true },
    { case: 'false for non-file drags (e.g. text)', event: { dataTransfer: { types: ['text/plain'] } }, expected: false },
    { case: 'false when dataTransfer is missing', event: {}, expected: false },
  ])('is $case', ({ event, expected }) => {
    expect(hasFilePayload(event as unknown as DragEvent)).toBe(expected);
  });
});

describe('clipboardFiles', () => {
  const image = new File(['x'], 'a.png', { type: 'image/png' });
  const pdf = new File(['y'], 'b.pdf', { type: 'application/pdf' });
  const event = {
    clipboardData: {
      items: [
        { kind: 'file', type: 'image/png', getAsFile: () => image },
        { kind: 'string', type: 'text/plain', getAsFile: () => null },
        { kind: 'file', type: 'application/pdf', getAsFile: () => pdf },
        { kind: 'file', type: 'application/zip', getAsFile: () => null },
      ],
    },
  } as unknown as ClipboardEvent;

  it('takes every file a paste carries, in order, and leaves its text', () => {
    expect(clipboardFiles(event)).toEqual([image, pdf]);
  });

  it('takes only images when asked, as the composer does', () => {
    expect(clipboardFiles(event, true)).toEqual([image]);
  });

  it('takes nothing from a paste with no clipboard data', () => {
    expect(clipboardFiles({} as unknown as ClipboardEvent)).toEqual([]);
  });
});
