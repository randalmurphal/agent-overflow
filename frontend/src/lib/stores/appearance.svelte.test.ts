import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  getAppearance,
  getAppearanceFileWarnings,
  getAppearanceLoadError,
  getAppearanceRevision,
  getAppearanceThemes,
  getThemeDirectory,
  getThemeParseWarnings,
  installAppearanceEvents,
  isAppearanceLoaded,
  isAppearanceWritable,
  isThemeDirectoryAvailable,
  loadAppearance,
  resetAppearanceForTest,
  setAppearance,
  syncWindowBackground,
} from './appearance.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';

// Spelled literally: the store no longer exports the key, because nothing
// outside it reads the selection cache — the boot script reads the APPLIED
// stamp under a different key entirely.
const KEY = 'agent-overflow:appearance';

/** The shape `GetThemeFiles` answers with. */
function files(overrides: Record<string, unknown> = {}) {
  return {
    dir: '/home/u/.config/agent-overflow/themes',
    themes: [
      { id: 'nord', raw: JSON.stringify({ name: 'Nord', dark: { colors: { accent: '#88c0d0' } } }) },
    ],
    appearance: { mode: 'dark', uiTheme: 'nord', codeTheme: 'github' },
    warnings: [],
    ...overrides,
  };
}

function methodNotFound(): Error & { code: string } {
  return Object.assign(new Error('method not found'), { code: 'method_not_found' });
}

/** A promise plus the handle that settles it, for interleaving tests. */
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** Let a chain of already-resolved microtasks drain. */
async function settle(): Promise<void> {
  for (let i = 0; i < 4; i += 1) await Promise.resolve();
}

beforeEach(() => {
  localStorage.clear();
  resetAppearanceForTest();
  setBindingMock('GetThemeFiles', async () => files());
  setBindingMock('SetAppearance', async () => undefined);
  setBindingMock('SetWindowBackgroundColor', async () => undefined);
});

afterEach(() => {
  localStorage.clear();
  resetAppearanceForTest();
  vi.restoreAllMocks();
});

