import { afterEach, describe, expect, it } from 'vitest';
import { resetCssColorProbe, rgbChannels, toConcreteColor } from './cssColorProbe';

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

describe('rgbChannels', () => {
  // BOTH of `toConcreteColor`'s documented output forms, in one place. Two
  // hand-rolled copies of the rgb() regex existed before this and one of them
  // silently dropped hex, so a hex-valued accent lost its terminal selection
  // tint — a bug that is only possible when the re-parse is duplicated.
  it('reads the rgb() / rgba() form', () => {
    expect(rgbChannels('rgb(16, 16, 23)')).toEqual({ r: 16, g: 16, b: 23, a: 1 });
    expect(rgbChannels('rgba(1, 2, 3, 0.5)')).toEqual({ r: 1, g: 2, b: 3, a: 0.5 });
    // Space-separated and slash-alpha syntax is the same colour.
    expect(rgbChannels('rgb(1 2 3)')).toEqual({ r: 1, g: 2, b: 3, a: 1 });
    expect(rgbChannels('rgb(1 2 3 / 50%)')).toEqual({ r: 1, g: 2, b: 3, a: 0.5 });
    expect(rgbChannels('  rgb(1, 2, 3)  ')).toEqual({ r: 1, g: 2, b: 3, a: 1 });
  });

  it('reads the hex form, in all four lengths', () => {
    expect(rgbChannels('#d97757')).toEqual({ r: 217, g: 119, b: 87, a: 1 });
    expect(rgbChannels('#abc')).toEqual({ r: 170, g: 187, b: 204, a: 1 });
    expect(rgbChannels('#abcf')).toEqual({ r: 170, g: 187, b: 204, a: 1 });
    expect(rgbChannels('#d97757ff')).toEqual({ r: 217, g: 119, b: 87, a: 1 });
    expect(rgbChannels('#d9775700')).toEqual({ r: 217, g: 119, b: 87, a: 0 });
  });

  it('answers undefined for anything it cannot read', () => {
    expect(rgbChannels(undefined)).toBeUndefined();
    expect(rgbChannels('')).toBeUndefined();
    expect(rgbChannels('   ')).toBeUndefined();
    expect(rgbChannels('#12345')).toBeUndefined();
    expect(rgbChannels('#gggggg')).toBeUndefined();
    expect(rgbChannels('rebeccapurple')).toBeUndefined();
    // Still in a wide-gamut space: never went through `toConcreteColor`.
    expect(rgbChannels('oklch(0.178 0.014 285.82)')).toBeUndefined();
    expect(rgbChannels('rgb(oops)')).toBeUndefined();
  });
});
