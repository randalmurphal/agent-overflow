import { describe, it, expect, beforeEach, vi } from 'vitest';
import { createDiffHighlighterPool } from './diffHighlighterPool';

// MockWorker fakes the worker — letting us drive responses without
// spinning up a real Web Worker (which jsdom doesn't fully support).
class MockWorker {
  onMessageHandlers: Array<(event: MessageEvent) => void> = [];
  onErrorHandlers: Array<(event: ErrorEvent) => void> = [];
  terminated = false;
  posted: unknown[] = [];

  addEventListener(type: 'message' | 'error', cb: (event: any) => void): void {
    if (type === 'message') this.onMessageHandlers.push(cb);
    if (type === 'error') this.onErrorHandlers.push(cb);
  }

  postMessage(data: unknown): void {
    this.posted.push(data);
  }

  terminate(): void {
    this.terminated = true;
  }

  // Test helper: simulate a tokens response for the most-recent posted request
  respondTokens(theme: string, tokens: any[][]): void {
    const last = this.posted[this.posted.length - 1] as { id: number };
    const event = new MessageEvent('message', {
      data: { id: last.id, kind: 'tokens', theme, tokens },
    });
    for (const handler of this.onMessageHandlers) handler(event);
  }

  respondError(error: string): void {
    const last = this.posted[this.posted.length - 1] as { id: number };
    const event = new MessageEvent('message', {
      data: { id: last.id, kind: 'error', error },
    });
    for (const handler of this.onMessageHandlers) handler(event);
  }
}

let mockWorker: MockWorker;

function poolFor(idleMs = 60_000) {
  return createDiffHighlighterPool({
    workerFactory: () => {
      // Construct a fresh worker on each (re)boot so the test can
      // observe the difference across termination cycles.
      mockWorker = new MockWorker();
      return mockWorker as unknown as Worker;
    },
    idleTerminateMs: idleMs,
  });
}

describe('diffHighlighterPool', () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  it('lazy-boots the worker on first request', async () => {
    const pool = poolFor();
    expect(pool.isActive).toBe(false);
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    expect(pool.isActive).toBe(true);
    expect(mockWorker.posted).toHaveLength(1);
    mockWorker.respondTokens('github-dark', [[{ content: 'a' }]]);
    await expect(promise).resolves.toEqual([[{ content: 'a' }]]);
  });

  it('discards theme-mismatched responses', async () => {
    const pool = poolFor();
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    // Simulate a response whose theme doesn't match the request
    mockWorker.respondTokens('github-light', [[{ content: 'a', color: '#000' }]]);
    const result = await promise;
    // Mismatched theme falls back to empty token arrays of the same length
    expect(result).toEqual([[]]);
  });

  it('rejects all in-flight requests on worker error', async () => {
    const pool = poolFor();
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    const errorEvent = new ErrorEvent('error', { message: 'boom' });
    for (const handler of mockWorker.onErrorHandlers) handler(errorEvent);
    await expect(promise).rejects.toThrow('boom');
  });

  it('idle-terminates after the configured timeout', async () => {
    vi.useFakeTimers();
    const pool = poolFor(1000);
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    mockWorker.respondTokens('github-dark', [[{ content: 'a' }]]);
    await promise;
    expect(pool.isActive).toBe(true);
    vi.advanceTimersByTime(1100);
    expect(mockWorker.terminated).toBe(true);
    expect(pool.isActive).toBe(false);
  });

  it('reboots after termination on next request', async () => {
    vi.useFakeTimers();
    const pool = poolFor(1000);
    const p1 = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    mockWorker.respondTokens('github-dark', [[{ content: 'a' }]]);
    await p1;

    vi.advanceTimersByTime(1100);
    expect(pool.isActive).toBe(false);
    const oldWorker = mockWorker;

    const p2 = pool.tokenize({ lines: ['b'], lang: 'typescript', theme: 'github-dark' });
    expect(pool.isActive).toBe(true);
    expect(mockWorker).not.toBe(oldWorker);
    mockWorker.respondTokens('github-dark', [[{ content: 'b' }]]);
    await expect(p2).resolves.toEqual([[{ content: 'b' }]]);
  });

  it('does not idle-terminate while requests are in flight', async () => {
    vi.useFakeTimers();
    const pool = poolFor(1000);
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    vi.advanceTimersByTime(2000);
    expect(mockWorker.terminated).toBe(false);
    expect(pool.isActive).toBe(true);
    mockWorker.respondTokens('github-dark', [[{ content: 'a' }]]);
    await promise;
  });

  it('manual terminate rejects pending requests', async () => {
    const pool = poolFor();
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    pool.terminate();
    await expect(promise).rejects.toThrow('worker terminated');
    expect(pool.isActive).toBe(false);
  });

  it('surfaces tokenizer errors', async () => {
    const pool = poolFor();
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    mockWorker.respondError('grammar load failed');
    await expect(promise).rejects.toThrow('grammar load failed');
  });

  it('rejects on an unrecognised response kind instead of stranding the caller', async () => {
    const pool = poolFor();
    const promise = pool.tokenize({ lines: ['a'], lang: 'typescript', theme: 'github-dark' });
    const last = mockWorker.posted[mockWorker.posted.length - 1] as { id: number };
    const event = new MessageEvent('message', {
      data: { id: last.id, kind: 'weird-from-the-future' },
    });
    for (const handler of mockWorker.onMessageHandlers) handler(event);
    await expect(promise).rejects.toThrow(/unrecognised tokenizer response/i);
  });
});