describe('loadAppearance', () => {
  it('parses the files, adopts the selection, and bumps the revision', async () => {
    const before = getAppearanceRevision();
    await loadAppearance();

    expect(isAppearanceLoaded()).toBe(true);
    expect(isThemeDirectoryAvailable()).toBe(true);
    expect(isAppearanceWritable()).toBe(true);
    expect(getThemeDirectory()).toBe('/home/u/.config/agent-overflow/themes');
    expect(getAppearance()).toEqual({
      mode: 'dark',
      uiTheme: 'nord',
      codeTheme: 'github',
      windowBackground: '',
    });
    expect(getAppearanceThemes().map((theme) => theme.name)).toEqual(['Nord']);
    expect(getAppearanceRevision()).toBeGreaterThan(before);
  });

  it('mirrors the selection into localStorage as this client’s durable copy', async () => {
    await loadAppearance();
    expect(JSON.parse(localStorage.getItem(KEY)!)).toMatchObject({
      mode: 'dark',
      uiTheme: 'nord',
    });
  });

  it('surfaces backend file warnings as data', async () => {
    setBindingMock('GetThemeFiles', async () =>
      files({ warnings: ['broken.json is larger than 64 KB and was skipped.'] }),
    );
    await loadAppearance();
    expect(getAppearanceFileWarnings()).toEqual([
      'broken.json is larger than 64 KB and was skipped.',
    ]);
  });

  it('collects per-file parse warnings from every loaded theme', async () => {
    setBindingMock('GetThemeFiles', async () =>
      files({
        themes: [{ id: 'typo', raw: JSON.stringify({ dark: { colors: { nonsuch: '#fff' } } }) }],
      }),
    );
    await loadAppearance();
    const warnings = getThemeParseWarnings();
    expect(warnings.some((warning) => warning.code === 'unknown-key')).toBe(true);
  });

  it('normalizes a selection the wire should not have been able to send', async () => {
    // The store is the last line: a hand-edited appearance.json reaches here
    // as whatever it says, and an unvalidated id is looked up and interpolated.
    setBindingMock('GetThemeFiles', async () =>
      files({ appearance: { mode: 'neon', uiTheme: 'Bad Id!', codeTheme: 5, windowBackground: 'red' } }),
    );
    await loadAppearance();
    expect(getAppearance()).toEqual({
      mode: 'system',
      uiTheme: 'default',
      codeTheme: 'github',
      windowBackground: '',
    });
  });

  it('skips a wire theme whose id is not a usable theme id, and says so', async () => {
    setBindingMock('GetThemeFiles', async () =>
      files({
        themes: [
          { id: '../../etc/passwd', raw: '{}' },
          { id: 'nord', raw: JSON.stringify({ dark: { colors: { accent: '#88c0d0' } } }) },
        ],
      }),
    );
    await loadAppearance();
    expect(getAppearanceThemes().map((theme) => theme.id)).toEqual(['nord']);
    expect(getAppearanceFileWarnings().some((text) => text.includes('not a usable theme id'))).toBe(
      true,
    );
  });

  it('degrades to built-ins when the RPC is refused, silently', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('GetThemeFiles', async () => {
      throw methodNotFound();
    });
    await expect(loadAppearance()).resolves.toBeUndefined();

    expect(isThemeDirectoryAvailable()).toBe(false);
    // The read is the LAN-allowed half, so a refused read means refused writes.
    expect(isAppearanceWritable()).toBe(false);
    expect(isAppearanceLoaded()).toBe(true);
    expect(getAppearanceThemes()).toEqual([]);
    expect(getThemeDirectory()).toBe('');
    expect(getAppearanceLoadError()).toBeNull();
    // A remote browser session is not a malfunction; it has no themes dir.
    expect(warn).not.toHaveBeenCalled();
  });

  it('leaves the selection alone while degrading, and keeps it local', async () => {
    setBindingMock('GetThemeFiles', async () => {
      throw methodNotFound();
    });
    await loadAppearance();
    const kept = getAppearance();

    await setAppearance({ mode: 'light' });
    expect(getAppearance().mode).toBe('light');
    expect(getAppearance().uiTheme).toBe(kept.uiTheme);
    // localStorage is the only durable copy such a session can have…
    expect(JSON.parse(localStorage.getItem(KEY)!).mode).toBe('light');
    // …and a refused session must not keep calling a method that is not there.
    expect(getBindingMock('SetAppearance')?.mock.calls.length ?? 0).toBe(0);
  });
});

describe('loadAppearance — transient failure', () => {
  it('keeps writes enabled and reports the failure as state', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    await loadAppearance();
    setBindingMock('GetThemeFiles', async () => {
      throw new Error('socket closed');
    });
    await loadAppearance();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(getAppearanceLoadError()).toContain('socket closed');
    // A boot-time WS blip must not silently stop persistence for the session.
    expect(isAppearanceWritable()).toBe(true);
    await setAppearance({ mode: 'light' });
    expect(getBindingMock('SetAppearance')!.mock.calls).toHaveLength(1);
  });

  it('keeps the themes it already loaded rather than emptying the picker', async () => {
    await loadAppearance();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('GetThemeFiles', async () => {
      throw new Error('socket closed');
    });
    await loadAppearance();
    expect(getAppearanceThemes().map((theme) => theme.id)).toEqual(['nord']);
    expect(isThemeDirectoryAvailable()).toBe(true);
  });

  it('clears the failure once a load succeeds again', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('GetThemeFiles', async () => {
      throw new Error('socket closed');
    });
    await loadAppearance();
    expect(getAppearanceLoadError()).not.toBeNull();

    setBindingMock('GetThemeFiles', async () => files());
    await loadAppearance();
    expect(getAppearanceLoadError()).toBeNull();
  });
});

