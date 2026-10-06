// The `unit` project runs many files in one worker against one happy-dom
// window (vitest.config.ts). A file that replaces a window global, a DOM
// prototype member or a host object property and does not put it back
// changes every later file in that worker, in an order that varies by run.
// `sharedGlobalChanges` lets sharedWorker.ts fail the file that leaves
// such a change behind, so the leak is reported where it happens instead of
// as a flaky victim.
//
// Compared: functions and primitives, and accessor identity. Objects that
// libraries and app modules install once (trace APIs, polyfill state) are
// not, and keys added to `globalThis` are not: a module evaluated once per
// worker may define them. Keys added elsewhere are reported unless they
// are `__`-prefixed engine or framework slots (Svelte's `Element.prototype`
// fields).

type Entry = { readonly get?: unknown; readonly set?: unknown; readonly value?: unknown; readonly accessor: boolean };
type Snapshot = Map<string, Entry>;

const TARGETS: ReadonlyArray<readonly [string, () => object]> = [
  ['globalThis', () => globalThis],
  ['document', () => document],
  ['navigator', () => navigator],
  ['performance', () => performance],
  ['URL', () => URL],
  ['EventTarget.prototype', () => EventTarget.prototype],
  ['Node.prototype', () => Node.prototype],
  ['Element.prototype', () => Element.prototype],
  ['HTMLElement.prototype', () => HTMLElement.prototype],
  ['Document.prototype', () => Document.prototype],
  ['HTMLCanvasElement.prototype', () => HTMLCanvasElement.prototype],
  ['HTMLInputElement.prototype', () => HTMLInputElement.prototype],
  ['HTMLTextAreaElement.prototype', () => HTMLTextAreaElement.prototype],
  ['Range.prototype', () => Range.prototype],
];

// Vitest rebinds these per file.
const IGNORED_GLOBALS = new Set(['__vitest_worker__', '__vitest_mocker__', '__vitest_required__', 'console']);

function compared(value: unknown): boolean {
  return typeof value === 'function' || value === null || typeof value !== 'object';
}

function snapshotTarget(target: object, isGlobal: boolean): Snapshot {
  const snapshot: Snapshot = new Map();
  for (const key of Reflect.ownKeys(target)) {
    if (typeof key !== 'string' || (isGlobal && IGNORED_GLOBALS.has(key))) continue;
    const descriptor = Object.getOwnPropertyDescriptor(target, key);
    if (descriptor === undefined) continue;
    if ('value' in descriptor) {
      snapshot.set(key, { accessor: false, value: descriptor.value });
      continue;
    }
    // happy-dom globals are accessors on globalThis whose setter stores the
    // assigned value behind the same getter, so read the value as well.
    let value: unknown;
    if (isGlobal) {
      try {
        value = (target as Record<string, unknown>)[key];
      } catch {
        value = undefined;
      }
    }
    snapshot.set(key, { accessor: true, get: descriptor.get, set: descriptor.set, value });
  }
  return snapshot;
}

export type SharedGlobalsSnapshot = ReadonlyArray<Snapshot>;

export function snapshotSharedGlobals(): SharedGlobalsSnapshot {
  return TARGETS.map(([name, target]) => snapshotTarget(target(), name === 'globalThis'));
}

function changed(before: Entry, after: Entry): boolean {
  if (before.accessor !== after.accessor) return true;
  if (before.accessor && (before.get !== after.get || before.set !== after.set)) return true;
  if (!compared(before.value) && !compared(after.value)) return false;
  return !Object.is(before.value, after.value);
}

function inheritedValue(prototype: Record<string, unknown>, key: string): unknown {
  try {
    return prototype[key];
  } catch {
    return undefined;
  }
}

/** Lists every global the file changed, removed or added since `before`. */
export function sharedGlobalChanges(before: SharedGlobalsSnapshot): string[] {
  const changes: string[] = [];
  TARGETS.forEach(([name, target], index) => {
    const isGlobal = name === 'globalThis';
    const prior = before[index];
    const now = snapshotTarget(target(), isGlobal);
    for (const [key, entry] of prior) {
      const next = now.get(key);
      if (next === undefined) {
        if (entry.accessor || compared(entry.value)) changes.push(`${name}.${key} was removed`);
      } else if (changed(entry, next)) {
        changes.push(`${name}.${key} was replaced`);
      }
    }
    if (isGlobal) return;
    const inherited = Object.getPrototypeOf(target()) as Record<string, unknown> | null;
    for (const [key, entry] of now) {
      if (prior.has(key) || key.startsWith('__')) continue;
      // A restored spy on an inherited method leaves an own copy of it.
      if (!entry.accessor && inherited !== null && Object.is(entry.value, inheritedValue(inherited, key))) continue;
      changes.push(`${name}.${key} was added`);
    }
  });
  return changes;
}
