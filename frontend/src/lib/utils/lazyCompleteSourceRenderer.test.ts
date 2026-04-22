import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetForTests,
  getCachedSource,
  registerPainter,
  type PainterSpec,
} from './lazyCompleteSourceRenderer';

// Helper: advance fake timers past the primitive's debounce and flush
// all microtasks (loaders, per-element renders, cache paints) so
// assertions see the post-scan state deterministically. Uses the async
// variant of the vitest timer API so microtasks run interleaved with
// timer ticks, then does a settle loop to drain any remaining awaits
// in the scan/process chain.
async function flushScan(): Promise<void> {
  await vi.advanceTimersByTimeAsync(100);
  for (let i = 0; i < 16; i++) await Promise.resolve();
}

function makeSpec<M>(overrides: Partial<PainterSpec<M>>): PainterSpec<M> {
  return {
    selector: 'pre.test-painter',
    key: 'test-painter',
    readSource: (el) => el.textContent ?? '',
    render: () => {},
    load: () => Promise.resolve({} as M),
    ...overrides,
  };
}

describe('lazyCompleteSourceRenderer', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    __resetForTests();
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  it('renders each matching element once; the idempotency attribute actually matches the primitive selector', async () => {
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = `<span class="rendered">${src}</span>`;
      },
    );
    const load = vi.fn(() => Promise.resolve({}));

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">alpha</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render, load }));
    await flushScan();

    // render fired exactly once, element got the idempotency attribute.
    expect(render).toHaveBeenCalledTimes(1);
    const el = wrap.querySelector<HTMLElement>('pre.test-painter')!;
    expect(el.hasAttribute('data-rendered-test-painter')).toBe(true);

    // Regression: the attribute the primitive writes must actually be
    // the one its selector excludes. The prior implementation wrote
    // `data-mermaid-rendered` but selected on `[data-mermaidRendered]`
    // (camelCase), so the guard never matched anything. If that bug
    // ever returns, this line fails.
    expect(
      document.querySelectorAll(
        'pre.test-painter:not([data-rendered-test-painter])',
      ).length,
    ).toBe(0);
  });

  it('cache hit: a second element with the same source is painted from cache without re-invoking render', async () => {
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = `<span class="rendered">${src}</span>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">shared</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    await flushScan();
    expect(render).toHaveBeenCalledTimes(1);

    // Add a second, identical-source element.
    const second = document.createElement('pre');
    second.className = 'test-painter';
    second.textContent = 'shared';
    wrap.appendChild(second);
    await flushScan();

    expect(render).toHaveBeenCalledTimes(1);
    expect(second.innerHTML).toContain('<span class="rendered">shared</span>');
    expect(second.getAttribute('data-rendered-test-painter')).not.toBeNull();
  });

  it('failed render paints the source AND the error, caches the composite, and does not re-invoke on re-scan', async () => {
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      () => {
        throw new Error('boom');
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">bad source</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    await flushScan();

    expect(render).toHaveBeenCalledTimes(1);
    const el = wrap.querySelector<HTMLElement>('pre.test-painter')!;
    // Source is visible.
    expect(el.querySelector('.rendered-source')?.textContent).toBe('bad source');
    // Error is visible below it.
    expect(el.querySelector('.rendered-error')?.textContent).toContain('boom');

    // Simulate the Svelte `{@html}` wipe that replaces the container's
    // children with a new element carrying the same source. The cached
    // source+error composite must be reinstated without re-invoking
    // the failing renderer — the bug this test pins is the prior
    // implementation's error-quoting-error loop where the error text
    // became the next scan's input source.
    wrap.innerHTML = '<pre class="test-painter">bad source</pre>';
    await flushScan();

    expect(render).toHaveBeenCalledTimes(1); // still 1, not 2
    const fresh = wrap.querySelector<HTMLElement>('pre.test-painter')!;
    expect(fresh.querySelector('.rendered-source')?.textContent).toBe('bad source');
    expect(fresh.querySelector('.rendered-error')?.textContent).toContain('boom');
  });

  it('mutations outside .markdown-body do not schedule a scan', async () => {
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = src;
      },
    );

    // Register painter first with an empty document — no painters yet.
    registerPainter(makeSpec({ render }));
    await flushScan();
    expect(render).toHaveBeenCalledTimes(0);

    // Mutate an element outside any .markdown-body subtree.
    const stray = document.createElement('div');
    stray.className = 'sidebar';
    stray.innerHTML = '<pre class="test-painter">stray</pre>';
    document.body.appendChild(stray);
    await flushScan();

    // The observer filters out mutations that don't touch a
    // .markdown-body, so the stray element is not processed.
    expect(render).toHaveBeenCalledTimes(0);
    const el = stray.querySelector<HTMLElement>('pre.test-painter')!;
    expect(el.hasAttribute('data-rendered-test-painter')).toBe(false);
  });

  it('two painters share a single observer and scan independently by key', async () => {
    const alphaRender = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = `<alpha>${src}</alpha>`;
      },
    );
    const betaRender = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = `<beta>${src}</beta>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML =
      '<pre class="test-painter-alpha">a</pre><pre class="test-painter-beta">b</pre>';
    document.body.appendChild(wrap);

    registerPainter(
      makeSpec({
        selector: 'pre.test-painter-alpha',
        key: 'test-painter-alpha',
        render: alphaRender,
      }),
    );
    registerPainter(
      makeSpec({
        selector: 'pre.test-painter-beta',
        key: 'test-painter-beta',
        render: betaRender,
      }),
    );
    await flushScan();

    expect(alphaRender).toHaveBeenCalledTimes(1);
    expect(betaRender).toHaveBeenCalledTimes(1);
    // Each painter's own attribute is written.
    const alpha = wrap.querySelector<HTMLElement>('pre.test-painter-alpha')!;
    const beta = wrap.querySelector<HTMLElement>('pre.test-painter-beta')!;
    expect(alpha.hasAttribute('data-rendered-test-painter-alpha')).toBe(true);
    expect(beta.hasAttribute('data-rendered-test-painter-beta')).toBe(true);
    // And they don't bleed across keys.
    expect(alpha.hasAttribute('data-rendered-test-painter-beta')).toBe(false);
  });

  it('rewriteCached is applied on cache hits so identical diagrams do not share element ids', async () => {
    let nextId = 0;
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el) => {
        el.innerHTML = `<svg id="gen-${nextId++}"><defs><linearGradient id="gen-0-grad"/></defs></svg>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">same</pre>';
    document.body.appendChild(wrap);

    let suffix = 0;
    registerPainter(
      makeSpec({
        render,
        rewriteCached: (html) => html.replace(/gen-[a-z0-9]+/g, `gen-cached-${suffix++}`),
      }),
    );
    await flushScan();

    // Second element — cache hit, rewriteCached fires.
    const second = document.createElement('pre');
    second.className = 'test-painter';
    second.textContent = 'same';
    wrap.appendChild(second);
    await flushScan();

    expect(render).toHaveBeenCalledTimes(1);
    expect(second.innerHTML).toContain('gen-cached-');
    // Sub-ids derived from the base id are rewritten consistently too.
    expect(second.innerHTML).not.toContain('gen-0');
  });

  it('registerPainter is idempotent per key — re-registering does not duplicate renders', async () => {
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = src;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">once</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    registerPainter(makeSpec({ render })); // duplicate; must be a no-op
    await flushScan();

    expect(render).toHaveBeenCalledTimes(1);
  });

  it('concurrent scans do not double-render: a DOM mutation during an in-flight render does not cause two render calls for the same element', async () => {
    // A render that awaits a timer, so we can inject a DOM mutation
    // while the first scan is still awaiting. Without the isScanning /
    // rescanRequested guards, the second scan's querySelectorAll
    // would pick up the same unmarked element and invoke render twice.
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => Promise<void>>(
      async (el, src) => {
        await new Promise<void>((r) => setTimeout(r, 200));
        el.innerHTML = `<rendered>${src}</rendered>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">once</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    // Kick the first scan past its debounce. Render's inner setTimeout
    // (200 ms) is now pending and scan is awaiting it.
    await vi.advanceTimersByTimeAsync(100);
    // Cause a DOM mutation inside .markdown-body. Without the guard
    // this would schedule a parallel scan that picks up the same el.
    wrap.appendChild(document.createElement('span'));
    // Advance past a full debounce AND the render's inner timer so
    // the first scan completes and any queued rescan runs.
    await vi.advanceTimersByTimeAsync(400);
    for (let i = 0; i < 16; i++) await Promise.resolve();

    expect(render).toHaveBeenCalledTimes(1);
  });

  it('a loader rejection does not poison the painter — the next scan retries', async () => {
    let loadAttempt = 0;
    const load = vi.fn(async () => {
      loadAttempt++;
      if (loadAttempt === 1) throw new Error('chunk load failed');
      return {};
    });
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = `<ok>${src}</ok>`;
      },
    );
    // Silence expected console.error from loadModule.
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">retry-me</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render, load }));
    await flushScan();

    // First scan: loader rejects, render never called.
    expect(loadAttempt).toBe(1);
    expect(render).toHaveBeenCalledTimes(0);

    // Trigger another scan (e.g. by mutating the markdown body).
    wrap.appendChild(document.createElement('span'));
    await flushScan();

    // Loader re-attempted, render now runs.
    expect(loadAttempt).toBe(2);
    expect(render).toHaveBeenCalledTimes(1);
    errSpy.mockRestore();
  });

  it('readSource throwing logs and does not kill other painters in the same scan', async () => {
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const goodRender = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = src;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML =
      '<pre class="test-painter-bad">x</pre><pre class="test-painter-good">y</pre>';
    document.body.appendChild(wrap);

    registerPainter(
      makeSpec({
        selector: 'pre.test-painter-bad',
        key: 'test-painter-bad',
        readSource: () => {
          throw new Error('read failed');
        },
      }),
    );
    registerPainter(
      makeSpec({
        selector: 'pre.test-painter-good',
        key: 'test-painter-good',
        render: goodRender,
      }),
    );
    await flushScan();

    // The good painter still renders; the bad one is logged and skipped.
    expect(goodRender).toHaveBeenCalledTimes(1);
    expect(errSpy).toHaveBeenCalled();
    errSpy.mockRestore();
  });

  it('sync cache-hit paint: a fresh element with a cached source is painted BEFORE the debounce fires', async () => {
    // This is the streaming-flicker regression test. During streaming,
    // Svelte's `{@html}` wipes the markdown-body every ~50 ms and the
    // debounce would never fire (each new mutation clears the prior
    // timer). The observer must paint cache hits synchronously so the
    // rendered content stays visible continuously.
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = `<rendered>${src}</rendered>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">stable</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    await flushScan();
    expect(render).toHaveBeenCalledTimes(1);

    // Simulate a streaming delta: replace the whole markdown-body
    // innerHTML with a fresh element that has the same source.
    wrap.innerHTML = '<pre class="test-painter">stable</pre>';
    // Let MutationObserver microtask run but DO NOT advance timers — a
    // sync paint must have already happened by the time the next
    // microtask tick completes.
    for (let i = 0; i < 4; i++) await Promise.resolve();

    const fresh = wrap.querySelector<HTMLElement>('pre.test-painter')!;
    expect(fresh.hasAttribute('data-rendered-test-painter')).toBe(true);
    expect(fresh.innerHTML).toContain('<rendered>stable</rendered>');
    // render was NOT re-invoked — the paint came from the cache.
    expect(render).toHaveBeenCalledTimes(1);
  });

  it('async render rejection paints the source+error fallback (same as sync throw)', async () => {
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => Promise<void>>(
      async () => {
        throw new Error('async rejection');
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">bad</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    await flushScan();

    const el = wrap.querySelector<HTMLElement>('pre.test-painter')!;
    expect(el.querySelector('.rendered-source')?.textContent).toBe('bad');
    expect(el.querySelector('.rendered-error')?.textContent).toContain('async rejection');
    expect(render).toHaveBeenCalledTimes(1);
  });

  it('LRU eviction: pushing past the cache cap drops the least-recently-used source', async () => {
    // 130 unique sources with a cache max of 128 — the first two added
    // must be evicted, so their next paint triggers a re-invocation.
    const renderCalls: string[] = [];
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        renderCalls.push(src);
        el.innerHTML = `<r>${src}</r>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));

    for (let i = 0; i < 130; i++) {
      const el = document.createElement('pre');
      el.className = 'test-painter';
      el.textContent = `src-${i}`;
      wrap.appendChild(el);
      await flushScan();
    }
    expect(renderCalls).toHaveLength(130);

    // Re-add src-0 — should be evicted, so render runs again.
    const evicted = document.createElement('pre');
    evicted.className = 'test-painter';
    evicted.textContent = 'src-0';
    wrap.appendChild(evicted);
    await flushScan();
    expect(renderCalls.filter((s) => s === 'src-0')).toHaveLength(2);

    // Re-add src-129 — most-recently-used, still in cache, no re-render.
    const kept = document.createElement('pre');
    kept.className = 'test-painter';
    kept.textContent = 'src-129';
    wrap.appendChild(kept);
    await flushScan();
    expect(renderCalls.filter((s) => s === 'src-129')).toHaveLength(1);
  });

  it('isConnected gate: an element detached mid-render does not poison the cache', async () => {
    // The first render blocks on a controllable promise so we can
    // detach the element while it's awaiting. Subsequent renders run
    // synchronously. Without the isConnected gate, the detached
    // element's wiped innerHTML would be cached and painted onto any
    // future fresh element with the same source.
    let releaseFirstRender!: () => void;
    const firstRenderGate = new Promise<void>((r) => {
      releaseFirstRender = r;
    });
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => Promise<void>>(
      async (el, src) => {
        if (render.mock.calls.length === 1) {
          await firstRenderGate;
        }
        el.innerHTML = `<r>${src}</r>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">only</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    // Kick the first scan past the debounce; render enters and awaits.
    await vi.advanceTimersByTimeAsync(100);
    for (let i = 0; i < 4; i++) await Promise.resolve();
    expect(render).toHaveBeenCalledTimes(1);

    // Detach the first element by wiping the parent.
    wrap.innerHTML = '';

    // Release the first render onto the now-detached node. The gate
    // MUST skip cache.set because el.isConnected is false.
    releaseFirstRender();
    for (let i = 0; i < 16; i++) await Promise.resolve();

    // A fresh element with the same source. Cache is empty (thanks to
    // the gate), so render runs a second time on this connected el.
    wrap.innerHTML = '<pre class="test-painter">only</pre>';
    await flushScan();

    expect(render).toHaveBeenCalledTimes(2);
    const fresh = wrap.querySelector<HTMLElement>('pre.test-painter')!;
    expect(fresh.innerHTML).toContain('<r>only</r>');
  });

  it('getCachedSource returns the source that produced the rendered output on a marked element', async () => {
    // After a successful render the primitive stashes the original
    // source in a hash-keyed LRU so callers (e.g. the mermaid
    // context-menu "Copy source" action) can recover it after
    // textContent has been replaced with the rendered output.
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el) => {
        el.innerHTML = `<svg><text>rendered</text></svg>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">graph-TD-a-to-b</pre>';
    document.body.appendChild(wrap);

    registerPainter(makeSpec({ render }));
    await flushScan();

    const el = wrap.querySelector<HTMLElement>('pre.test-painter')!;
    expect(getCachedSource('test-painter', el)).toBe('graph-TD-a-to-b');

    // An element without the idempotency attribute returns null.
    const fresh = document.createElement('pre');
    fresh.className = 'test-painter';
    fresh.textContent = 'graph-TD-a-to-b';
    expect(getCachedSource('test-painter', fresh)).toBeNull();

    // Unknown painter keys return null rather than throwing.
    expect(getCachedSource('no-such-painter', el)).toBeNull();
  });

  it('rewriteCached throwing logs and paints the raw cached html, never leaving the element stuck', async () => {
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const render = vi.fn<(el: HTMLElement, src: string, mod: unknown) => void>(
      (el, src) => {
        el.innerHTML = `<cached>${src}</cached>`;
      },
    );

    const wrap = document.createElement('div');
    wrap.className = 'markdown-body';
    wrap.innerHTML = '<pre class="test-painter">same</pre>';
    document.body.appendChild(wrap);

    registerPainter(
      makeSpec({
        render,
        rewriteCached: () => {
          throw new Error('rewrite failed');
        },
      }),
    );
    await flushScan();
    expect(render).toHaveBeenCalledTimes(1);

    // Second element — cache hit path runs rewriteCached which throws.
    const second = document.createElement('pre');
    second.className = 'test-painter';
    second.textContent = 'same';
    wrap.appendChild(second);
    await flushScan();

    // render was NOT called again (we took the cache-hit branch).
    expect(render).toHaveBeenCalledTimes(1);
    // The element still has the cached HTML painted (rewrite failure
    // is non-fatal; the raw cached value wins).
    expect(second.innerHTML).toContain('<cached>same</cached>');
    expect(errSpy).toHaveBeenCalled();
    errSpy.mockRestore();
  });
});
