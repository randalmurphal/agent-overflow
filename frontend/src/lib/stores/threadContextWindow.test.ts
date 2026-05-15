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

  it('parses persisted token usage and recomputes percentage from max tokens', () => {
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
      usedPercentage: 12.95238095238095,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
  });

  it('drops malformed or incomplete persisted token usage', () => {
    expect(seedContextWindow(makeThread({ lastTokenUsage: '{nope' }))).toBeNull();
    expect(seedContextWindow(makeThread({
      lastTokenUsage: JSON.stringify({ maxTokens: 200000 }),
    }))).toBeNull();
  });

  it('normalizes live snapshots against the active thread', () => {
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
      usedPercentage: 12.95238095238095,
      autoCompactPercent: 88,
      autoCompactTokenLimit: 924000,
    });
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
