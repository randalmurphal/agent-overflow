// src/page-stall.ts against a fake CDP session, for orderings a real page
// cannot be made to produce on demand.
import { EventEmitter } from 'node:events';
import type { CDPSession } from '@playwright/test';
import { expect, test } from './base-test.js';
import { armDebugger, pauseAndReadStack } from '../src/page-stall.js';

const SCRIPT = 'function holdMainThread() {\n  for (;;) wait();\n}\n';

interface FakeSession {
  session: CDPSession;
  calls: string[];
  emit(event: string, payload: unknown): void;
  /** Make the next `Debugger.resume` reject. */
  failResume(): void;
}

function fakeSession(): FakeSession {
  const events = new EventEmitter();
  const calls: string[] = [];
  let resumeFails = false;
  const session = {
    on: (event: string, fn: (payload: unknown) => void) => events.on(event, fn),
    once: (event: string, fn: (payload: unknown) => void) => events.once(event, fn),
    off: (event: string, fn: (payload: unknown) => void) => events.off(event, fn),
    send: async (method: string) => {
      calls.push(method);
      if (method === 'Debugger.getScriptSource') return { scriptSource: SCRIPT };
      if (method === 'Debugger.resume' && resumeFails) {
        resumeFails = false;
        throw new Error('Target closed');
      }
      return {};
    },
  } as unknown as CDPSession;
  return {
    session,
    calls,
    emit: (event, payload) => events.emit(event, payload),
    failResume: () => (resumeFails = true),
  };
}

const PAUSED = {
  callFrames: [{ functionName: 'holdMainThread', location: { scriptId: '7', lineNumber: 1, columnNumber: 2 } }],
};
const OPTIONS = { pauseMs: 50, sourceMs: 1_000, resumeMs: 1_000 };

test('a pause that timed out does not resume the next sample before it reads its source', async () => {
  const fake = fakeSession();
  const armed = await armDebugger(fake.session);
  fake.emit('Debugger.scriptParsed', { scriptId: '7', url: 'http://app/main.js', length: SCRIPT.length });

  const first = await pauseAndReadStack(armed, OPTIONS);
  expect(first.paused).toBe(false);

  fake.calls.length = 0;
  const second = pauseAndReadStack(armed, OPTIONS);
  setTimeout(() => fake.emit('Debugger.paused', PAUSED), 5);
  const stack = await second;
  expect(stack.paused).toBe(true);
  expect(fake.calls).toEqual(['Debugger.pause', 'Debugger.getScriptSource', 'Debugger.resume']);
});

test('a pause that lands after its sample gave up is resumed, and a failed resume is reported', async () => {
  const fake = fakeSession();
  const armed = await armDebugger(fake.session);
  const first = await pauseAndReadStack(armed, OPTIONS);
  expect(first.paused).toBe(false);

  fake.failResume();
  fake.emit('Debugger.paused', PAUSED);
  await expect.poll(() => fake.calls.filter((call) => call === 'Debugger.resume')).toHaveLength(1);

  const next = pauseAndReadStack(armed, OPTIONS);
  setTimeout(() => fake.emit('Debugger.paused', PAUSED), 5);
  const stack = await next;
  expect(stack.earlierResumeErrors).toEqual([expect.stringContaining('Target closed')]);
});

test('an armed session keeps a bounded number of script records', async () => {
  const fake = fakeSession();
  const armed = await armDebugger(fake.session);
  for (let i = 0; i < 2_500; i += 1) {
    fake.emit('Debugger.scriptParsed', { scriptId: `s${i}`, url: `http://app/chunk-${i}.js`, length: 10 });
    fake.emit('Debugger.scriptParsed', { scriptId: `eval${i}`, url: '', length: 10 });
  }
  expect(armed.scripts.size).toBe(2_000);
  expect(armed.scripts.has('s0')).toBe(false);
  expect(armed.scripts.has('s2499')).toBe(true);
});

test('a script is fetched once however many frames point into it, within the source budget', async () => {
  const fake = fakeSession();
  const armed = await armDebugger(fake.session);
  fake.emit('Debugger.scriptParsed', { scriptId: '7', url: 'http://app/main.js', length: SCRIPT.length });
  fake.emit('Debugger.scriptParsed', { scriptId: '8', url: 'http://app/huge.js', length: 9 * 2 ** 20 });
  const frame = (scriptId: string, lineNumber: number) => ({
    functionName: 'f',
    location: { scriptId, lineNumber, columnNumber: 0 },
  });
  const options = { ...OPTIONS, sourceBudget: { remainingChars: 8 * 2 ** 20 } };
  const stack = pauseAndReadStack(armed, options);
  setTimeout(() => fake.emit('Debugger.paused', { callFrames: [frame('7', 0), frame('7', 1), frame('8', 0), frame('7', 2)] }), 5);
  const result = await stack;
  expect(result.paused).toBe(true);
  expect(fake.calls.filter((call) => call === 'Debugger.getScriptSource')).toHaveLength(1);
  if (!result.paused) return;
  expect(result.frames[1]!.source).toContain('for (;;) wait();');
  expect(result.frames[2]!.sourceError).toMatch(/is 9437184 characters and \d+ of this failure's source budget are left/);
  expect(options.sourceBudget.remainingChars).toBe(8 * 2 ** 20 - SCRIPT.length);
});

test('the page fixture hands over a page whose debugger session is armed', async ({ page, stalledPageCapture, browserName }) => {
  test.skip(browserName !== 'chromium', 'only Chromium has CDP');
  expect(stalledPageCapture.isArmed(page)).toBe(true);
});
