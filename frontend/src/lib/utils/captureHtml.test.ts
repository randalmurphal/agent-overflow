import { describe, expect, it, vi, afterEach } from 'vitest';
import { requestIframeCapture, requestIframeCaptureTiles } from './captureHtml';

// requestIframeCapture is the parent-side messenger for the
// iframe-internal capture script. We can exercise the postMessage
// round-trip in happy-dom by stubbing the iframe's contentWindow
// and dispatching synthetic MessageEvent payloads. The actual DOM
// rendering happens inside the iframe's injected script (lazy-loaded
// modern-screenshot) which is server-side concern.

afterEach(() => {
  vi.useRealTimers();
});

function makeIframeWithContentWindow(): {
  iframe: HTMLIFrameElement;
  postedMessages: unknown[];
  fakeWindow: Window;
} {
  const iframe = document.createElement('iframe');
  document.body.appendChild(iframe);
  const postedMessages: unknown[] = [];
  // Replace the natural contentWindow with a stub that captures
  // postMessage calls and matches identity in MessageEvent.source.
  const fakeWindow = {
    postMessage: (msg: unknown) => {
      postedMessages.push(msg);
    },
  } as unknown as Window;
  Object.defineProperty(iframe, 'contentWindow', {
    configurable: true,
    get: () => fakeWindow,
  });
  return { iframe, postedMessages, fakeWindow };
}

describe('requestIframeCapture', () => {
  it('resolves with pngBase64 when the iframe responds with capture-result', async () => {
    const { iframe, postedMessages, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCapture(iframe, 'req-1', 1000);
    // The capture request is posted into the iframe.
    expect(postedMessages).toHaveLength(1);

    // Simulate the iframe's response.
    const event = new MessageEvent('message', {
      data: { aoDesign: 'capture-result', requestId: 'req-1', pngBase64: 'AAEC' },
      source: fakeWindow,
    });
    window.dispatchEvent(event);

    await expect(promise).resolves.toBe('AAEC');
  });

  it('rejects when the iframe responds with capture-error', async () => {
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCapture(iframe, 'req-2', 1000);
    const event = new MessageEvent('message', {
      data: { aoDesign: 'capture-error', requestId: 'req-2', error: 'modern-screenshot failed' },
      source: fakeWindow,
    });
    window.dispatchEvent(event);

    await expect(promise).rejects.toThrow('modern-screenshot failed');
  });

  it('ignores responses with mismatched requestId', async () => {
    vi.useFakeTimers();
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCapture(iframe, 'req-correct', 500);

    // Send a wrong-id response — should be ignored.
    const wrong = new MessageEvent('message', {
      data: { aoDesign: 'capture-result', requestId: 'req-other', pngBase64: 'WRONG' },
      source: fakeWindow,
    });
    window.dispatchEvent(wrong);

    // Capture must time out because the right response never arrives.
    vi.advanceTimersByTime(600);
    await expect(promise).rejects.toThrow(/timed out/);
  });

  it('rejects when the iframe has no contentWindow', async () => {
    const iframe = document.createElement('iframe');
    Object.defineProperty(iframe, 'contentWindow', {
      configurable: true,
      get: () => null,
    });
    await expect(requestIframeCapture(iframe, 'req-x', 100)).rejects.toThrow(
      /contentWindow/,
    );
  });

  it('sends mode=single in the outbound envelope', () => {
    const { iframe, postedMessages } = makeIframeWithContentWindow();
    void requestIframeCapture(iframe, 'req-mode', 1000);
    expect(postedMessages).toHaveLength(1);
    expect(postedMessages[0]).toMatchObject({
      aoDesign: 'capture',
      requestId: 'req-mode',
      mode: 'single',
    });
  });
});

describe('requestIframeCaptureTiles', () => {
  it('sends mode=tiles in the outbound envelope', () => {
    const { iframe, postedMessages } = makeIframeWithContentWindow();
    void requestIframeCaptureTiles(iframe, 'req-tiles', 1000);
    expect(postedMessages).toHaveLength(1);
    expect(postedMessages[0]).toMatchObject({
      aoDesign: 'capture',
      requestId: 'req-tiles',
      mode: 'tiles',
    });
  });

  it('resolves with tiles + clipped when iframe responds with capture-tiles-result', async () => {
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCaptureTiles(iframe, 'req-1', 1000);

    const event = new MessageEvent('message', {
      data: {
        aoDesign: 'capture-tiles-result',
        requestId: 'req-1',
        tilesJpegBase64: ['AAEC', 'AwQF', 'BgcI'],
        clipped: true,
      },
      source: fakeWindow,
    });
    window.dispatchEvent(event);

    await expect(promise).resolves.toEqual({
      tiles: ['AAEC', 'AwQF', 'BgcI'],
      clipped: true,
    });
  });

  it('defaults clipped to false when the iframe omits the flag', async () => {
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCaptureTiles(iframe, 'req-noflag', 1000);

    const event = new MessageEvent('message', {
      data: {
        aoDesign: 'capture-tiles-result',
        requestId: 'req-noflag',
        tilesJpegBase64: ['AAEC'],
      },
      source: fakeWindow,
    });
    window.dispatchEvent(event);

    await expect(promise).resolves.toEqual({ tiles: ['AAEC'], clipped: false });
  });

  it('rejects when the iframe responds with capture-error', async () => {
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCaptureTiles(iframe, 'req-2', 1000);
    const event = new MessageEvent('message', {
      data: { aoDesign: 'capture-error', requestId: 'req-2', error: 'unknown capture mode' },
      source: fakeWindow,
    });
    window.dispatchEvent(event);

    await expect(promise).rejects.toThrow('unknown capture mode');
  });

  it('rejects when the iframe responds with zero tiles', async () => {
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCaptureTiles(iframe, 'req-empty', 1000);
    const event = new MessageEvent('message', {
      data: {
        aoDesign: 'capture-tiles-result',
        requestId: 'req-empty',
        tilesJpegBase64: [],
      },
      source: fakeWindow,
    });
    window.dispatchEvent(event);

    await expect(promise).rejects.toThrow(/zero tiles/);
  });

  it('rejects when the iframe replies to the wrong mode (kind mismatch)', async () => {
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCaptureTiles(iframe, 'req-kind', 1000);
    // The single-mode reply must not satisfy a tiles request.
    const event = new MessageEvent('message', {
      data: { aoDesign: 'capture-result', requestId: 'req-kind', pngBase64: 'AAEC' },
      source: fakeWindow,
    });
    window.dispatchEvent(event);

    await expect(promise).rejects.toThrow(/unexpected capture result kind/);
  });

  it('ignores responses with mismatched requestId', async () => {
    vi.useFakeTimers();
    const { iframe, fakeWindow } = makeIframeWithContentWindow();
    const promise = requestIframeCaptureTiles(iframe, 'req-correct', 500);

    const wrong = new MessageEvent('message', {
      data: {
        aoDesign: 'capture-tiles-result',
        requestId: 'req-other',
        tilesJpegBase64: ['WRONG'],
      },
      source: fakeWindow,
    });
    window.dispatchEvent(wrong);

    vi.advanceTimersByTime(600);
    await expect(promise).rejects.toThrow(/timed out/);
  });

  it('rejects when the iframe has no contentWindow', async () => {
    const iframe = document.createElement('iframe');
    Object.defineProperty(iframe, 'contentWindow', {
      configurable: true,
      get: () => null,
    });
    await expect(requestIframeCaptureTiles(iframe, 'req-x', 100)).rejects.toThrow(
      /contentWindow/,
    );
  });
});
