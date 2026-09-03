// The composition half of the screen-presence frame: what "this screen is
// being looked at" resolves to on a desktop and on a phone. The wire
// behavior — dedup, bounds, restatement after hello — is
// lib/transport/wsClient.test.ts.
//
// Spies on the real transport singleton rather than mocking the module, the
// shape ./watchedThreads.test.ts uses and for the same reason: src/test/setup.ts
// already holds a live reference, so a module mock would leave this suite
// asserting against a different instance than the code under test calls.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { wsClient } from '../transport/wsClient';
import { installScreenPresence, refreshScreenPresence } from './screenPresence';

// The two stores this module reads. Mocked rather than driven, because the
// question here is the composition rule and building a real pane registry
// would answer a different one.
const layout = vi.hoisted(() => ({
  compact: false,
  screen: 'list' as 'list' | 'thread',
}));
const panes = vi.hoisted(() => ({
  open: [] as string[],
  focused: null as string | null,
}));

vi.mock('./layoutMode.svelte', () => ({
  isCompactLayout: () => layout.compact,
  getCompactScreen: () => layout.screen,
}));
vi.mock('./panes.svelte', () => ({
  openThreadIds: () => new Set(panes.open),
  getFocusedPaneOrNull: () => (panes.focused ? { threadId: panes.focused } : null),
}));

interface Stated {
  focused: boolean;
  threads: string[];
}

let stated: Stated[];

function last(): Stated | undefined {
  const entry = stated.at(-1);
  return entry ? { focused: entry.focused, threads: [...entry.threads].sort() } : undefined;
}

function setDocument(opts: { hidden?: boolean; focused?: boolean }): void {
  Object.defineProperty(document, 'hidden', {
    configurable: true,
    get: () => opts.hidden ?? false,
  });
  vi.spyOn(document, 'hasFocus').mockReturnValue(opts.focused ?? true);
}

describe('screen presence composition', () => {
  beforeEach(() => {
    stated = [];
    layout.compact = false;
    layout.screen = 'list';
    panes.open = [];
    panes.focused = null;
    vi.spyOn(wsClient, 'setPresence').mockImplementation((focused, threads) => {
      stated.push({ focused, threads: [...threads] });
    });
    setDocument({});
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('states every open pane on a desktop', () => {
    panes.open = ['t-a', 't-b'];
    refreshScreenPresence();
    expect(last()).toEqual({ focused: true, threads: ['t-a', 't-b'] });
  });

  it('states focus false while another app is in front, and keeps the panes', () => {
    // The second rule is exactly this case: not focused, still on screen.
    panes.open = ['t-a'];
    setDocument({ focused: false });
    refreshScreenPresence();
    expect(last()).toEqual({ focused: false, threads: ['t-a'] });
  });

  it('states nothing on screen while the document is hidden', () => {
    panes.open = ['t-a'];
    setDocument({ hidden: true });
    refreshScreenPresence();
    expect(last()).toEqual({ focused: false, threads: [] });
  });

  it('states only the revealed thread under the compact layout', () => {
    layout.compact = true;
    layout.screen = 'thread';
    panes.open = ['t-a', 't-b'];
    panes.focused = 't-b';
    refreshScreenPresence();
    // The strip is one pane wide there, so the focused pane is the answer —
    // never the whole registry, which is what the desktop reads.
    expect(last()).toEqual({ focused: true, threads: ['t-b'] });
  });

  it('states no thread while the compact layout is showing its list', () => {
    layout.compact = true;
    layout.screen = 'list';
    panes.open = ['t-a'];
    panes.focused = 't-a';
    refreshScreenPresence();
    expect(last()).toEqual({ focused: true, threads: [] });
  });

  it('recomputes on focus, blur and visibility, and stops on dispose', () => {
    panes.open = ['t-a'];
    const dispose = installScreenPresence();
    // Installing states it once, so a screen that never changes is not
    // treated as unattended for its whole life.
    expect(stated).toHaveLength(1);

    setDocument({ focused: false });
    window.dispatchEvent(new Event('blur'));
    expect(last()).toEqual({ focused: false, threads: ['t-a'] });

    setDocument({ focused: true });
    window.dispatchEvent(new Event('focus'));
    expect(last()).toEqual({ focused: true, threads: ['t-a'] });

    setDocument({ hidden: true });
    document.dispatchEvent(new Event('visibilitychange'));
    expect(last()).toEqual({ focused: false, threads: [] });

    dispose();
    const before = stated.length;
    window.dispatchEvent(new Event('focus'));
    document.dispatchEvent(new Event('visibilitychange'));
    expect(stated).toHaveLength(before);
  });

  it('hands an unchanged screen the same state, and leaves the dedup to the transport', () => {
    panes.open = ['t-a'];
    refreshScreenPresence();
    refreshScreenPresence();
    // Two calls, one meaning: nothing here compares, because the transport
    // is where an unchanged state stops costing bytes and it is the only
    // place that knows what each connection was last told.
    expect(stated).toEqual([
      { focused: true, threads: ['t-a'] },
      { focused: true, threads: ['t-a'] },
    ]);
  });
});
