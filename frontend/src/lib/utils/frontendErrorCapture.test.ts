import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  flushFrontendErrors,
  frontendErrorCaptureStateForTest,
  installFrontendErrorCapture,
  resetFrontendErrorCaptureForTest,
} from './frontendErrorCapture';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { copyToClipboard, reportCopyFailure } from './clipboard';

function dispatchError(message: string, options: { error?: unknown; filename?: string } = {}): void {
  window.dispatchEvent(
    new ErrorEvent('error', {
      message,
      // `error` is the thrown value: pass null/objects through verbatim
      // (WebKitGTK nulls it for non-Error throws); only default when the
      // caller didn't specify one.
      error: 'error' in options ? options.error : new Error(message),
      filename: options.filename ?? 'app.js',
      lineno: 12,
      colno: 7,
    }),
  );
}

function dispatchRejection(reason: unknown): void {
  // happy-dom lacks a PromiseRejectionEvent constructor; a plain Event
  // with `reason` grafted on matches the listener's runtime contract.
  const event = new Event('unhandledrejection');
  Object.defineProperty(event, 'reason', { value: reason });
  window.dispatchEvent(event);
}

function dispatchCspViolation(fields: Record<string, unknown>): void {
  // happy-dom lacks a SecurityPolicyViolationEvent constructor; a plain Event
  // with the report fields grafted on matches the listener's runtime contract.
  const event = new Event('securitypolicyviolation');
  for (const [key, value] of Object.entries(fields)) {
    Object.defineProperty(event, key, { value });
  }
  document.dispatchEvent(event);
}

function reportedLines(call = 0): string[] {
  return (getBindingMock('ReportFrontendErrorBatch')?.mock.calls[call]?.[0] ?? []) as string[];
}

