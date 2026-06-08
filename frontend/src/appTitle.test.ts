import { describe, expect, it } from 'vitest';
import { appTitle, appTitleForEnv } from './appTitle';

describe('appTitle', () => {
  it('marks dev builds in the document title', () => {
    expect(appTitle(true)).toBe('Agent Overflow (dev)');
  });

  it('keeps production builds unmarked', () => {
    expect(appTitle(false)).toBe('Agent Overflow');
  });

  it('marks Vite dev-server runs', () => {
    expect(appTitleForEnv({ DEV: true, MODE: 'development' })).toBe('Agent Overflow (dev)');
  });

  it('marks Vite development-mode builds used by dev-wsl', () => {
    expect(appTitleForEnv({ DEV: false, MODE: 'development' })).toBe('Agent Overflow (dev)');
  });

  it('keeps Vite production builds unmarked', () => {
    expect(appTitleForEnv({ DEV: false, MODE: 'production' })).toBe('Agent Overflow');
  });
});
