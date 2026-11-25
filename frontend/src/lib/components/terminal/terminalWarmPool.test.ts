import { afterEach, describe, expect, it, vi } from 'vitest';
import type { BuiltTerminal } from './terminalXterm';
import {
  clearWarmTerminals,
  evictWarmTerminalsForBackend,
  parkWarmTerminal,
  takeWarmTerminal,
} from './terminalWarmPool';

function fakeBuilt(initialWebgl = true): {
  built: BuiltTerminal;
  setHasWebgl: (value: boolean) => void;
  dispose: ReturnType<typeof vi.fn>;
} {
  let hasWebgl = initialWebgl;
  const dispose = vi.fn();
  const built = {
    get hasWebgl() {
      return hasWebgl;
    },
    dispose,
  } as unknown as BuiltTerminal;
  return {
    built,
    setHasWebgl: (value: boolean) => {
      hasWebgl = value;
    },
    dispose,
  };
}

afterEach(() => {
  clearWarmTerminals();
});

describe('terminalWarmPool', () => {
  it('discards and disposes a parked terminal whose WebGL2 context was lost while parked', () => {
    const handle = {};
    const entry = fakeBuilt(true);
    parkWarmTerminal({
      terminalID: 'term-1',
      handle,
      paneId: 'main',
      backend: 'home',
      built: entry.built,
    });

    entry.setHasWebgl(false);

    expect(takeWarmTerminal(handle, 'term-1')).toBeNull();
    expect(entry.dispose).toHaveBeenCalledOnce();
  });

  it('evicts only warm terminals belonging to the specified backend', () => {
    const handleHome = {};
    const handleRemote = {};
    const homeEntry = fakeBuilt(true);
    const remoteEntry = fakeBuilt(true);

    parkWarmTerminal({
      terminalID: 'term-home',
      handle: handleHome,
      paneId: 'main',
      backend: 'home',
      built: homeEntry.built,
    });
    parkWarmTerminal({
      terminalID: 'term-remote',
      handle: handleRemote,
      paneId: 'pane-2',
      backend: 'peer-1',
      built: remoteEntry.built,
    });

    evictWarmTerminalsForBackend('peer-1');

    expect(remoteEntry.dispose).toHaveBeenCalledOnce();
    expect(homeEntry.dispose).not.toHaveBeenCalled();
    expect(takeWarmTerminal(handleRemote, 'term-remote')).toBeNull();
    expect(takeWarmTerminal(handleHome, 'term-home')).toBe(homeEntry.built);
  });
});
