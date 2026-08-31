import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { __resetRunModeForTest } from '../transport/runMode';
import { setPageGrantsFromBootstrap } from '../transport/scopes';
import { probeDevServerURL, resetDevServerProbeForTest } from './devServerProbe';

describe('probeDevServerURL', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetDevServerProbeForTest();
    __resetRunModeForTest();
  });

  afterEach(() => {
    __resetRunModeForTest();
    vi.restoreAllMocks();
  });

  it('returns the backend verdict', async () => {
    const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => true));

    await expect(probeDevServerURL('http://localhost:5173/')).resolves.toBe(true);
    expect(probe).toHaveBeenCalledWith('http://localhost:5173/');
  });

  it('keeps no verdict memo of its own — every settled call re-asks the backend', async () => {
    const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => true));

    await expect(probeDevServerURL('http://localhost:5173/')).resolves.toBe(true);
    await expect(probeDevServerURL('http://localhost:5173/')).resolves.toBe(true);

    // The backend cache (internal/devserverprobe) is the single TTL
    // authority; a frontend memo would let the two layers disagree.
    expect(probe).toHaveBeenCalledTimes(2);
  });

  it('dedupes concurrent probes of the same URL onto one RPC', async () => {
    let release!: (live: boolean) => void;
    const probe = setBindingMock(
      'ProbeDevServerURL',
      vi.fn(() => new Promise<boolean>((resolve) => (release = resolve))),
    );

    const first = probeDevServerURL('http://localhost:5173/');
    const second = probeDevServerURL('http://localhost:5173/');
    release(true);

    await expect(first).resolves.toBe(true);
    await expect(second).resolves.toBe(true);
    expect(probe).toHaveBeenCalledTimes(1);
  });

  it('short-circuits to not-live in a view-only session without touching the wire', async () => {
    const probe = setBindingMock('ProbeDevServerURL', vi.fn(async () => true));
    setPageGrantsFromBootstrap(true);

    await expect(probeDevServerURL('http://localhost:5173/')).resolves.toBe(false);
    expect(probe).not.toHaveBeenCalled();
  });

  it('logs and treats an RPC failure as not live', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock(
      'ProbeDevServerURL',
      vi.fn(async () => {
        throw new Error('wire broke');
      }),
    );

    await expect(probeDevServerURL('http://localhost:5173/')).resolves.toBe(false);
    expect(warn).toHaveBeenCalled();
  });
});
