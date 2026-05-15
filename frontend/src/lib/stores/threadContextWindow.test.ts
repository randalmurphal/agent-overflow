import { describe, expect, it } from 'vitest';
import type { Thread } from '../types/models';
import {
  activeAutoCompactPercent,
  normalizeContextWindowForThread,
  seedContextWindow,
} from './threadContextWindow';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp/workspace',
    projectPath: '/tmp/workspace',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('threadContextWindow', () => {
  it('returns null when a thread has no usage and no declared context window', () => {
    expect(seedContextWindow(makeThread())).toBeNull();
    expect(seedContextWindow(null)).toBeNull();
  });

  it('seeds an empty context snapshot from the thread context window', () => {
    expect(seedContextWindow(makeThread({ contextWindow: 200000 }))).toEqual({
      usedTokens: 0,
      maxTokens: 200000,
      usedPercentage: 0,
      autoCompactPercent: 90,
      autoCompactTokenLimit: 180000,
    });
  });

  it('parses persisted token usage and trusts the wire percentage', () => {
    // Go side computes UsedPercentage with a provider-aware formula
    // (Codex subtracts a 12000-token baseline; Claude uses the plain
    // ratio). Frontend must trust the wire value rather than recomputing.
    const thread = makeThread({
      contextWindow: 272000,
      autoCompactStandardPercent: 80,
      autoCompactExtendedPercent: 88,
      lastTokenUsage: JSON.stringify({
        usedTokens: 136000,
        maxTokens: 1050000,
        contextPercent: 12.95,
      }),
    });

    expect(seedContextWindow(thread)).toEqual({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
  });

  it('falls back to plain ratio when wire percentage is absent', () => {
    // Legacy persisted blobs (or any future event missing the field) are
    // recomputed locally so the meter still renders something sensible.
    const thread = makeThread({
      contextWindow: 272000,
      lastTokenUsage: JSON.stringify({
        usedTokens: 136000,
        maxTokens: 1050000,
      }),
    });
    const seeded = seedContextWindow(thread);
    expect(seeded?.usedPercentage).toBeCloseTo(12.95238095238095, 9);
  });

  it('drops malformed or incomplete persisted token usage', () => {
    expect(seedContextWindow(makeThread({ lastTokenUsage: '{nope' }))).toBeNull();
    expect(seedContextWindow(makeThread({
      lastTokenUsage: JSON.stringify({ maxTokens: 200000 }),
    }))).toBeNull();
  });

  it('normalizes live snapshots and trusts the wire percentage', () => {
    const thread = makeThread({
      contextWindow: 272000,
      autoCompactStandardPercent: 80,
      autoCompactExtendedPercent: 88,
    });

    expect(normalizeContextWindowForThread({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    }, thread)).toEqual({
      usedTokens: 136000,
      maxTokens: 1050000,
      usedPercentage: 12.95,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
  });

  it('propagates the exceeded sentinel through normalize and seed', () => {
    // Codex `total.totalTokens === modelContextWindow` is the
    // ContextWindowExceeded sentinel (set by `fill_to_context_window`);
    // the meter renders it distinctly. The persisted JSON arrives with
    // `exceeded: true` after Go-side classification.
    const thread = makeThread({
      provider: 'codex',
      contextWindow: 200000,
      lastTokenUsage: JSON.stringify({
        usedTokens: 200000,
        maxTokens: 200000,
        contextPercent: 100,
        exceeded: true,
      }),
    });
    const seeded = seedContextWindow(thread);
    expect(seeded?.exceeded).toBe(true);

    const normalized = normalizeContextWindowForThread({
      usedTokens: 200000,
      maxTokens: 200000,
      usedPercentage: 100,
      exceeded: true,
    }, thread);
    expect(normalized.exceeded).toBe(true);
  });

  it('prefers thread-specific compact overrides by context tier', () => {
    const thread = makeThread({
      autoCompactStandardPercent: 75,
      autoCompactExtendedPercent: 85,
    });

    expect(activeAutoCompactPercent(thread, 272000)).toBe(75);
    expect(activeAutoCompactPercent(thread, 1050000)).toBe(85);
  });
});
