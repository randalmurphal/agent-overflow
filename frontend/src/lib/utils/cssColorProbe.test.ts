import { afterEach, describe, expect, it } from 'vitest';
import { resetCssColorProbe, toConcreteColor } from './cssColorProbe';

// happy-dom has no canvas and no cascade for `oklch()`, so this file
// covers only what is environment-independent: the strict hex fast path
// and the OMIT-never-pass-through contract. The serialization facts that
// motivate the canvas hop (getComputedStyle hands back `oklch(…)`;
// `color-mix()` computes to `oklab(…)`) are pinned in real Chromium by
// `components/chat/markdown/mermaidTokens.browser.test.ts`.

afterEach(() => {
  resetCssColorProbe();
});

describe('toConcreteColor', () => {
  it('fast-paths strictly validated hex', () => {
    expect(toConcreteColor('#d97757')).toBe('#d97757');
    expect(toConcreteColor('  #d97757  ')).toBe('#d97757');
    expect(toConcreteColor('#abc')).toBe('#abc');
    expect(toConcreteColor('#abcd')).toBe('#abcd');
    expect(toConcreteColor('#d9775780')).toBe('#d9775780');
  });

  it('drops a fully transparent hex', () => {
    // Nothing useful downstream can be done with alpha 0, and it is the
    // same answer a rejected value gets — keep the two indistinguishable.
    expect(toConcreteColor('#abc0')).toBeUndefined();
    expect(toConcreteColor('#d9775700')).toBeUndefined();
  });

  it('returns undefined rather than an unparseable value', () => {
    // The contract is OMIT, never pass through: mermaid's `base` theme
    // has a defensible default for every variable it derives, and a
    // string khroma throws on has no default at all.
    expect(toConcreteColor(undefined)).toBeUndefined();
    expect(toConcreteColor('')).toBeUndefined();
    expect(toConcreteColor('   ')).toBeUndefined();
    expect(toConcreteColor('var(--surface-1)')).toBeUndefined();
    expect(toConcreteColor('oklch(0.178 0.014 285.82)')).toBeUndefined();
  });

  it('refuses hex-shaped strings that are not hex colors', () => {
    // The fast path is a WHITELIST, not a sniff: anything the regex does
    // not accept takes the canvas round trip (absent here), so a
    // malformed value can never reach khroma by looking color-shaped.
    expect(toConcreteColor('#12345')).toBeUndefined();
    expect(toConcreteColor('#gggggg')).toBeUndefined();
    expect(toConcreteColor('#abc; drop')).toBeUndefined();
  });

  it('sends rgb() through the canvas rather than passing it through', () => {
    // Deliberate: uniform transparent-drop and validation semantics for
    // everything that is not a validated hex literal. Without a canvas
    // (happy-dom) that means `undefined` — the omit answer — rather than
    // an unvalidated pass-through.
    expect(toConcreteColor('rgb(16, 16, 23)')).toBeUndefined();
    expect(toConcreteColor('rgba(0, 0, 0, 0)')).toBeUndefined();
  });
});
