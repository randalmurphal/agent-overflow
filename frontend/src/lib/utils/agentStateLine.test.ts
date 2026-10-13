import { describe, expect, it } from 'vitest';
import type { ThreadWakeRow } from '../stores/bindings';
import { agentStateLine } from './agentStateLine';

const NOW = 1_000_000_000;

function row(overrides: Partial<ThreadWakeRow>): ThreadWakeRow {
  return { id: overrides.label ?? 'id', kind: 'loop', label: 'board', state: 'armed', ...overrides } as ThreadWakeRow;
}

describe('agentStateLine', () => {
  it('is empty with no live loop or watch; reminders are not state', () => {
    expect(agentStateLine([], NOW)).toBeNull();
    expect(agentStateLine([row({ kind: 'reminder', label: 'later', nextAt: NOW + 60_000 })], NOW)).toBeNull();
  });

  it('names the row, its next fire and its missed count', () => {
    const line = agentStateLine([row({ label: 'board check', nextAt: NOW + 12 * 60_000, missed: 1 })], NOW);
    expect(line?.text).toBe('loop board check · next 12m · 1 missed');
    expect(line?.title).toBe('loop board check · next 12m · 1 missed');
  });

  it('leads with a running row, then the soonest fire, and counts the rest', () => {
    const line = agentStateLine([
      row({ kind: 'watch', label: 'MRs', nextAt: NOW + 60 * 60_000 }),
      row({ kind: 'loop', label: 'board', nextAt: NOW + 5 * 60_000 }),
      row({ kind: 'watch', label: 'feed', state: 'running', longRunning: true }),
    ], NOW);
    expect(line?.text).toBe('watch feed · running · +2 more');
    expect(line?.title.split('\n')).toEqual([
      'watch feed · running',
      'loop board · next 5m',
      'watch MRs · next 1h',
    ]);
  });

  it('says a long-running watch whose process is not alive is stopped', () => {
    expect(agentStateLine([row({ kind: 'watch', label: 'tail', longRunning: true })], NOW)?.text).toBe('watch tail · stopped');
  });

  it('does not mutate the rows it is given', () => {
    const rows = [row({ label: 'b', nextAt: NOW + 2 }), row({ label: 'a', nextAt: NOW + 1 })];
    agentStateLine(rows, NOW);
    expect(rows.map((r) => r.label)).toEqual(['b', 'a']);
  });
});
