import { describe, expect, it } from 'vitest';
import { TransportError } from '../transport/wsClient';
import {
  errorHasBackendLog,
  errorHasDetails,
  errorLayers,
  errorReport,
  errorReportText,
} from './errorReport';

const CHAIN = [
  'interrupt-and-revert: claude rollback: uuid "8e78" is missing from session',
  'claude rollback: uuid "8e78" is missing from session',
  'uuid "8e78" is missing from session',
];

function backendError(chain: string[], backend = ''): TransportError {
  return new TransportError('method_error', chain[0] ?? 'failed', {
    detail: { ref: 'rKVz', method: 'InterruptAndRevertIfClean', at: Date.UTC(2026, 9, 8, 12), chain },
    backend,
  });
}

describe('errorReport', () => {
  it('lists the backend chain and reads the log only from the page\'s own backend', () => {
    const local = errorReport(backendError(CHAIN), { context: 'Could not undo the message' });
    expect(errorLayers(local)).toEqual(CHAIN);
    expect(errorHasBackendLog(local)).toBe(true);
    expect(errorHasDetails(local)).toBe(true);

    // Another attached computer: the log read would be answered by HOME,
    // which never logged this ref.
    expect(errorHasBackendLog(errorReport(backendError(CHAIN, 'b-office')))).toBe(false);
    // An off-host caller receives no chain, so it has no log to read.
    expect(errorHasBackendLog(errorReport(backendError([])))).toBe(false);
  });

  it('falls back to the cause chain of a client error', () => {
    const report = errorReport(new Error('could not save', { cause: new Error('disk full') }));
    expect(errorLayers(report)).toEqual(['could not save', 'disk full']);
    expect(report.detail).toBeUndefined();
  });

  it('has details only when they add to the headline', () => {
    expect(errorHasDetails(errorReport(new Error('disk full'), { context: 'Save failed: disk full' }))).toBe(false);
    expect(errorHasDetails(errorReport(new Error('disk full'), { context: 'Save failed' }))).toBe(true);
    expect(errorHasDetails(errorReport(backendError([])))).toBe(true);
  });

  it('writes a report an agent can act on', () => {
    const report = errorReport(backendError(CHAIN), { context: 'Could not undo the message', facts: [['thread', 't1']] });
    const text = errorReportText(report, {
      appVersion: '1.2.3',
      platform: 'Linux',
      backendLog: { lines: ['triage: dropping init event', 'transport: failed (id: rKVz): boom'], found: true },
    });
    expect(text).toContain('Agent Overflow error: Could not undo the message');
    expect(text).toContain('Reason: Uuid "8e78" is missing from session.');
    expect(text).toContain('- code: method_error');
    expect(text).toContain('- method: InterruptAndRevertIfClean');
    expect(text).toContain('- ref: rKVz');
    expect(text).toContain('- time: 2026-10-08T12:00:00.000Z');
    expect(text).toContain('- thread: t1');
    expect(text).toContain('- app: 1.2.3 (Linux)');
    expect(text).toContain(`1. ${CHAIN[0]}\n2. ${CHAIN[1]}\n3. ${CHAIN[2]}`);
    expect(text).toContain('```\ntriage: dropping init event\ntransport: failed (id: rKVz): boom\n```');
  });

  it('does not repeat a reason the headline already says', () => {
    const text = errorReportText(errorReport(new Error('disk full'), { context: 'Save failed: disk full' }));
    expect(text).not.toContain('Reason:');
  });

  it('says where the backend log is when it was not read', () => {
    const report = errorReport(backendError([]));
    expect(errorReportText(report)).toContain("Backend log: on the backend's computer, under ref rKVz.");
    expect(errorReportText(report, { backendLog: { lines: [], found: false } }))
      .toContain('no longer retained in memory');
    expect(errorReportText(report, { backendLog: { unavailable: 'could not be read (offline)' } }))
      .toContain('Backend log: could not be read (offline)');
  });
});
