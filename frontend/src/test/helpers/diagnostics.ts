// Shared harness for suites that assert an app-internal diagnostic reaches
// `ui-trace/frontend-errors.jsonl`.
//
// Deliberately asserts through the REAL capture pipeline (dedupe -> serialize
// -> batch -> `ReportFrontendErrorBatch`) rather than spying on
// `reportFrontendDiagnostic`. Two reasons, and both have bitten:
//
//  - The claim under test is that a broken invariant lands somewhere a human
//    can find it after the fact. A spy proves a function was called; it does
//    not prove the record survived the per-signature cap, the distinct-signature
//    overflow bucket, the line-size clamp, or the batcher.
//  - `vi.mock` does not reliably reach importers that are `.svelte.ts` modules
//    (see frontend/AGENTS.md § Testing), so mocking `utils/frontendErrorCapture`
//    silently fails for exactly the guards that live in stores.
//
// Every diagnostic caller follows `reportFrontendDiagnostic`'s shape: a
// CONSTANT message, with the variable data in the second `detail` argument.
// That is what keeps signatures bounded, and it is why this helper surfaces
// message and detail separately — an assertion on the message pins the
// constant, one on the detail pins the variables.

import { afterEach, beforeEach, vi } from 'vitest';
import {
  flushFrontendErrors,
  resetFrontendErrorCaptureForTest,
} from '../../lib/utils/frontendErrorCapture';
import { setBindingMock } from '../mocks/bindings-app';

export interface CapturedDiagnostic {
  kind: 'error' | 'unhandledrejection' | 'diagnostic';
  /** The constant message. */
  message: string;
  /** The variable second argument, as persisted (the record's `stack`). */
  detail: string;
}

export interface DiagnosticsCapture {
  /** Flushes the batcher, then every record captured so far. */
  all(): Promise<CapturedDiagnostic[]>;
  /** Flushes the batcher, then just the messages — the common assertion. */
  messages(): Promise<string[]>;
  /**
   * `console.warn` lines emitted during the case, joined per call.
   *
   * Every diagnostic caller pairs its report with a console line, because a
   * non-loopback session cannot persist and the console is then the only
   * evidence — so the fallback is part of the contract and is asserted here.
   * Capturing also keeps the run's output readable: these suites drive the
   * failure paths on purpose, and their warnings are expected output, not
   * signal.
   */
  warnings(): string[];
}

/**
 * Install capture for the surrounding suite. Call at describe (or file) scope;
 * it registers its own `beforeEach` (fresh capture state + binding mock) and
 * `afterEach` (detach), so nothing leaks between cases.
 */
export function installDiagnosticsCapture(): DiagnosticsCapture {
  let captured: CapturedDiagnostic[] = [];
  let warned: string[] = [];

  beforeEach(() => {
    resetFrontendErrorCaptureForTest();
    captured = [];
    warned = [];
    vi.spyOn(console, 'warn').mockImplementation((...args: unknown[]) => {
      warned.push(args.map((arg) => String(arg)).join(' '));
    });
    setBindingMock('ReportFrontendErrorBatch', (lines: unknown) => {
      for (const line of lines as string[]) {
        const record = JSON.parse(line) as {
          kind: CapturedDiagnostic['kind'];
          message: string;
          stack: string;
        };
        captured.push({ kind: record.kind, message: record.message, detail: record.stack });
      }
    });
  });

  afterEach(() => {
    resetFrontendErrorCaptureForTest();
    vi.mocked(console.warn).mockRestore();
  });

  const all = async (): Promise<CapturedDiagnostic[]> => {
    await flushFrontendErrors();
    return captured;
  };

  return {
    all,
    messages: async () => (await all()).map((record) => record.message),
    warnings: () => warned,
  };
}
