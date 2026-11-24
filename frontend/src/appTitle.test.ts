import { describe, expect, it } from 'vitest';
import { appTitleForEnv } from './appTitle';

describe('appTitle', () => {
  it.each([
    ['marks Vite dev-server runs', { DEV: true, MODE: 'development' }, 'Agent Overflow (dev)'],
    ['marks Vite development-mode builds used by dev-wsl', { DEV: false, MODE: 'development' }, 'Agent Overflow (dev)'],
    ['keeps Vite production builds unmarked', { DEV: false, MODE: 'production' }, 'Agent Overflow'],
  ])('%s', (_case, env, title) => {
    expect(appTitleForEnv(env)).toBe(title);
  });
});
