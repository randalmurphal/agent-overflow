import { describe, expect, it, vi, afterEach } from 'vitest';
import { requestIframeCapture } from './captureHtml';

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
});
