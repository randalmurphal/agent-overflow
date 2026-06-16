import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

// The store reaches the backend through these three RPC wrappers; replace the
// module so each test controls their resolution. The store imports nothing else
// from ./bindings, so a minimal factory is sufficient.
vi.mock('./bindings', () => ({
  CheckForUpdate: vi.fn(),
  DownloadUpdate: vi.fn(),
  RestartToUpdate: vi.fn(),
}));

import { CheckForUpdate, DownloadUpdate, RestartToUpdate } from './bindings';
import {
  getUpdateState,
  hasPendingUpdate,
  isDownloadInFlight,
  runUpdateCheck,
  startUpdateDownload,
  restartForUpdate,
  initUpdates,
  resetForTest,
} from './updates.svelte';
import {
  emitWailsEvent,
  resetWailsMocks,
  wailsListenerCount,
} from '../../test/mocks/wailsio-runtime';

const mockCheck = vi.mocked(CheckForUpdate);
const mockDownload = vi.mocked(DownloadUpdate);
const mockRestart = vi.mocked(RestartToUpdate);

// One macrotask flush so a fire-and-forget runUpdateCheck() (the launch check
// inside initUpdates) settles before assertions.
const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

interface Availability {
  supported: boolean;
  available: boolean;
  currentVersion: string;
  latestVersion?: string;
  releaseName?: string;
  releaseNotes?: string;
}

function availability(overrides: Partial<Availability> = {}): Availability {
  return { supported: true, available: false, currentVersion: '1.0.0', ...overrides };
}

