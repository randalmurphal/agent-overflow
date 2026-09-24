// The Windows launcher's /loading.js (cmd/agent-overflow-windows/loading.js)
// runs on /loading and the distro picker while the backend boots. It polls
// /loading.json every 500 ms while the document is visible, pauses while it
// is hidden, and asks at once when it is shown again. The launcher package
// only builds for Windows and has no JavaScript runtime, so the script's
// behavior is tested here against a detached document.

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const script = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), '../../../../cmd/agent-overflow-windows/loading.js'),
  'utf8',
);

interface Page {
  doc: Document;
  fetch: ReturnType<typeof vi.fn>;
  setVisibility(state: DocumentVisibilityState): void;
  /** Answer the oldest unanswered request. */
  answer(): void;
  pending(): number;
}

const report = { title: 'Starting Agent Overflow', status: 'Applying migration 2 of 7 add_index', phase: 'store.migrate', step: 2, steps: 7, elapsedMs: 61_000 };

function load(initial: DocumentVisibilityState, { held = false } = {}): Page {
  const doc = document.implementation.createHTMLDocument('loading');
  doc.body.innerHTML = '<div id="ao-loading-title"></div><div id="ao-loading-status">Booting backend in WSL...</div><div id="ao-loading-meta"></div>';
  let visibility = initial;
  Object.defineProperty(doc, 'visibilityState', { configurable: true, get: () => visibility });
  const answers: Array<() => void> = [];
  const fetch = vi.fn((url: string) => {
    expect(url).toBe('/loading.json');
    const response = { ok: true, json: async () => report };
    if (!held) return Promise.resolve(response);
    return new Promise((resolveFetch) => answers.push(() => resolveFetch(response)));
  });
  new Function('document', 'fetch', 'setTimeout', 'clearTimeout', script)(doc, fetch, setTimeout, clearTimeout);
  return {
    doc,
    fetch,
    setVisibility(state) {
      visibility = state;
      doc.dispatchEvent(new Event('visibilitychange'));
    },
    answer() {
      answers.shift()?.();
    },
    pending: () => answers.length,
  };
}

describe('launcher loading.js', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('polls every 500 ms while visible and renders the report', async () => {
    const page = load('visible');
    await vi.advanceTimersByTimeAsync(0);
    expect(page.fetch).toHaveBeenCalledTimes(1);
    expect(page.doc.getElementById('ao-loading-status')?.textContent).toBe(report.status);
    expect(page.doc.getElementById('ao-loading-meta')?.textContent).toBe('Step 2 of 7 · 1:01 elapsed');
    await vi.advanceTimersByTimeAsync(499);
    expect(page.fetch).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(page.fetch).toHaveBeenCalledTimes(2);
  });

  it('pauses on hide and asks at once on show, every time', async () => {
    const page = load('visible');
    await vi.advanceTimersByTimeAsync(0);
    expect(page.fetch).toHaveBeenCalledTimes(1);

    for (let round = 0; round < 2; round++) {
      const before = page.fetch.mock.calls.length;
      page.setVisibility('hidden');
      await vi.advanceTimersByTimeAsync(10_000);
      expect(page.fetch).toHaveBeenCalledTimes(before);

      page.setVisibility('visible');
      await vi.advanceTimersByTimeAsync(0);
      expect(page.fetch).toHaveBeenCalledTimes(before + 1);
      await vi.advanceTimersByTimeAsync(500);
      expect(page.fetch).toHaveBeenCalledTimes(before + 2);
    }
  });

  it('does not schedule the next ask when an answer lands while hidden', async () => {
    const page = load('visible', { held: true });
    expect(page.fetch).toHaveBeenCalledTimes(1);
    page.setVisibility('hidden');
    page.answer();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(page.fetch).toHaveBeenCalledTimes(1);

    page.setVisibility('visible');
    expect(page.fetch).toHaveBeenCalledTimes(2);
  });

  it('keeps one request in flight across a hide and show', async () => {
    const page = load('visible', { held: true });
    page.setVisibility('hidden');
    page.setVisibility('visible');
    expect(page.fetch).toHaveBeenCalledTimes(1);

    page.answer();
    await vi.advanceTimersByTimeAsync(500);
    expect(page.fetch).toHaveBeenCalledTimes(2);
    expect(page.pending()).toBe(1);
  });

  it('waits for a page loaded hidden to be shown', async () => {
    const page = load('hidden');
    await vi.advanceTimersByTimeAsync(10_000);
    expect(page.fetch).not.toHaveBeenCalled();
    page.setVisibility('visible');
    expect(page.fetch).toHaveBeenCalledTimes(1);
  });
});
