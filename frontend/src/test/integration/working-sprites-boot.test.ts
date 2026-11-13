// The app root readies the working-indicator sprites before any turn
// starts: the custom list loads with the setting (a list landing mid-turn
// would grow the pool and reroll that turn's sprite) and every showable
// bundled strip is retained (lib/spinners/retain.ts). Turning animations
// off releases them.
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import App from '../../App.svelte';
import { flush, installAnimateShim, installAppDefaults, resetAppState } from './_helpers';
import { setBindingMock } from '../mocks/bindings-app';
import { updateFrontendPreferences } from '../../lib/stores/frontendPreferences.svelte';
import { __resetCustomSpinnersForTest } from '../../lib/stores/spinners.svelte';
import { __retainedSpriteSrcsForTest, retainSpriteImages } from '../../lib/spinners/retain';
import { BUILTIN_SPRITES } from '../../lib/spinners/catalog';

beforeAll(installAnimateShim);

describe('App integration: working sprites', () => {
  const listSpinners = vi.fn(async () => ({ dir: '/cfg', sprites: [], warnings: [] }));

  beforeEach(() => {
    resetAppState();
    installAppDefaults();
    listSpinners.mockClear();
    setBindingMock('GetSpinnerFiles', listSpinners);
  });

  afterEach(() => {
    updateFrontendPreferences({ spinnerAnimationsEnabled: false, spinnerDisabledAnimations: [] });
    retainSpriteImages([]);
    __resetCustomSpinnersForTest();
  });

  it('loads nothing while animations are off', async () => {
    updateFrontendPreferences({ spinnerAnimationsEnabled: false });
    render(App);
    await flush(10);
    expect(listSpinners).not.toHaveBeenCalled();
    expect(__retainedSpriteSrcsForTest()).toEqual([]);
  });

  it('loads the custom list and retains the showable strips at boot, then releases them when turned off', async () => {
    updateFrontendPreferences({ spinnerAnimationsEnabled: true, spinnerDisabledAnimations: ['nyan-cat'] });
    render(App);
    await flush(10);
    expect(listSpinners).toHaveBeenCalledTimes(1);
    const expected = BUILTIN_SPRITES.filter((sprite) => sprite.id !== 'nyan-cat').map((sprite) => sprite.src);
    expect(__retainedSpriteSrcsForTest().sort()).toEqual(expected.sort());

    updateFrontendPreferences({ spinnerAnimationsEnabled: false });
    await flush(4);
    expect(__retainedSpriteSrcsForTest()).toEqual([]);

    updateFrontendPreferences({ spinnerAnimationsEnabled: true });
    await flush(4);
    expect(__retainedSpriteSrcsForTest().length).toBe(expected.length);
    expect(listSpinners).toHaveBeenCalledTimes(1);
  });
});
