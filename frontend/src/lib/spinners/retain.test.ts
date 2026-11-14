import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BUILTIN_SPRITES, type SpinnerSprite } from './catalog';

const reportFrontendDiagnostic = vi.fn();
vi.mock('../utils/frontendErrorCapture', () => ({ reportFrontendDiagnostic }));

const { retainSpriteImages, __retainedSpriteSrcsForTest } = await import('./retain');

class FakeImage {
  static created: FakeImage[] = [];
  src = '';
  onerror: (() => void) | null = null;
  constructor() {
    FakeImage.created.push(this);
  }
}

const [a, b] = BUILTIN_SPRITES;
const custom: SpinnerSprite = { ...a, id: 'mine', src: 'data:image/png;base64,AAAA', custom: true };

beforeEach(() => {
  FakeImage.created = [];
  reportFrontendDiagnostic.mockClear();
  vi.stubGlobal('Image', FakeImage);
});

afterEach(() => {
  retainSpriteImages([]);
  vi.unstubAllGlobals();
});

describe('retainSpriteImages', () => {
  it('loads each bundled strip once and keeps it across repeated calls', () => {
    retainSpriteImages([a, b]);
    retainSpriteImages([a, b]);
    expect(FakeImage.created.map((image) => image.src)).toEqual([a.src, b.src]);
    expect(__retainedSpriteSrcsForTest()).toEqual([a.src, b.src]);
  });

  it('releases strips that can no longer show and loads new ones', () => {
    retainSpriteImages([a]);
    retainSpriteImages([b]);
    expect(__retainedSpriteSrcsForTest()).toEqual([b.src]);
    expect(FakeImage.created.map((image) => image.src)).toEqual([a.src, b.src]);
  });

  it('releases everything when turned off and reloads when turned back on', () => {
    retainSpriteImages([a]);
    retainSpriteImages([]);
    expect(__retainedSpriteSrcsForTest()).toEqual([]);
    retainSpriteImages([a]);
    expect(__retainedSpriteSrcsForTest()).toEqual([a.src]);
    expect(FakeImage.created.length).toBe(2);
  });

  it('skips custom data-URL sprites', () => {
    retainSpriteImages([custom, a]);
    expect(__retainedSpriteSrcsForTest()).toEqual([a.src]);
  });

  it('reports a failed load and retries it on the next call', () => {
    retainSpriteImages([a]);
    FakeImage.created[0].onerror?.();
    expect(reportFrontendDiagnostic).toHaveBeenCalledWith('working sprite strip failed to load', a.src);
    expect(__retainedSpriteSrcsForTest()).toEqual([]);
    retainSpriteImages([a]);
    expect(FakeImage.created.length).toBe(2);
  });

  it('ignores a late error from a released strip', () => {
    retainSpriteImages([a]);
    const released = FakeImage.created[0];
    retainSpriteImages([]);
    expect(released.onerror).toBeNull();
  });
});