describe('loadAppearance — read allowed, writes refused', () => {
  // The LAN posture: `GetThemeFiles` answers, both writes are local-only and
  // refused. Every `theme:changed` used to adopt the desktop's selection and
  // clobber this client's own, which inverts per-client residency.
  async function refuseWrites(): Promise<void> {
    await loadAppearance();
    setBindingMock('SetAppearance', async () => {
      throw methodNotFound();
    });
    await setAppearance({ uiTheme: 'default' });
    expect(isAppearanceWritable()).toBe(false);
  }

  it('takes the files off the wire but keeps the local selection', async () => {
    await refuseWrites();
    setBindingMock('GetThemeFiles', async () =>
      files({
        themes: [
          { id: 'nord', raw: JSON.stringify({ dark: { colors: { accent: '#5e81ac' } } }) },
          { id: 'mono', raw: JSON.stringify({ dark: { ansi: { 'ansi-fg-31': '#f00' } } }) },
        ],
        appearance: { mode: 'dark', uiTheme: 'nord', codeTheme: 'github' },
      }),
    );
    await loadAppearance();

    // Files: adopted. Selection: not.
    expect(getAppearanceThemes().map((theme) => theme.id)).toEqual(['nord', 'mono']);
    expect(getThemeDirectory()).toBe('/home/u/.config/agent-overflow/themes');
    expect(getAppearance().uiTheme).toBe('default');
    // And the refusal does not un-latch, so nothing re-arms the doomed write.
    expect(isAppearanceWritable()).toBe(false);
  });

  it('survives a repeated theme:changed without ever adopting a selection', async () => {
    await refuseWrites();
    await setAppearance({ mode: 'light' });
    for (let i = 0; i < 3; i += 1) await loadAppearance();
    expect(getAppearance().mode).toBe('light');
    expect(getAppearance().uiTheme).toBe('default');
    expect(JSON.parse(localStorage.getItem(KEY)!).mode).toBe('light');
  });
});

describe('loadAppearance — concurrency', () => {
  it('drops a superseded answer so two loads cannot land out of order', async () => {
    const slow = deferred<ReturnType<typeof files>>();
    const fast = deferred<ReturnType<typeof files>>();
    const queued = [slow.promise, fast.promise];
    setBindingMock('GetThemeFiles', () => queued.shift()!);

    const first = loadAppearance();
    const second = loadAppearance();
    // The SECOND request answers first with the newer truth…
    fast.resolve(files({ appearance: { mode: 'light', uiTheme: 'nord', codeTheme: 'github' } }));
    await second;
    // …and the first, stale answer must not overwrite it when it finally lands.
    slow.resolve(files({ appearance: { mode: 'dark', uiTheme: 'default', codeTheme: 'github' } }));
    await first;

    expect(getAppearance().mode).toBe('light');
    expect(getAppearance().uiTheme).toBe('nord');
  });

  it('does not let a refetch in flight revert a pick made after it was issued', async () => {
    await loadAppearance();
    const pending = deferred<ReturnType<typeof files>>();
    setBindingMock('GetThemeFiles', () => pending.promise);

    // A theme:changed refetch goes out, reading the file as it is NOW…
    const refetch = loadAppearance();
    // …the user picks something else, which persists…
    await setAppearance({ uiTheme: 'default' });
    // …and only then does the refetch land, carrying the pre-write file.
    pending.resolve(files());
    await refetch;

    expect(getAppearance().uiTheme).toBe('default');
    expect(JSON.parse(localStorage.getItem(KEY)!).uiTheme).toBe('default');
    // The FILES the stale answer carried still land — it is only the selection
    // that predates the user's pick.
    expect(getAppearanceThemes().map((theme) => theme.id)).toEqual(['nord']);
  });
});

