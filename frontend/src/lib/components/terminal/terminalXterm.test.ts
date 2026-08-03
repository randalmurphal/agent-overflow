import { beforeEach, describe, expect, it, vi } from 'vitest';
import { buildTerminal } from './terminalXterm';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetRunMode, setRunMode } from '../../../test/runMode';

const mocks = vi.hoisted(() => {
  const linkHandlers: Array<((event: MouseEvent, uri: string) => void) | undefined> = [];
  return {
    linkHandlers,
    FakeTerminal: class {
      loadAddon(): void {}
      open(): void {}
      attachCustomKeyEventHandler(): void {}
      dispose(): void {}
    },
    FakeWebLinksAddon: class {
      constructor(handler?: (event: MouseEvent, uri: string) => void) {
        linkHandlers.push(handler);
      }
    },
  };
});

vi.mock('@xterm/xterm', () => ({ Terminal: mocks.FakeTerminal }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit(): void {} } }));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: mocks.FakeWebLinksAddon }));
vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: class {
    onContextLoss(): void {}
    dispose(): void {}
  },
}));
vi.mock('../../stores/keybindings.svelte', () => ({
  eventEscapesTerminalToCommand: vi.fn(() => false),
}));
vi.mock('../../utils/clipboard', () => ({ copyToClipboard: vi.fn(async () => true) }));
vi.mock('../../stores/toast.svelte', () => ({ addToast: vi.fn() }));

function buildAndTakeLinkHandler(): (event: MouseEvent, uri: string) => void {
  mocks.linkHandlers.length = 0;
  buildTerminal(document.createElement('div'), { onInput: () => {} });
  const handler = mocks.linkHandlers.at(-1);
  expect(handler).toBeTypeOf('function');
  return handler!;
}

// A dev server started in the terminal prints its URL into the buffer, so
// xterm's link provider IS this surface's open-in-browser affordance. It
// must not fall back to WebLinksAddon's default window.open handler: that
// bypasses internal/externalurl and loses the WSL → Windows bridge.
describe('buildTerminal web links', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
  });

  it('routes a clicked link through the OpenExternalURL binding', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

    buildAndTakeLinkHandler()(new MouseEvent('click'), 'http://localhost:5173/');

    await vi.waitFor(() => expect(open).toHaveBeenCalledWith('http://localhost:5173/'));
  });

  it('routes non-loopback links through the same wrapper', async () => {
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));

    buildAndTakeLinkHandler()(new MouseEvent('click'), 'https://svelte.dev/docs');

    await vi.waitFor(() => expect(open).toHaveBeenCalledWith('https://svelte.dev/docs'));
  });

  it('uses window.open instead of the binding in a remote client session', async () => {
    setRunMode('client');
    const open = setBindingMock('OpenExternalURL', vi.fn(async () => undefined));
    const windowOpen = vi.fn();
    const originalOpen = window.open;
    window.open = windowOpen as typeof window.open;

    try {
      buildAndTakeLinkHandler()(new MouseEvent('click'), 'http://localhost:5173/');

      await vi.waitFor(() =>
        expect(windowOpen).toHaveBeenCalledWith(
          'http://localhost:5173/',
          '_blank',
          'noopener,noreferrer',
        ),
      );
      expect(open).not.toHaveBeenCalled();
    } finally {
      window.open = originalOpen;
      resetRunMode();
    }
  });
});
