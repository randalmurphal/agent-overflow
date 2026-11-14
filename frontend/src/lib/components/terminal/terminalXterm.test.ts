import { beforeEach, describe, expect, it, vi } from 'vitest';
import { buildTerminal } from './terminalXterm';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetRunMode, setRunMode } from '../../../test/runMode';

const mocks = vi.hoisted(() => {
  const linkHandlers: Array<((event: MouseEvent, uri: string) => void) | undefined> = [];
  return {
    linkHandlers,
    FakeTerminal: class {
      unicode = { activeVersion: '6' };
      // The buffer events a test fires, and what clear() and reset() did.
      writeParsed: Array<() => void> = [];
      resized: Array<() => void> = [];
      calls: string[] = [];
      loadAddon(): void {}
      open(): void {}
      attachCustomKeyEventHandler(): void {}
      onWriteParsed(listener: () => void): void {
        this.writeParsed.push(listener);
      }
      onResize(listener: () => void): void {
        this.resized.push(listener);
      }
      clear(): void {
        this.calls.push('clear');
      }
      reset(): void {
        this.calls.push('reset');
      }
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
vi.mock('@xterm/addon-unicode11', () => ({ Unicode11Addon: class {} }));
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
vi.mock('../../stores/toast.svelte', () => ({ addToast: vi.fn(), addErrorToast: vi.fn() }));

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

// One notification of every buffer change, whoever makes it. Writes carry a
// full reset, the alternate screen and scrollback trimming; the browser test
// (terminalFind.browser.test.ts) runs each against a real xterm.
describe('buildTerminal onBufferChange', () => {
  type Fake = InstanceType<typeof mocks.FakeTerminal>;

  it('fires after parsed writes, resizes, clear() and reset(), until disposed', () => {
    const built = buildTerminal(document.createElement('div'), { onInput: () => {} });
    const fake = built.term as unknown as Fake;
    const seen: string[] = [];
    let cause = '';
    const subscription = built.onBufferChange(() => seen.push(cause));

    cause = 'write';
    for (const listener of fake.writeParsed) listener();
    cause = 'resize';
    for (const listener of fake.resized) listener();
    cause = 'clear';
    built.term.clear();
    cause = 'reset';
    built.term.reset();
    expect(seen).toEqual(['write', 'resize', 'clear', 'reset']);
    // The wrapped calls still do their own work.
    expect(fake.calls).toEqual(['clear', 'reset']);

    subscription.dispose();
    built.term.clear();
    expect(seen).toHaveLength(4);
  });

});
