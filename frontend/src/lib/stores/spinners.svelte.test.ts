import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import {
  __resetCustomSpinnersForTest,
  ensureCustomSpinners,
  peekCustomSpinners,
} from './spinners.svelte';

// happy-dom never decodes image bytes, so the store's size probe is fed by
// this stub: any data URL "decodes" to a 288x72 strip. The pure geometry
// rules have their own tests (lib/spinners/customs.test.ts); here the
// subject is the glue — a bad file costs one sprite and lands in warnings
// while the good rest still loads.
class FakeImage {
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  naturalWidth = 288;
  naturalHeight = 72;
  set src(_value: string) {
    queueMicrotask(() => this.onload?.());
  }
}

beforeEach(() => {
  vi.stubGlobal('Image', FakeImage);
});

afterEach(() => {
  __resetCustomSpinnersForTest();
  resetBindingMocks();
  vi.unstubAllGlobals();
});

describe('custom spinners store', () => {
  it('loads valid sprites and reports broken ones as warnings', async () => {
    setBindingMock('GetSpinnerFiles', () => ({
      dir: '/cfg/spinners',
      sprites: [
        { id: 'good', manifest: '{"frames": 4, "frameMs": 100}', png: 'aGk=' },
        { id: 'broken', manifest: '{nope', png: 'aGk=' },
        { id: 'crooked', manifest: '{"frames": 7, "frameMs": 100}', png: 'aGk=' },
      ],
      warnings: ['lonely.png: skipped, no lonely.json beside it'],
    }));

    ensureCustomSpinners();
    await vi.waitFor(() => {
      expect(peekCustomSpinners().dir).toBe('/cfg/spinners');
    });

    const { sprites, warnings } = peekCustomSpinners();
    expect(sprites.map((sprite) => sprite.id)).toEqual(['good']);
    expect(sprites[0]).toMatchObject({
      frames: 4,
      frameMs: 100,
      frameWidth: 72,
      frameHeight: 72,
      custom: true,
    });
    expect(sprites[0]!.src.startsWith('data:image/png;base64,')).toBe(true);
    // Backend warning survives, and each broken file explains itself.
    expect(warnings).toContain('lonely.png: skipped, no lonely.json beside it');
    expect(warnings.some((warning) => warning.includes('broken.json'))).toBe(true);
    expect(warnings.some((warning) => warning.includes('crooked.png'))).toBe(true);
  });

  it('answers empty before the first load resolves', () => {
    expect(peekCustomSpinners()).toEqual({ dir: '', sprites: [], warnings: [] });
  });
});
