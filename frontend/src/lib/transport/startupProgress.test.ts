import { describe, expect, it } from 'vitest';
import {
  displayVersion,
  parseStartupProgress,
  sameTransportStartup,
  startupMetaText,
  startupStatusText,
  transportStartup,
} from './startupProgress';

describe('startup report', () => {
  it('parses only a starting report and bounds what it keeps', () => {
    expect(parseStartupProgress(null)).toBeNull();
    expect(parseStartupProgress({ reason: 'ready' })).toBeNull();
    expect(parseStartupProgress({ reason: 'starting', phase: 7, detail: 'x'.repeat(500), step: -1, steps: 2.7, startedAt: 'soon' }))
      .toEqual({ phase: '', detail: 'x'.repeat(200), step: 0, steps: 2, startedAt: 0, updatedAt: 0, updatingTo: '' });
  });

  it('measures elapsed time on the backend clock', () => {
    const progress = { phase: 'p', detail: 'd', step: 1, steps: 2, startedAt: 1_000, updatedAt: 13_500, updatingTo: '' };
    expect(transportStartup(progress).elapsedMs).toBe(12_500);
    expect(transportStartup({ ...progress, startedAt: 0 }).elapsedMs).toBe(0);
    expect(transportStartup({ ...progress, updatedAt: 500 }).elapsedMs).toBe(0);
  });

  it('compares snapshots field by field', () => {
    const a = transportStartup({ phase: 'p', detail: 'd', step: 1, steps: 2, startedAt: 1, updatedAt: 2, updatingTo: '' });
    expect(sameTransportStartup(a, { ...a })).toBe(true);
    expect(sameTransportStartup(a, { ...a, elapsedMs: 2 })).toBe(false);
    expect(sameTransportStartup(a, undefined)).toBe(false);
    expect(sameTransportStartup(undefined, undefined)).toBe(true);
  });
});

// The same sentences internal/startupprogress writes for the Windows
// launcher's loading page (TestStatusNamesTheUpdateBeingFinished).
describe('startup status text', () => {
  it('reads the detail on a plain start', () => {
    expect(startupStatusText({ detail: 'Applying migration 3 of 7 add_index', updatingTo: '' }))
      .toBe('Applying migration 3 of 7 add_index');
    expect(startupStatusText({ detail: '', updatingTo: '' })).toBe('Starting');
  });

  it('names the update being finished', () => {
    expect(startupStatusText({ detail: 'Applying migration 3 of 7 add_index', updatingTo: '1.2.3' }))
      .toBe('Finishing update to v1.2.3: applying migration 3 of 7 add_index');
    expect(startupStatusText({ detail: 'WSL is starting', updatingTo: 'v2.0.0' }))
      .toBe('Finishing update to v2.0.0: WSL is starting');
    expect(startupStatusText({ detail: '', updatingTo: '2.0.0' })).toBe('Finishing update to v2.0.0: starting');
    expect(displayVersion('v1')).toBe('v1');
  });

  it('writes the launcher\'s step and elapsed line', () => {
    expect(startupMetaText({ step: 3, steps: 7, elapsedMs: 72_400 })).toBe('Step 3 of 7 · 1:12 elapsed');
    expect(startupMetaText({ step: 0, steps: 0, elapsedMs: 5_000 })).toBe('0:05 elapsed');
    expect(startupMetaText({ step: 0, steps: 0, elapsedMs: 999 })).toBe('');
  });
});
