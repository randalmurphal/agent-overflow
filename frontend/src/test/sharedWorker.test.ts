import { expect, it } from 'vitest';
import { TRACK_IMPORT_KEY } from './config/sharedWorkerKeys';
import { trackImports } from './config/importTrackerPlugin';
import { FileImports, FileResources, retireFile, type Retirement } from './sharedWorker';

const realTimers = { setTimeout, clearTimeout, setInterval, clearInterval, setImmediate, clearImmediate };

function deferred(): { promise: Promise<void>; resolve(): void; reject(error: Error): void } {
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

/** A retirement over `imports` and `resources`, recording its steps. */
function retirement(imports: FileImports, resources: FileResources, steps: string[], sleep: Retirement['sleep'] = () => new Promise(() => {})): Retirement {
  return {
    imports,
    takeUnansweredBindings: () => [],
    restoreVitest: () => steps.push('restore vitest'),
    releaseResources: () => {
      steps.push('release');
      resources.releaseFile();
    },
    resetDocument: async () => {
      steps.push('reset document');
    },
    resetModules: () => steps.push('reset modules'),
    globalChanges: () => [],
    importBoundMs: 10_000,
    sleep,
    inFlight: () => imports.inFlight(),
  };
}

it("releases what a file's import installs after the file's tests, once the import settles", async () => {
  const target = new EventTarget();
  const resources = new FileResources(realTimers);
  resources.trackListeners(target);
  const imports = new FileImports();
  resources.openFile('a.test.ts');
  imports.openFile('a.test.ts');
  const gate = deferred();
  let calls = 0;
  // The import's module: evaluates when the gate opens and adds a listener.
  const imported = imports.track(gate.promise.then(() => target.addEventListener('ping', () => (calls += 1))), './late');
  const steps: string[] = [];
  const retired = retireFile('a.test.ts', retirement(imports, resources, steps));
  await flushMicrotasks();
  expect(steps, 'nothing is released while the import is in flight').toEqual([]);
  gate.resolve();
  await imported;
  expect(await retired).toEqual([]);
  expect(steps).toEqual(['restore vitest', 'release', 'reset document', 'reset modules']);
  target.dispatchEvent(new Event('ping'));
  expect(calls, "the import's listener is released").toBe(0);
});

it('fails a file whose import outlasts the bound, naming the import and the file', async () => {
  const imports = new FileImports();
  imports.openFile('a.test.ts');
  void imports.track(new Promise(() => {}), "src/lib/x.ts: import('./never')");
  const steps: string[] = [];
  const errors = await retireFile('a.test.ts', retirement(imports, new FileResources(realTimers), steps, async () => {}));
  expect(errors.map((error) => error.message)).toEqual([
    "a.test.ts left dynamic imports in flight 10000ms after its last test: src/lib/x.ts: import('./never') (started by a.test.ts). " +
      'Await the import, or the condition that waits for it, in the test that starts it.',
  ]);
  expect(steps.at(-1), 'the file still retires').toBe('reset modules');
});

it('fails a file for the binding calls no mock answered since its last check, before anything is released', async () => {
  const imports = new FileImports();
  imports.openFile('a.test.ts');
  const steps: string[] = [];
  const errors = await retireFile('a.test.ts', {
    ...retirement(imports, new FileResources(realTimers), steps),
    takeUnansweredBindings: () => {
      steps.push('take calls');
      return ['GitPush, after a.test.ts ended'];
    },
  });
  expect(errors.map((error) => error.message)).toEqual([
    '1 binding call had no mock installed:\n  GitPush, after a.test.ts ended\nInstall an answer with setBindingMock in the test.',
  ]);
  expect(steps[0]).toBe('take calls');
});

it('waits for an import that a settled import started', async () => {
  const imports = new FileImports();
  imports.openFile('a.test.ts');
  const second = deferred();
  void imports.track(Promise.resolve().then(() => void imports.track(second.promise, './second')), './first');
  let settled = false;
  const waiting = imports.settled().then(() => (settled = true));
  await flushMicrotasks();
  expect(settled).toBe(false);
  expect(imports.inFlight()).toEqual(['./second (started by a.test.ts)']);
  second.resolve();
  await waiting;
  expect(imports.inFlight()).toEqual([]);
});

it('hands the caller the import itself: a rejection still rejects there and leaves the in-flight set', async () => {
  const imports = new FileImports();
  imports.openFile('a.test.ts');
  const failing = deferred();
  const tracked = imports.track(failing.promise, './missing');
  expect(tracked).toBe(failing.promise);
  failing.reject(new Error('no such module'));
  await expect(tracked).rejects.toThrow('no such module');
  await imports.settled();
  expect(imports.inFlight()).toEqual([]);
});

it('releases a listener added while no file is open at once, and fails the next file naming it', () => {
  const target = new EventTarget();
  const resources = new FileResources(realTimers);
  resources.trackListeners(target);
  resources.openFile('a.test.ts');
  resources.releaseFile();
  let calls = 0;
  target.addEventListener('ping', () => (calls += 1));
  target.dispatchEvent(new Event('ping'));
  expect(calls).toBe(0);
  expect(resources.openFile('b.test.ts')?.message).toMatch(
    /^after a\.test\.ts retired and before b\.test\.ts started, code installed a resource, released at once:\n {2}a 'ping' listener, installed at\n    at [^\n]*sharedWorker\.test\.ts/,
  );
  expect(resources.openFile('c.test.ts'), 'the next file after that opens clean').toBeNull();
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
