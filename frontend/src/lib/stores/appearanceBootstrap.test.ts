import { afterEach, beforeEach, expect, it } from 'vitest';
import { __resetScopesForTest, setPageGrantsFromBootstrap } from '../transport/scopes';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { getAppearance, isAppearanceLoaded, loadAppearance, resetAppearanceForTest, setAppearance } from './appearance.svelte';
import { readSpinnerFiles } from './appearanceFiles';

beforeEach(() => {
  resetAppearanceForTest();
  setBindingMock('SetAppearance', () => undefined);
  setBindingMock('GetSpinnerFiles', () => ({ dir: '/local/spinners', sprites: [], warnings: [] }));
  setBindingMock('GetThemeFiles', () => ({
    dir: '/local/themes', themes: [], warnings: [],
    appearance: { mode: 'dark', uiTheme: 'blacklight', codeTheme: 'github', windowBackground: '#000005' },
  }));
  __resetScopesForTest();
});
afterEach(() => { setPageGrantsFromBootstrap(false); resetAppearanceForTest(); });

it('waits for desktop bootstrap before loading appearance or choosing spinner storage', async () => {
  const loading = loadAppearance();
  const spinners = readSpinnerFiles();
  // Attach the rejection handler immediately: the old code rejects before
  // bootstrap because an imperative scope read cannot use its placeholder.
  const spinnerResult = spinners.then(value => ({ value }), error => ({ error }));
  await Promise.resolve();
  await Promise.resolve();
  expect(isAppearanceLoaded()).toBe(false);
  expect(getBindingMock('GetThemeFiles')).not.toHaveBeenCalled();
  setPageGrantsFromBootstrap(false);
  await loading;
  expect(await spinnerResult).not.toHaveProperty('error');
  expect(getBindingMock('GetSpinnerFiles')).toHaveBeenCalledOnce();
  expect(getAppearance()).toMatchObject({ uiTheme: 'blacklight', mode: 'dark' });
  expect(getBindingMock('SetAppearance')).not.toHaveBeenCalled();
});

it.each([false, true])('keeps an early choice and persists only on its own desktop (remote=%s)', async (remote) => {
  const saving = setAppearance({ uiTheme: 'blacklight' });
  const result = saving.then(() => null, error => error);
  expect(getAppearance().uiTheme).toBe('blacklight');
  expect(getBindingMock('SetAppearance')).not.toHaveBeenCalled();
  setPageGrantsFromBootstrap(remote);
  expect(await result).toBeNull();
  if (remote) expect(getBindingMock('SetAppearance')).not.toHaveBeenCalled();
  else expect(getBindingMock('SetAppearance')).toHaveBeenCalledWith(expect.objectContaining({ uiTheme: 'blacklight' }));
});