describe('updates store', () => {
  beforeEach(() => {
    resetWailsMocks();
    resetForTest();
    mockCheck.mockReset();
    mockDownload.mockReset().mockResolvedValue(undefined);
    mockRestart.mockReset().mockResolvedValue(undefined);
  });

  describe('isDownloadInFlight', () => {
    it('is true only for the active download/verify/install phases', () => {
      expect(isDownloadInFlight('downloading')).toBe(true);
      expect(isDownloadInFlight('verifying')).toBe(true);
      expect(isDownloadInFlight('installing')).toBe(true);
      expect(isDownloadInFlight('available')).toBe(false);
      expect(isDownloadInFlight('ready')).toBe(false);
      expect(isDownloadInFlight('idle')).toBe(false);
    });
  });

  describe('runUpdateCheck', () => {
    it('flips to "available" and records release metadata when an update exists', async () => {
      mockCheck.mockResolvedValue(
        availability({
          available: true,
          latestVersion: '2.0.0',
          releaseName: 'Big Release',
          releaseNotes: 'notes here',
        }),
      );
      await runUpdateCheck();
      const s = getUpdateState();
      expect(s.phase).toBe('available');
      expect(s.latestVersion).toBe('2.0.0');
      expect(s.releaseName).toBe('Big Release');
      expect(s.releaseNotes).toBe('notes here');
      expect(s.currentVersion).toBe('1.0.0');
    });

    it('flips to "up-to-date" and clears a stale latestVersion when none exists', async () => {
      getUpdateState().latestVersion = 'stale';
      mockCheck.mockResolvedValue(availability({ available: false }));
      await runUpdateCheck();
      const s = getUpdateState();
      expect(s.phase).toBe('up-to-date');
      expect(s.latestVersion).toBe('');
    });

    it('marks unsupported builds and hides the section', async () => {
      mockCheck.mockResolvedValue(availability({ supported: false, currentVersion: 'dev' }));
      await runUpdateCheck();
      const s = getUpdateState();
      expect(s.supported).toBe(false);
      expect(s.phase).toBe('idle');
      expect(hasPendingUpdate()).toBe(false);
    });

    it('surfaces a check failure as the error phase', async () => {
      mockCheck.mockRejectedValue(new Error('network boom'));
      await runUpdateCheck();
      const s = getUpdateState();
      expect(s.phase).toBe('error');
      expect(s.error).not.toBe('');
    });

    it('does NOT re-check (and clobber) when an update is already staged ready', async () => {
      // Regression: a re-check from "ready" would re-resolve the same release
      // and demote the user from "Restart to update" back to "Download" even
      // though the staged build is still valid.
      getUpdateState().phase = 'ready';
      getUpdateState().latestVersion = '2.0.0';
      await runUpdateCheck();
      expect(mockCheck).not.toHaveBeenCalled();
      expect(getUpdateState().phase).toBe('ready');
    });

    it('does NOT re-check while a download is in flight', async () => {
      getUpdateState().phase = 'downloading';
      await runUpdateCheck();
      expect(mockCheck).not.toHaveBeenCalled();
      expect(getUpdateState().phase).toBe('downloading');
    });
  });

  describe('hasPendingUpdate', () => {
    it('tracks the latest check: false → true → false', async () => {
      expect(hasPendingUpdate()).toBe(false);
      mockCheck.mockResolvedValue(availability({ available: true, latestVersion: '2.0.0' }));
      await runUpdateCheck();
      expect(hasPendingUpdate()).toBe(true);
      mockCheck.mockResolvedValue(availability({ available: false }));
      await runUpdateCheck();
      expect(hasPendingUpdate()).toBe(false);
    });
  });

  describe('startUpdateDownload', () => {
    it('starts the download and optimistically flips to "downloading" from "available"', async () => {
      getUpdateState().phase = 'available';
      await startUpdateDownload();
      expect(mockDownload).toHaveBeenCalledOnce();
      expect(getUpdateState().phase).toBe('downloading');
    });

    it('is a no-op from the error phase — the backend has reset to StateError, so retry is a re-check', async () => {
      // Regression: after a failed download the Download button must not be a
      // dead control. The store refuses to start unless phase is "available".
      getUpdateState().phase = 'error';
      getUpdateState().latestVersion = '2.0.0';
      await startUpdateDownload();
      expect(mockDownload).not.toHaveBeenCalled();
      expect(getUpdateState().phase).toBe('error');
    });

    it('flips to "error" when the download RPC rejects', async () => {
      getUpdateState().phase = 'available';
      mockDownload.mockRejectedValue(new Error('rpc fail'));
      await startUpdateDownload();
      expect(getUpdateState().phase).toBe('error');
      expect(getUpdateState().error).not.toBe('');
    });
  });

  describe('restartForUpdate', () => {
    it('restarts only when an update is staged ready', async () => {
      getUpdateState().phase = 'ready';
      await restartForUpdate();
      expect(mockRestart).toHaveBeenCalledOnce();
    });

    it('is a no-op from any non-ready phase', async () => {
      getUpdateState().phase = 'available';
      await restartForUpdate();
      expect(mockRestart).not.toHaveBeenCalled();
    });
  });

  describe('initUpdates event bridge', () => {
    let cleanup: () => void;

    beforeEach(async () => {
      // The launch check fires inside initUpdates; settle it to up-to-date so
      // each event assertion starts from a known phase.
      mockCheck.mockResolvedValue(availability({ available: false }));
      cleanup = initUpdates();
      await tick();
    });

    afterEach(() => {
      cleanup();
    });

    it('subscribes to all six updater channels', () => {
      for (const ch of [
        'updater:download-started',
        'updater:progress',
        'updater:verifying',
        'updater:installing',
        'updater:ready',
        'updater:error',
      ]) {
        expect(wailsListenerCount(ch)).toBe(1);
      }
    });

    it('maps progress events to the downloading phase with byte counts', () => {
      emitWailsEvent('updater:progress', { written: 512, total: 2048 });
      const s = getUpdateState();
      expect(s.phase).toBe('downloading');
      expect(s.written).toBe(512);
      expect(s.total).toBe(2048);
    });

    it('advances through verifying → installing → ready', () => {
      emitWailsEvent('updater:verifying', null);
      expect(getUpdateState().phase).toBe('verifying');
      emitWailsEvent('updater:installing', null);
      expect(getUpdateState().phase).toBe('installing');
      emitWailsEvent('updater:ready', null);
      expect(getUpdateState().phase).toBe('ready');
    });

    it('formats an error event with its stage and message', () => {
      emitWailsEvent('updater:error', { stage: 'verify', message: 'digest mismatch' });
      const s = getUpdateState();
      expect(s.phase).toBe('error');
      expect(s.error).toBe('Update verify failed: digest mismatch');
    });

    it('cleanup unsubscribes every channel', () => {
      cleanup();
      expect(wailsListenerCount('updater:progress')).toBe(0);
      expect(wailsListenerCount('updater:ready')).toBe(0);
      cleanup = initUpdates(); // re-arm so afterEach stays balanced
    });

    it('is idempotent — a second initUpdates does not double-subscribe', () => {
      const secondCleanup = initUpdates();
      expect(wailsListenerCount('updater:progress')).toBe(1);
      secondCleanup(); // no-op cleanup; the real subscription stays live
      expect(wailsListenerCount('updater:progress')).toBe(1);
    });
  });
});
