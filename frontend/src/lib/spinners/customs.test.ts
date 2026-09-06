import { describe, expect, it } from 'vitest';
import { buildCustomSprite, parseCustomManifest, pngDimensions } from './customs';
import { pngHeader } from '../../test/pngHeader';

describe('pngDimensions', () => {
  it('reads a bounded strip without decoding image pixels', () => {
    expect(pngDimensions(pngHeader(288, 72))).toEqual({ width: 288, height: 72 });
  });
  it.each([[0, 10], [10, 0], [32_000, 32_000], [65_536, 1]])('refuses unsafe dimensions %s by %s', (width, height) => {
    expect(pngDimensions(pngHeader(width, height))).toBeNull();
  });
  it.each(['', 'bad base64 !', btoa('this is not a PNG')])('refuses invalid PNG headers', (raw) => {
    expect(pngDimensions(raw)).toBeNull();
  });
});

describe('parseCustomManifest', () => {
  it('parses a minimal manifest', () => {
    expect(parseCustomManifest('cat', '{"frames": 8, "frameMs": 100}')).toEqual({
      frames: 8,
      frameMs: 100,
    });
  });

  it('carries a trimmed label', () => {
    const parsed = parseCustomManifest('cat', '{"frames":2,"frameMs":50,"label":"  My Cat  "}');
    expect(parsed).toEqual({ frames: 2, frameMs: 50, label: 'My Cat' });
  });

  it('rejects broken JSON with the file named', () => {
    expect(parseCustomManifest('cat', '{nope')).toBe('cat.json: not valid JSON');
  });

  it('rejects non-object roots', () => {
    expect(parseCustomManifest('cat', '[1,2]')).toContain('expected an object');
  });

  it.each([
    ['{"frameMs": 100}', 'frames'],
    ['{"frames": 0, "frameMs": 100}', 'frames'],
    ['{"frames": 2.5, "frameMs": 100}', 'frames'],
    ['{"frames": 999, "frameMs": 100}', 'frames'],
    ['{"frames": 8}', 'frameMs'],
    ['{"frames": 8, "frameMs": 5}', 'frameMs'],
    ['{"frames": 8, "frameMs": 9999}', 'frameMs'],
  ])('rejects %s naming the bad field', (raw, field) => {
    const parsed = parseCustomManifest('cat', raw);
    expect(typeof parsed).toBe('string');
    expect(parsed).toContain(`"${field}"`);
  });
});

describe('buildCustomSprite', () => {
  const manifest = { frames: 4, frameMs: 100 };

  it('derives frame geometry from the decoded size', () => {
    const sprite = buildCustomSprite('cat', manifest, 'data:x', 288, 72);
    expect(sprite).toMatchObject({ frames: 4, frameWidth: 72, frameHeight: 72, custom: true });
  });

  it('labels fall back to the id', () => {
    const sprite = buildCustomSprite('cat', manifest, 'data:x', 288, 72);
    expect(typeof sprite === 'object' && sprite.label).toBe('cat');
  });

  it('rejects a width that does not divide into frames', () => {
    expect(buildCustomSprite('cat', manifest, 'data:x', 290, 72)).toContain(
      'does not divide into 4 equal frames',
    );
  });

  it('rejects a failed decode', () => {
    expect(buildCustomSprite('cat', manifest, 'data:x', 0, 0)).toContain('could not decode');
  });
});
