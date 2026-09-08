import { IDBFactory } from 'fake-indexeddb';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { copyAppearanceFiles, readAppearanceFiles, readSpinnerFiles } from './appearanceFiles';
import { getAppearance, loadAppearance, resetAppearanceForTest, setAppearance } from './appearance.svelte';

const context = vi.hoisted(() => ({ local: false, home: true, targets: [] as string[], grants: Promise.resolve() }));
vi.mock('../transport/scopes', () => ({
  hasScope: (scope: string) => scope === 'host' ? context.local : true,
  pageGrantsResolved: () => context.grants,
}));
vi.mock('../transport/backends', async (original) => ({
  ...await original<typeof import('../transport/backends')>(),
  backendById: () => context.home ? {} : undefined,
  withBackendTarget: <T>(target: string, issue: () => T): T => { context.targets.push(target); return issue(); },
}));

beforeEach(() => {
  vi.stubGlobal('indexedDB', new IDBFactory());
  context.local = false;
  context.home = true;
  context.targets = [];
  context.grants = Promise.resolve();
  localStorage.clear();
  resetAppearanceForTest();
  setBindingMock('GetThemeFiles', () => ({ dir: '/mac/themes', themes: [{ id: 'nord', raw: '{}' }], warnings: [], appearance: { mode: 'dark', uiTheme: 'nord', codeTheme: 'nord' } }));
  setBindingMock('GetSpinnerFiles', () => ({ dir: '/mac/spinners', sprites: [], warnings: [] }));
});
afterEach(() => { vi.unstubAllGlobals(); resetAppearanceForTest(); });

describe('appearance file residency', () => {
  it('does not adopt a cached browser library as the desktop selection during bootstrap', async () => {
    // A prior unresolved boot may have populated this origin's browser
    // library. That library contains files only, never a desktop selection.
    await readAppearanceFiles();
    let resolve!: () => void;
    context.grants = new Promise<void>(done => { resolve = done; });
    setBindingMock('GetThemeFiles', () => ({
      dir: '/local/themes', themes: [], warnings: [],
      appearance: { mode: 'dark', uiTheme: 'blacklight', codeTheme: 'github' },
    }));
    const loading = loadAppearance();
    context.local = true;
    resolve();
    await loading;
    expect(getAppearance()).toMatchObject({ mode: 'dark', uiTheme: 'blacklight' });
    expect(getBindingMock('GetThemeFiles')).toHaveBeenCalledOnce();
  });

  it('migrates legacy files once and survives removal of the original computer', async () => {
    expect((await readAppearanceFiles()).themes.map((file) => file.id)).toEqual(['nord']);
    expect(context.targets).toEqual(['']);
    context.home = false;
    setBindingMock('GetThemeFiles', () => { throw new Error('computer removed'); });
    expect((await readAppearanceFiles()).themes.map((file) => file.id)).toEqual(['nord']);
    expect(getBindingMock('GetThemeFiles')).not.toHaveBeenCalled();
  });

  it('a fresh frontend with no first host loads built-ins without a remote call', async () => {
    context.home = false;
    expect((await readAppearanceFiles()).themes).toEqual([]);
    expect((await readSpinnerFiles()).sprites).toEqual([]);
    expect(context.targets).toEqual([]);
  });

  it('copies from the explicitly chosen computer and preserves this frontend’s selection', async () => {
    await setAppearance({ mode: 'light', uiTheme: 'default' });
    await copyAppearanceFiles('themes', 'gpu-computer');
    await loadAppearance();
    expect(context.targets).toEqual(['gpu-computer']);
    expect(getAppearance()).toMatchObject({ mode: 'light', uiTheme: 'default' });
    expect((await readAppearanceFiles()).dir).toBe('');
  });

  it('a remote file edit does not silently replace the device’s library', async () => {
    await readAppearanceFiles();
    setBindingMock('GetThemeFiles', () => ({ themes: [{ id: 'new', raw: '{}' }], warnings: [] }));
    expect((await readAppearanceFiles()).themes[0].id).toBe('nord');
    await copyAppearanceFiles('themes', 'other-computer');
    expect((await readAppearanceFiles()).themes[0].id).toBe('new');
  });

  it('the desktop continues reading its own editable directory', async () => {
    context.local = true;
    expect((await readAppearanceFiles()).dir).toBe('/mac/themes');
    expect((await readSpinnerFiles()).dir).toBe('/mac/spinners');
    expect(context.targets).toEqual([]);
    await expect(copyAppearanceFiles('themes', 'gpu-computer')).rejects.toThrow('own appearance directory');
  });
});
