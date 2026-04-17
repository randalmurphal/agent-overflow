import { describe, expect, it } from 'vitest';
import { summarizeActiveTools } from './activeToolsSummary';
import type { WorkEntryData } from '../types/models';

function entry(overrides: Partial<WorkEntryData> = {}): WorkEntryData {
  return {
    id: overrides.id ?? 'tool',
    type: overrides.type ?? 'Bash',
    name: overrides.name,
    status: 'running',
    meta: null,
    ...overrides,
  };
}

describe('summarizeActiveTools()', () => {
  it('returns empty label and names for an empty set', () => {
    expect(summarizeActiveTools([])).toEqual({ count: 0, names: [], label: '' });
  });

  it('labels a single tool as "Running 1 tool — Bash"', () => {
    const result = summarizeActiveTools([entry({ id: '1', type: 'Bash' })]);
    expect(result.count).toBe(1);
    expect(result.names).toEqual(['Bash']);
    expect(result.label).toBe('Running 1 tool — Bash');
  });

  it('labels multiple tools with plural noun and joined names', () => {
    const result = summarizeActiveTools([
      entry({ id: '1', type: 'Read' }),
      entry({ id: '2', type: 'Grep' }),
      entry({ id: '3', type: 'Bash' }),
    ]);
    expect(result.count).toBe(3);
    expect(result.names).toEqual(['Read', 'Grep', 'Bash']);
    expect(result.label).toBe('Running 3 tools — Read, Grep, Bash');
  });

  it('deduplicates identical tool names while preserving appearance order', () => {
    const result = summarizeActiveTools([
      entry({ id: '1', type: 'Read' }),
      entry({ id: '2', type: 'Read' }),
      entry({ id: '3', type: 'Bash' }),
    ]);
    expect(result.count).toBe(3);
    expect(result.names).toEqual(['Read', 'Bash']);
    expect(result.label).toBe('Running 3 tools — Read, Bash');
  });

  it('caps the displayed names and adds an ellipsis when truncated', () => {
    const result = summarizeActiveTools(
      [
        entry({ id: '1', type: 'Read' }),
        entry({ id: '2', type: 'Grep' }),
        entry({ id: '3', type: 'Write' }),
        entry({ id: '4', type: 'Bash' }),
      ],
      3,
    );
    expect(result.names.length).toBe(4);
    expect(result.label).toBe('Running 4 tools — Read, Grep, Write, ...');
  });

  it('prefers the explicit entry.name over entry.type when present', () => {
    const result = summarizeActiveTools([
      entry({ id: '1', type: 'tool', name: 'Read' }),
      entry({ id: '2', type: 'tool', name: 'Grep' }),
    ]);
    expect(result.names).toEqual(['Read', 'Grep']);
    expect(result.label).toBe('Running 2 tools — Read, Grep');
  });

  it('ignores entries whose name and type are blank', () => {
    const result = summarizeActiveTools([
      entry({ id: '1', type: '   ' }),
      entry({ id: '2', type: 'Bash' }),
    ]);
    // The blank entry still counts in the total (the user still has 2 tools
    // running) but is not included in the names list.
    expect(result.count).toBe(2);
    expect(result.names).toEqual(['Bash']);
    expect(result.label).toBe('Running 2 tools — Bash');
  });
});
