import { expect, it } from 'vitest';
import { TRACK_IMPORT_KEY } from './config/sharedWorkerKeys';
import { trackImports } from './config/importTrackerPlugin';
import { retireFile, WorkerFiles, type OpenFile, type Retirement } from './sharedWorker';

const realTimers = { setTimeout, clearTimeout, setInterval, clearInterval, setImmediate, clearImmediate };

type Deferred = { promise: Promise<void>; resolve(): void; reject(error: Error): void };

function deferred(): Deferred {
  let resolve = () => {};
  let reject: (error: Error) => void = () => {};
  const promise = new Promise<void>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
}

/** The bound's timer as the retirement sees it: each `sleep` call, with
 * the gate that ends it and the signal that cancels it. */
type Bound = { calls: Array<{ ms: number; signal: AbortSignal; expire(): void }> };

function bound(): Bound & { sleep: Retirement['sleep'] } {
  const calls: Bound['calls'] = [];
  return {
    calls,
    sleep: (ms, signal) =>
      new Promise<void>((resolve, reject) => {
        calls.push({ ms, signal, expire: resolve });
        signal.addEventListener('abort', () => reject(signal.reason));
      }),
  };
}

/** A retirement that records its steps; `resetDocument` runs `teardown`
 * on its first pass, as a document reset runs a file's disconnect code. */
function retirement(steps: string[], options: { sleep?: Retirement['sleep']; teardown?: () => void; calls?: string[] } = {}): Retirement {
  let passes = 0;
  return {
    takeUnansweredBindings: () => options.calls ?? [],
    restoreVitest: () => steps.push('restore vitest'),
    resetDocument: async () => {
      steps.push('reset document');
      if (passes++ === 0) options.teardown?.();
    },
    resetModules: () => steps.push('reset modules'),
    globalChanges: () => [],
    importBoundMs: 10_000,
    sleep: options.sleep ?? bound().sleep,
  };
}

function worker(target = new EventTarget()): { files: WorkerFiles; target: EventTarget } {
  const files = new WorkerFiles(realTimers);
  files.trackListeners(target);
  return { files, target };
}

it("releases what a file's import installs after the file's tests, once the import settles", async () => {
  const { files, target } = worker();
  const { file } = files.open('a.test.ts');
  const gate = deferred();
  let calls = 0;
  // The import's module: evaluates when the gate opens and adds a listener.
  const imported = files.track(gate.promise.then(() => target.addEventListener('ping', () => (calls += 1))), './late');
  const steps: string[] = [];
  const retired = retireFile(file, files, retirement(steps));
  await flushMicrotasks();
  expect(steps, 'nothing runs while the import is in flight').toEqual([]);
  gate.resolve();
  await imported;
  expect(await retired).toEqual([]);
  expect(steps).toEqual(['restore vitest', 'reset document', 'reset modules']);
  target.dispatchEvent(new Event('ping'));
  expect(calls, "the import's listener is released").toBe(0);
});

it('keeps the file open through its document teardown, and drains what teardown imports before modules reset', async () => {
  const { files, target } = worker();
  const { file } = files.open('a.test.ts');
  const teardownImport = deferred();
  let calls = 0;
  const steps: string[] = [];
  const retired = retireFile(file, files, retirement(steps, {
    teardown: () => {
      target.addEventListener('ping', () => (calls += 1));
      void files.track(teardownImport.promise.then(() => steps.push('teardown import settled')), './teardown');
    },
  }));
  await flushMicrotasks();
  expect(steps, 'the second pass waits for the teardown import').toEqual(['restore vitest', 'reset document']);
  teardownImport.resolve();
  expect(await retired).toEqual([]);
  expect(steps).toEqual(['restore vitest', 'reset document', 'teardown import settled', 'restore vitest', 'reset document', 'reset modules']);
  target.dispatchEvent(new Event('ping'));
  expect(calls, "the teardown's listener was the file's and is released with it").toBe(0);
  expect(files.open('b.test.ts').leftovers, 'nothing is charged to the next file').toBeNull();
});