describe('revision', () => {
  it('does not bump on a boot with no user themes', async () => {
    setBindingMock('GetThemeFiles', async () =>
      files({ themes: [], appearance: { mode: 'system', uiTheme: 'default', codeTheme: 'github' } }),
    );
    await loadAppearance();
    // Every bump remounts every mermaid diagram and re-atlases every terminal.
    expect(getAppearanceRevision()).toBe(0);
  });

  it('does not bump for an answer that changed no file', async () => {
    await loadAppearance();
    const before = getAppearanceRevision();
    await loadAppearance();
    await loadAppearance();
    expect(getAppearanceRevision()).toBe(before);
  });

  it('bumps when a file’s content actually moves', async () => {
    await loadAppearance();
    const before = getAppearanceRevision();
    setBindingMock('GetThemeFiles', async () =>
      files({
        themes: [
          { id: 'nord', raw: JSON.stringify({ name: 'Nord', dark: { colors: { accent: '#5e81ac' } } }) },
        ],
      }),
    );
    await loadAppearance();
    expect(getAppearanceRevision()).toBe(before + 1);
  });

  it('does not bump for a selection change, which moves the identity itself', async () => {
    await loadAppearance();
    const before = getAppearanceRevision();
    await setAppearance({ uiTheme: 'default' });
    // The palette identity already moves through its uiTheme component; a
    // second bump would charge one pick two remounts.
    expect(getAppearanceRevision()).toBe(before);
  });
});

describe('installAppearanceEvents', () => {
  it('refetches on every theme:changed', async () => {
    await loadAppearance();
    const stop = installAppearanceEvents();
    try {
      const calls = getBindingMock('GetThemeFiles')!.mock.calls.length;
      emitWailsEvent('theme:changed', undefined);
      await settle();
      expect(getBindingMock('GetThemeFiles')!.mock.calls.length).toBe(calls + 1);
    } finally {
      stop();
    }
  });

  it('stops listening when released', async () => {
    await loadAppearance();
    const stop = installAppearanceEvents();
    stop();
    const calls = getBindingMock('GetThemeFiles')!.mock.calls.length;
    emitWailsEvent('theme:changed', undefined);
    await settle();
    expect(getBindingMock('GetThemeFiles')!.mock.calls.length).toBe(calls);
  });

  it('reloads once on a transport reconnect, so a dropped theme:changed heals', async () => {
    await loadAppearance();
    const stop = installAppearanceEvents();
    try {
      const calls = getBindingMock('GetThemeFiles')!.mock.calls.length;
      // A watcher fire emitted while the socket was down is simply gone.
      __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
      __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
      await settle();
      expect(getBindingMock('GetThemeFiles')!.mock.calls.length).toBe(calls + 1);

      // Still connected: no second reload for a snapshot that did not dip.
      __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
      await settle();
      expect(getBindingMock('GetThemeFiles')!.mock.calls.length).toBe(calls + 1);
    } finally {
      stop();
      __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    }
  });
});

