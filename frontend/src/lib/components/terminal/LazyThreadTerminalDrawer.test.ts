import { cleanup, render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

const terminalDrawerMock = vi.hoisted(() => ({
  shouldFailLoad: false,
}));

vi.mock('./ThreadTerminalDrawer.svelte', () => {
  if (terminalDrawerMock.shouldFailLoad) {
    throw new Error('terminal chunk unavailable');
  }
  return { default: () => ({}) };
});

import LazyThreadTerminalDrawer from './LazyThreadTerminalDrawer.svelte';

function makePane() {
  const thread = {
    id: 'thread-A',
    workspacePath: '/workspace',
    title: 't',
    provider: 'claude',
    projectPath: '/workspace',
    model: '',
    mode: 'chat',
    createdAt: 0,
    updatedAt: 0,
  };
  return {
    get thread() { return thread; },
    setShowTerminal: vi.fn(),
    toggleTerminal: vi.fn(),
  };
}

afterEach(() => {
  cleanup();
  terminalDrawerMock.shouldFailLoad = false;
});

describe('LazyThreadTerminalDrawer', () => {
  it('renders a load error when the terminal chunk fails to load', async () => {
    terminalDrawerMock.shouldFailLoad = true;

    const { getByTestId } = render(LazyThreadTerminalDrawer, {
      pane: makePane() as never,
      manual: true,
    });

    await waitFor(() => {
      expect(getByTestId('terminal-drawer-load-error')).toHaveTextContent('Failed to load terminal drawer');
    });
  });
});
