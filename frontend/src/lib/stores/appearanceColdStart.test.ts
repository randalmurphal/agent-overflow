import { afterEach, expect, it, vi } from 'vitest';

afterEach(() => { localStorage.clear(); vi.resetModules(); });

it('restores every saved selection field during actual module initialization', async () => {
  vi.resetModules();
  const saved = { mode: 'dark', uiTheme: 'travel', codeTheme: 'nord', windowBackground: '#101017' };
  localStorage.setItem('agent-overflow:appearance', JSON.stringify(saved));
  const appearance = await import('./appearance.svelte');
  expect(appearance.getAppearance()).toEqual(saved);
  expect(appearance.getAppearanceMode()).toBe('dark');
});

it('rejects invalid stored ids without discarding valid mode and background', async () => {
  vi.resetModules();
  localStorage.setItem('agent-overflow:appearance', JSON.stringify({ mode: 'light', uiTheme: '../secret', codeTheme: 'nord', windowBackground: '#abcdef' }));
  const appearance = await import('./appearance.svelte');
  expect(appearance.getAppearance()).toMatchObject({ mode: 'light', uiTheme: appearance.DEFAULT_UI_THEME, codeTheme: 'nord', windowBackground: '#abcdef' });
});