describe('setAppearance', () => {
  it('applies optimistically and sends the whole selection', async () => {
    await loadAppearance();
    const promise = setAppearance({ uiTheme: 'solarized' });
    // Visible before the RPC settles — the applier re-resolves from state.
    expect(getAppearance().uiTheme).toBe('solarized');
    await promise;

    const call = getBindingMock('SetAppearance')!.mock.calls[0][0];
    expect(call).toMatchObject({ mode: 'dark', uiTheme: 'solarized', codeTheme: 'github' });
  });

  it('omits an empty window background rather than sending a non-hex value', async () => {
    await loadAppearance();
    await setAppearance({ mode: 'light' });
    // Asserted on the WIRE shape: absent means "no cached value yet" on the Go
    // side, and an empty string would fail its hex check.
    const wire = JSON.parse(JSON.stringify(getBindingMock('SetAppearance')!.mock.calls[0][0]));
    expect(wire).not.toHaveProperty('windowBackground');
  });

  it('writes nothing when the patch changes nothing', async () => {
    await loadAppearance();
    await setAppearance({ uiTheme: 'nord', mode: 'dark' });
    expect(getBindingMock('SetAppearance')!.mock.calls).toHaveLength(0);
  });

  it('restores only the keys it replaced when the write fails', async () => {
    await loadAppearance();
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('SetAppearance', async () => {
      throw new Error('disk full');
    });

    await setAppearance({ codeTheme: 'monokai' });

    expect(getAppearance()).toEqual({
      mode: 'dark',
      uiTheme: 'nord',
      codeTheme: 'github',
      windowBackground: '',
    });
    expect(consoleErr).toHaveBeenCalled();
    consoleErr.mockRestore();
  });

  it('does not revert a concurrent change to a key it never wrote', async () => {
    await loadAppearance();
    vi.spyOn(console, 'error').mockImplementation(() => {});
    let failFirst!: (err: unknown) => void;
    setBindingMock(
      'SetAppearance',
      () =>
        new Promise((_resolve, reject) => {
          failFirst = reject;
        }),
    );

    // A code-theme write goes out and will fail…
    const first = setAppearance({ codeTheme: 'monokai' });
    // …and a mode change lands (and persists) while it is still in flight.
    setBindingMock('SetAppearance', async () => undefined);
    await setAppearance({ mode: 'light' });
    failFirst(new Error('disk full'));
    await first;

    expect(getAppearance().codeTheme).toBe('github');
    // Restoring the whole pre-call snapshot would have taken this with it.
    expect(getAppearance().mode).toBe('light');
  });

  it('keeps the local choice and stops persisting when the write is refused', async () => {
    await loadAppearance();
    setBindingMock('SetAppearance', async () => {
      throw methodNotFound();
    });

    await setAppearance({ mode: 'light' });
    // The only copy that can exist in a view-only session is this one.
    expect(getAppearance().mode).toBe('light');
    expect(isAppearanceWritable()).toBe(false);
    // The READ is a different capability and stays available.
    expect(isThemeDirectoryAvailable()).toBe(true);
    expect(JSON.parse(localStorage.getItem(KEY)!).mode).toBe('light');

    await setAppearance({ mode: 'dark' });
    expect(getBindingMock('SetAppearance')!.mock.calls).toHaveLength(1);
  });

  it('refuses a selection value that is not a theme id', async () => {
    await loadAppearance();
    await setAppearance({ uiTheme: 'not a; valid { id' });
    expect(getAppearance().uiTheme).toBe('default');
  });
});

describe('syncWindowBackground', () => {
  it('paints the native window and caches the value for the next launch', async () => {
    await loadAppearance();
    await syncWindowBackground('#101017');

    expect(getBindingMock('SetWindowBackgroundColor')!.mock.calls[0][0]).toBe('#101017');
    expect(getAppearance().windowBackground).toBe('#101017');
    expect(getBindingMock('SetAppearance')!.mock.calls[0][0]).toMatchObject({
      windowBackground: '#101017',
    });
  });

  it('is a no-op when the ground did not move', async () => {
    await loadAppearance();
    await syncWindowBackground('#101017');
    const paints = getBindingMock('SetWindowBackgroundColor')!.mock.calls.length;
    await syncWindowBackground('#101017');
    // Without this guard every re-resolve is an RPC plus a file write.
    expect(getBindingMock('SetWindowBackgroundColor')!.mock.calls).toHaveLength(paints);
  });

  it('ignores anything that is not #rrggbb', async () => {
    await loadAppearance();
    await syncWindowBackground('oklch(0.18 0.01 285)');
    expect(getBindingMock('SetWindowBackgroundColor')!.mock.calls).toHaveLength(0);
  });

  it('still caches the value when the paint itself is refused', async () => {
    await loadAppearance();
    setBindingMock('SetWindowBackgroundColor', async () => {
      throw methodNotFound();
    });
    await syncWindowBackground('#202027');
    // A refused paint does not make the cache useless next launch.
    expect(getAppearance().windowBackground).toBe('#202027');
  });

  it('does not attempt the paint at all once writes are known refused', async () => {
    await loadAppearance();
    setBindingMock('SetAppearance', async () => {
      throw methodNotFound();
    });
    await setAppearance({ mode: 'light' });
    await syncWindowBackground('#303037');
    expect(getBindingMock('SetWindowBackgroundColor')!.mock.calls).toHaveLength(0);
  });
});
