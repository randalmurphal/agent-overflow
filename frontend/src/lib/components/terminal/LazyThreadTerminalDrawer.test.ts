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

function makeSurface() {
  return {
    paneId: 'main',
    threadId: 'thread-A',
    workspacePath: '/workspace',
    setVisible: vi.fn(),
    acquireResizeLease: vi.fn(() => null),
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
      surface: makeSurface() as never,
      manual: true,
    });

    await waitFor(() => {
      expect(getByTestId('terminal-drawer-load-error')).toHaveTextContent('Failed to load terminal drawer');
    });
  });
});