it('fails a file whose import outlasts the bound, naming the import, and still retires it', async () => {
  const { files } = worker();
  const { file } = files.open('a.test.ts');
  void files.track(new Promise(() => {}), "src/lib/x.ts: import('./never')");
  const timer = bound();
  const steps: string[] = [];
  const retired = retireFile(file, files, retirement(steps, { sleep: timer.sleep }));
  await flushMicrotasks();
  expect(timer.calls.map(({ ms }) => ms)).toEqual([10_000]);
  timer.calls[0]!.expire();
  expect((await retired).map((error) => error.message)).toEqual([
    "a.test.ts left dynamic imports in flight 10000ms after its last test: src/lib/x.ts: import('./never'). " +
      'Await the import, or the condition that waits for it, in the test that starts it.',
  ]);
  expect(steps).toEqual(['restore vitest', 'reset document', 'reset modules']);
});

it('starts no bound for a file with no import in flight, and cancels the bound once the imports settle', async () => {
  const { files } = worker();
  const idle = bound();
  expect(await retireFile(files.open('a.test.ts').file, files, retirement([], { sleep: idle.sleep }))).toEqual([]);
  expect(idle.calls, 'no import, no timer').toEqual([]);
  const { file } = files.open('b.test.ts');
  void files.track(Promise.resolve(), './settled');
  expect(await retireFile(file, files, retirement([], { sleep: idle.sleep }))).toEqual([]);
  expect(idle.calls, 'an import that had settled, no timer').toEqual([]);
  const busy = bound();
  const { file: c } = files.open('c.test.ts');
  const gate = deferred();
  void files.track(gate.promise, './late');
  const retired = retireFile(c, files, retirement([], { sleep: busy.sleep }));
  await flushMicrotasks();
  expect(busy.calls.map(({ signal }) => signal.aborted)).toEqual([false]);
  gate.resolve();
  expect(await retired).toEqual([]);
  expect(busy.calls.map(({ signal }) => signal.aborted), 'the timer is cancelled').toEqual([true]);
});

it("keeps a retirement that gave up on its own file's state: the next file's import is the next file's to wait for", async () => {
  const { files } = worker();
  const { file: a } = files.open('a.test.ts');
  const aImport = deferred();
  void files.track(aImport.promise, './a-late');
  const aBound = bound();
  const aRetired = retireFile(a, files, retirement([], { sleep: aBound.sleep }));
  await flushMicrotasks();
  aBound.calls[0]!.expire();
  expect((await aRetired).map((error) => error.message)).toEqual([expect.stringContaining("in flight 10000ms after its last test: ./a-late.")]);
  const { file: b } = files.open('b.test.ts');
  const bImport = deferred();
  void files.track(bImport.promise, './b-late');
  aImport.resolve();
  await flushMicrotasks();
  const steps: string[] = [];
  const bRetired = retireFile(b, files, retirement(steps));
  await flushMicrotasks();
  expect(steps, "b's retirement waits for b's import").toEqual([]);
  bImport.resolve();
  expect(await bRetired).toEqual([]);
  expect(steps).toEqual(['restore vitest', 'reset document', 'reset modules']);
});

it('waits for an import that a settled import started', async () => {
  const { files } = worker();
  const { file } = files.open('a.test.ts');
  const second = deferred();
  void files.track(Promise.resolve().then(() => void files.track(second.promise, './second')), './first');
  const steps: string[] = [];
  const retired = retireFile(file, files, retirement(steps));
  await flushMicrotasks();
  expect(steps).toEqual([]);
  expect(file.inFlight()).toEqual(['./second']);
  second.resolve();
  expect(await retired).toEqual([]);
});