describe('frontendErrorCapture', () => {
  afterEach(() => {
    resetFrontendErrorCaptureForTest();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('persists caught clipboard failures with write-time state and no copied text', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const focus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    const activation = { isActive: true };
    const event = new MouseEvent('click', { detail: 1 });
    Object.defineProperty(event, 'isTrusted', { value: true });
    const clock = vi.spyOn(performance, 'now').mockReturnValue(event.timeStamp + 12);
    vi.stubGlobal('isSecureContext', true);
    vi.stubGlobal('navigator', {
      userActivation: activation,
      clipboard: {
        writeText: async () => {
          focus.mockReturnValue(false);
          activation.isActive = false;
          clock.mockReturnValue(event.timeStamp + 200);
          throw new DOMException('Write was refused', 'NotAllowedError');
        },
      },
    });

    expect(await copyToClipboard('private clipboard payload', event)).toBe(false);
    await flushFrontendErrors();

    const lines = reportedLines();
    expect(lines).toHaveLength(1);
    const record = JSON.parse(lines[0]);
    expect(record).toMatchObject({ kind: 'diagnostic', message: 'Clipboard write failed:' });
    expect(record.stack).toContain('NotAllowedError');
    expect(record.stack).toContain('"focusedAtWrite":true');
    expect(record.stack).toContain('"activeAtWrite":true');
    expect(record.stack).toContain('"eventTrusted":true');
    expect(record.stack).toContain('"eventType":"click"');
    expect(record.stack).toContain('"eventDetail":1');
    const writeState = JSON.parse(record.stack.split('\n', 1)[0]);
    expect(writeState.eventAgeAtWrite).toBeCloseTo(12);
    expect(record.stack).toContain('"focusedAtFailure":false');
    expect(record.stack).toContain('"activeAtFailure":false');
    expect(lines.join('')).not.toContain('private clipboard payload');
  });

  it('persists structural copy-button failures through the same diagnostic path', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();
    vi.spyOn(console, 'error').mockImplementation(() => undefined);

    reportCopyFailure('[static-code-copy] handler failed', new Error('code host is incomplete'));
    await flushFrontendErrors();

    const record = JSON.parse(reportedLines()[0]);
    expect(record.message).toBe('[static-code-copy] handler failed');
    expect(record.stack).toContain('code host is incomplete');
  });

  it('persists window error events with message, stack, and location', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    dispatchError('render boom');
    await flushFrontendErrors();

    const lines = reportedLines();
    expect(lines).toHaveLength(1);
    expect(JSON.parse(lines[0])).toMatchObject({
      kind: 'error',
      message: 'render boom',
      source: 'app.js',
      line: 12,
      col: 7,
      seen: 1,
    });
    expect(JSON.parse(lines[0]).stack).toContain('render boom');
  });

  it('prefers the event message when the thrown value is not an Error', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    // WebKitGTK shape for a non-Error throw: the event message is the
    // only signal and must not be shadowed by String(null) ("null").
    dispatchError("TypeError: null is not an object (evaluating 'x.y')", { error: null });
    await flushFrontendErrors();

    const record = JSON.parse(reportedLines()[0]);
    expect(record.message).toBe("TypeError: null is not an object (evaluating 'x.y')");
    expect(record.stack).toBe('');
  });

  it('stringifies a non-Error thrown value when the event has no message', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    dispatchError('', { error: { code: 5 } });
    await flushFrontendErrors();

    expect(JSON.parse(reportedLines()[0]).message).toBe('{"code":5}');
  });

  it('persists unhandled rejections, including non-Error reasons', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    dispatchRejection(new Error('lease failed'));
    dispatchRejection({ code: 'ECONN' });
    await flushFrontendErrors();

    const lines = reportedLines();
    expect(lines).toHaveLength(2);
    expect(JSON.parse(lines[0])).toMatchObject({
      kind: 'unhandledrejection',
      message: 'lease failed',
    });
    expect(JSON.parse(lines[1])).toMatchObject({
      kind: 'unhandledrejection',
      message: '{"code":"ECONN"}',
    });
  });

  it('persists Content-Security-Policy refusals with the directive and the blocked load', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    dispatchCspViolation({
      effectiveDirective: 'img-src',
      violatedDirective: 'img-src',
      blockedURI: 'https://cdn.example/pic.png?t=secret-token',
      sourceFile: 'https://127.0.0.1:1/app.js',
      lineNumber: 3,
      columnNumber: 9,
    });
    dispatchCspViolation({ effectiveDirective: 'script-src', violatedDirective: 'script-src', blockedURI: '' });
    await flushFrontendErrors();

    const lines = reportedLines();
    expect(lines).toHaveLength(2);
    const first = JSON.parse(lines[0]);
    expect(first).toMatchObject({ kind: 'csp', source: 'https://127.0.0.1:1/app.js', line: 3, col: 9 });
    expect(first.message).toContain('img-src refused https://cdn.example/pic.png');
    expect(first.message).not.toContain('secret-token');
    expect(JSON.parse(lines[1]).message).toBe('script-src refused (inline)');
  });

  it('caps repeated signatures and samples the overflow', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    const error = new Error('same throw site');
    for (let i = 0; i < 120; i++) {
      dispatchError('same throw site', { error });
    }
    await flushFrontendErrors();

    const lines = reportedLines();
    // 10 reported outright, plus the every-100th sample at seen=100.
    expect(lines).toHaveLength(11);
    expect(JSON.parse(lines.at(-1) ?? '{}').seen).toBe(100);
  });

  it('keeps the seen counter across flushes', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    const error = new Error('recurring');
    dispatchError('recurring', { error });
    await flushFrontendErrors();
    dispatchError('recurring', { error });
    await flushFrontendErrors();

    expect(JSON.parse(reportedLines(1)[0]).seen).toBe(2);
  });

  it('folds high-cardinality signatures into a bounded overflow bucket', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    for (let i = 0; i < 1_100; i++) {
      const message = `failed for item-${i}`;
      dispatchError(message, { error: new Error(message) });
    }

    // 1000 distinct signatures admitted, plus one shared overflow bucket
    // that obeys the per-signature cap instead of growing the map.
    const state = frontendErrorCaptureStateForTest();
    expect(state.distinctSignatures).toBe(1_001);
    expect(state.pendingCount).toBeLessThanOrEqual(100);
  });

  it('installs listeners only once', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();
    installFrontendErrorCapture();

    dispatchError('once');
    await flushFrontendErrors();

    const lines = reportedLines();
    expect(lines).toHaveLength(1);
    expect(JSON.parse(lines[0]).seen).toBe(1);
  });

  it('ignores resource-load error events with no payload', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    window.dispatchEvent(new Event('error'));
    await flushFrontendErrors();

    expect(getBindingMock('ReportFrontendErrorBatch')?.mock.calls ?? []).toHaveLength(0);
  });

  it('redacts credential-like query params from messages and stacks', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    const error = new Error(
      'ws connect failed: ws://host/ws?token=supersecret123&access_token=access456&api_key=key789&x=1',
    );
    error.stack =
      'Error: boom\n    at ws://host/ws?refresh_token=refresh123&client_secret=secret456&id_token=id789:1:1';
    dispatchError(error.message, { error });
    await flushFrontendErrors();

    const record = JSON.parse(reportedLines()[0]);
    expect(record.message).toContain('?token=[redacted]&access_token=[redacted]&api_key=[redacted]&x=1');
    expect(record.stack).toContain('?refresh_token=[redacted]&client_secret=[redacted]&id_token=[redacted]');
    expect(JSON.stringify(record)).not.toContain('supersecret123');
    expect(JSON.stringify(record)).not.toContain('access456');
    expect(JSON.stringify(record)).not.toContain('key789');
    expect(JSON.stringify(record)).not.toContain('refresh123');
    expect(JSON.stringify(record)).not.toContain('secret456');
    expect(JSON.stringify(record)).not.toContain('id789');
  });

  it('falls back to a stripped record when a line exceeds the byte cap', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    // Control characters JSON-escape to 6 chars each, so capped fields
    // (message 2048 + stack 8192 + source 2048 chars) serialize to ~74KB
    // — the only realistic way past the 64KB line cap after field caps.
    const error = new Error('\u0001'.repeat(3_000));
    error.stack = '\u0001'.repeat(10_000);
    dispatchError(error.message, { error, filename: '\u0001'.repeat(3_000) });
    await flushFrontendErrors();

    const lines = reportedLines();
    expect(lines).toHaveLength(1);
    const record = JSON.parse(lines[0]);
    expect(record.stack).toBe('');
    expect(record.message.length).toBeLessThanOrEqual(513);
    expect(new TextEncoder().encode(lines[0]).length).toBeLessThanOrEqual(64 * 1024);
  });

  it('re-queues a failed batch and delivers it on the next flush', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    let failures = 1;
    setBindingMock('ReportFrontendErrorBatch', async () => {
      if (failures-- > 0) throw new Error('transport blip');
      return '/tmp/frontend-errors.jsonl';
    });
    installFrontendErrorCapture();

    dispatchError('boom during reconnect');
    await flushFrontendErrors();
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(1);

    await flushFrontendErrors();
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(0);
    const calls = getBindingMock('ReportFrontendErrorBatch')?.mock.calls ?? [];
    expect(calls).toHaveLength(2);
    expect(JSON.parse((calls[1][0] as string[])[0]).message).toBe('boom during reconnect');
    expect(warn).not.toHaveBeenCalled();
  });

  it('drops the batch after repeated flush failures', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('ReportFrontendErrorBatch', async () => {
      throw new Error('persistent failure');
    });
    installFrontendErrorCapture();

    dispatchError('poisoned?');
    await flushFrontendErrors();
    await flushFrontendErrors();
    await flushFrontendErrors();

    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(0);
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('dropped error batch'),
      expect.anything(),
    );
  });

  it('disables capture permanently when the backend refuses the method', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('ReportFrontendErrorBatch', async () => {
      throw Object.assign(new Error('method not registered'), { code: 'method_not_found' });
    });
    installFrontendErrorCapture();

    dispatchError('remote client error');
    await flushFrontendErrors();

    expect(frontendErrorCaptureStateForTest().reporterUnavailable).toBe(true);
    expect(warn).toHaveBeenCalledTimes(1);

    dispatchError('another error');
    await flushFrontendErrors();
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(0);
    expect(getBindingMock('ReportFrontendErrorBatch')?.mock.calls).toHaveLength(1);
  });

  it('chunks large flushes under the batch byte budget', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    // Control-char stacks escape to ~6 bytes per char, so each record
    // serializes to ~49KB even after the 8K-char stack cap — 30 of them
    // is ~1.5MB, forcing at least two byte-budgeted batches.
    for (let i = 0; i < 30; i++) {
      const error = new Error(`bulk ${i}`);
      error.stack = `frame-${i} ` + '\u0001'.repeat(10_000);
      dispatchError(`bulk ${i}`, { error });
    }
    await flushFrontendErrors();

    const calls = getBindingMock('ReportFrontendErrorBatch')?.mock.calls ?? [];
    expect(calls.length).toBeGreaterThan(1);
    let total = 0;
    const encoder = new TextEncoder();
    for (const call of calls) {
      const lines = call[0] as string[];
      const bytes = lines.reduce((sum, line) => sum + encoder.encode(line).length + 1, 0);
      expect(bytes).toBeLessThanOrEqual(1024 * 1024);
      total += lines.length;
    }
    expect(total).toBe(30);
  });

  it('survives a failing reporter without throwing', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('ReportFrontendErrorBatch', async () => {
      throw new Error('transport down');
    });
    installFrontendErrorCapture();

    dispatchError('boom while offline');
    await expect(flushFrontendErrors()).resolves.toBeUndefined();
    // First failure re-queues silently; the warn comes on terminal drops.
    expect(warn).not.toHaveBeenCalled();
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(1);
  });

  it('flushes on the batching timer without an explicit flush call', async () => {
    vi.useFakeTimers();
    try {
      setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
      installFrontendErrorCapture();

      dispatchError('timer flush');
      expect(getBindingMock('ReportFrontendErrorBatch')?.mock.calls ?? []).toHaveLength(0);
      await vi.advanceTimersByTimeAsync(1_100);

      expect(getBindingMock('ReportFrontendErrorBatch')?.mock.calls).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('flushes on pagehide', async () => {
    setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
    installFrontendErrorCapture();

    dispatchError('closing now');
    window.dispatchEvent(new Event('pagehide'));
    await vi.waitFor(() => {
      expect(getBindingMock('ReportFrontendErrorBatch')?.mock.calls).toHaveLength(1);
    });
  });
});
