import { describe, expect, it } from 'vitest';
import { expectedStreamingFenceTexts } from './streamingFenceOracle';

describe('expectedStreamingFenceTexts', () => {
  it.each([
    ['closed backticks', '```ts\nconst x = 1;\n```\n\nafter', ['const x = 1;'], false],
    ['closed tildes', '~~~\na\n\n~~~~   \nafter', ['a\n'], false],
    ['short closer in body', '````ts\na\n```\nb\n`````', ['a\n```\nb'], false],
    ['open body', '```ts\nstill open', ['still open'], true],
    ['opener at EOF', '```ts', [''], true],
    ['one-byte partial closer', '```ts\nbody\n`', ['body'], true],
    ['two-byte partial closer', '```ts\nbody\n``', ['body'], true],
    ['invalid inline opener', '`` ` not a fence\n```ts\na\n```', ['a'], false],
    ['four-space indentation', '    ```ts\nnot top level\n    ```', [], false],
  ] as const)('%s', (_name, source, texts, hasOpenFence) => {
    expect(expectedStreamingFenceTexts(source)).toEqual({ texts, hasOpenFence });
  });

  it('never treats marker-looking lines inside an open fence as another opener', () => {
    expect(expectedStreamingFenceTexts(
      '```ts\nbody\n~~~\nstill code',
    )).toEqual({
      texts: ['body\n~~~\nstill code'],
      hasOpenFence: true,
    });
  });
});