it('fails a file for an import that rejects after its last test, naming the import and the rejection', async () => {
  const { files } = worker();
  const { file } = files.open('a.test.ts');
  const failing = deferred();
  const tracked = files.track(failing.promise, './missing');
  expect(tracked, 'the caller gets the import itself').toBe(failing.promise);
  const retired = retireFile(file, files, retirement([]));
  await flushMicrotasks();
  failing.reject(new Error('no such module'));
  expect((await retired).map((error) => error.message)).toEqual([
    'a.test.ts started a dynamic import that rejected after its last test: ./missing: no such module. Await the import in the test that starts it.',
  ]);
  await expect(tracked, 'and it still rejects there').rejects.toThrow('no such module');
});

it('leaves an import that rejected during the tests to its caller', async () => {
  const { files } = worker();
  const { file }: { file: OpenFile } = files.open('a.test.ts');
  const tracked = files.track(Promise.reject(new Error('chunk failed')), './chunk');
  await expect(tracked).rejects.toThrow('chunk failed');
  expect(await retireFile(file, files, retirement([]))).toEqual([]);
});

it('fails a file for the binding calls no mock answered, after its teardown', async () => {
  const { files } = worker();
  const steps: string[] = [];
  const errors = await retireFile(files.open('a.test.ts').file, files, retirement(steps, { calls: ['GitPush, after a.test.ts ended'] }));
  expect(errors.map((error) => error.message)).toEqual([
    '1 binding call had no mock installed:\n  GitPush, after a.test.ts ended\nInstall an answer with setBindingMock in the test.',
  ]);
});

it('releases a listener added while no file is open at once, and fails the next file naming it', async () => {
  const { files, target } = worker();
  await retireFile(files.open('a.test.ts').file, files, retirement([]));
  let calls = 0;
  target.addEventListener('ping', () => (calls += 1));
  target.dispatchEvent(new Event('ping'));
  expect(calls).toBe(0);
  expect(files.open('b.test.ts').leftovers?.message).toMatch(
    /^after a\.test\.ts retired and before b\.test\.ts started, code installed a resource, released at once:\n {2}a 'ping' listener, installed at\n    at [^\n]*sharedWorker\.test\.ts/,
  );
  expect(files.open('c.test.ts').leftovers, 'the next file after that opens clean').toBeNull();
});

it('wraps each dynamic import in the tracker call, on its own lines', () => {
  const code = "const a = 1;\nconst m = import('./x');\nconst n = import(\n  `./${name}`\n);\n";
  const tracked = trackImports(code, 'src/lib/y.ts')!;
  const key = JSON.stringify(TRACK_IMPORT_KEY);
  expect(tracked).toBe(
    'const a = 1;\n' +
      `const m = globalThis[Symbol.for(${key})](import('./x'), "src/lib/y.ts: import('./x')");\n` +
      `const n = globalThis[Symbol.for(${key})](import(\n  \`./\${name}\`\n), "src/lib/y.ts: import(\`./\${name}\`)");\n`,
  );
  expect(trackImports("const s = 'import(';", 'src/lib/y.ts'), 'text that only looks like an import').toBeNull();
});

it("tracks this file's own dynamic imports and hands back the same promise", async () => {
  const worker = globalThis as { [key: symbol]: unknown };
  const key = Symbol.for(TRACK_IMPORT_KEY);
  const runnerTrack = worker[key] as (promise: Promise<unknown>, label: string) => Promise<unknown>;
  const seen: Array<{ promise: Promise<unknown>; label: string }> = [];
  worker[key] = (promise: Promise<unknown>, label: string) => {
    seen.push({ promise, label });
    return runnerTrack(promise, label);
  };
  try {
    const loading = import('./openThreadPanes');
    // The label is the transformed source's: TypeScript's emit double-quotes.
    expect(seen.map(({ label }) => label)).toEqual(['src/test/sharedWorker.test.ts: import("./openThreadPanes")']);
    expect(seen[0]?.promise).toBe(loading);
    await loading;
  } finally {
    worker[key] = runnerTrack;
  }
});
