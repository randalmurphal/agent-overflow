// The online probe contract. Keep this table declarative so dispatch, CDP
// command checks, and the lease use the same ownership decision.

const READ = 'read';
const MEASURE = 'measure';
const PAGE_OBSERVER = 'page-observer';
const MUTATE = 'mutate';
const TRACE = 'trace';
const PROFILER = 'profiler';
const COUNTER = 'counter';

const define = (kind, ...capabilities) => ({ kind, capabilities: new Set([READ, MEASURE, kind, ...capabilities]) });

// A probe not listed here cannot talk to CDP. Offline probes do not import
// cdp.mjs and are therefore unaffected by this table.
export const PROBE_POLICIES = Object.freeze({
  ab: define(MUTATE, TRACE, PROFILER),
  activetrim: define(MUTATE, PROFILER),
  alloc: define(PROFILER),
  animations: define(MEASURE),
  attrflap: define(PAGE_OBSERVER),
  blinkpages: define(MEASURE, TRACE),
  checkerboard: define(TRACE),
  churn: define(TRACE, PROFILER),
  'compositor-contract': define(MEASURE, TRACE),
  cpu: define(PROFILER),
  csshold: define(MUTATE),
  describenodes: define(MEASURE),
  detached: define(MEASURE),
  doccount: define(MEASURE),
  driveburn: define(MUTATE),
  drivestop: define(MUTATE),
  editcmdgrowth: define(TRACE, PROFILER),
  editcmdpages: define(TRACE, PROFILER),
  edittypes: define(TRACE, PROFILER),
  emulate: define(MUTATE),
  evalq: define(MUTATE),
  followcheck: define(PAGE_OBSERVER),
  framedrops: define(PAGE_OBSERVER),
  frames: define(TRACE),
  gpuheap: define(TRACE),
  gpuinfo: define(MEASURE),
  gpumalloc: define(TRACE),
  gpupressure: define(MUTATE, TRACE),
  heapsnapshot: define(PROFILER),
  jumpwatch: define(PAGE_OBSERVER),
  layers: define(MEASURE, TRACE),
  layersfull: define(MEASURE, TRACE),
  loadolderdrive: define(MUTATE),
  mainstalls: define(PAGE_OBSERVER),
  markdownstate: define(MEASURE),
  markdownwatch: define(PAGE_OBSERVER),
  memdump: define(TRACE),
  mutations: define(PAGE_OBSERVER),
  oilpan: define(TRACE, PROFILER),
  oilpanspaces: define(TRACE, PROFILER),
  overview: define(MEASURE),
  pageping: define(MEASURE),
  paintinv: define(TRACE),
  procinfo: define(MEASURE),
  realuse: define(PAGE_OBSERVER, COUNTER),
  'realuse-state': define(MEASURE),
  resizewatch: define(PAGE_OBSERVER),
  sample: define(TRACE),
  'scroll-contract': define(MUTATE),
  scrolldrift: define(PAGE_OBSERVER),
  scrollgesture: define(MUTATE),
  'snapshot-detached': define(MEASURE),
  'snapshot-node': define(MEASURE),
  'snapshot-signal': define(MEASURE),
  'snapshot-string-rope': define(MEASURE),
  spritecheck: define(MUTATE),
  spritecheck2: define(MUTATE),
  stopsnap: define(MUTATE),
  svgcost: define(TRACE, PROFILER),
  tiles: define(TRACE),
  transforms: define(MEASURE),
  targets: define(MEASURE),
  webviewmem: define(COUNTER),
});

export const PROBE_KINDS = Object.freeze({ READ, MEASURE, PAGE_OBSERVER, MUTATE, TRACE, PROFILER, COUNTER });

export function probeNameFromArgv(argv = process.argv) {
  const raw = argv[1] || '';
  return raw.split(/[\\/]/).at(-1)?.replace(/\.mjs$/, '') || '';
}

export function policyForProbe(name) {
  const policy = PROBE_POLICIES[name];
  if (!policy) throw new Error(`perfprobe: ${name || '<unknown>'} has no declarative online probe policy`);
  return policy;
}

export function methodKind(method) {
  if (/^(Input\.|Page\.(?:reload|navigate|addScriptToEvaluateOnNewDocument|removeScriptToEvaluateOnNewDocument|set|bringToFront|close|createIsolatedWorld)|Emulation\.|Memory\.simulatePressureNotification)/.test(method)) return MUTATE;
  if (/^(Tracing\.)/.test(method)) return TRACE;
  if (/^(Profiler\.|HeapProfiler\.(?:start|stop|take|collect|add|remove|get|enable|disable))/.test(method)) return PROFILER;
  if (/^(Runtime\.(?:evaluate|callFunctionOn|queryObjects|releaseObject|addBinding|removeBinding)|Page\.(?:enable|disable)|DOM\.)/.test(method)) return PAGE_OBSERVER;
  return MEASURE;
}

export function methodAllowed(policy, method) {
  const kind = methodKind(method);
  return policy.capabilities.has(kind) || (kind === PAGE_OBSERVER && policy.capabilities.has(MEASURE));
}

const COMPATIBLE = new Set([
  `${COUNTER}:${PAGE_OBSERVER}`,
  `${PAGE_OBSERVER}:${COUNTER}`,
  `${MEASURE}:${MEASURE}`,
  `${MEASURE}:${COUNTER}`,
  `${COUNTER}:${MEASURE}`,
  `${MEASURE}:${PAGE_OBSERVER}`,
  `${PAGE_OBSERVER}:${MEASURE}`,
  `${READ}:${COUNTER}`,
  `${COUNTER}:${READ}`,
  `${READ}:${PAGE_OBSERVER}`,
  `${PAGE_OBSERVER}:${READ}`,
  `${READ}:${MEASURE}`,
  `${MEASURE}:${READ}`,
  `${READ}:${READ}`,
]);

export function instrumentsCompatible(left, right) {
  return left === right || COMPATIBLE.has(`${left}:${right}`);
}
