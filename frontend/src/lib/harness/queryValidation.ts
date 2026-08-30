export interface HarnessQueryError {
  error: string;
}

const QUERY_KEYS: Readonly<Record<string, readonly string[]>> = {
  viewport: ['v', 'kind', 'pageId', 'settledMs', 'textHead'],
  element: ['v', 'kind', 'pageId', 'selector', 'textCap', 'includeScroll'],
  globals: ['v', 'kind', 'pageId', 'name', 'args'],
  perf: ['v', 'kind', 'pageId', 'op', 'longFrameMs', 'meters', 'budgetsMs', 'runId'],
  monitor: ['v', 'kind', 'pageId', 'op', 'runId', 'monitorIds', 'heartbeatTimeoutMs', 'atMs', 'compatibilityLeg', 'withRunId'],
  reload: ['v', 'kind', 'pageId', 'delayMs'],
  open: ['v', 'kind', 'pageId', 'threadId', 'newPane'],
};

function fail(message: string): HarnessQueryError {
  return { error: message };
}

function str(spec: Record<string, unknown>, key: string): string {
  const raw = spec[key];
  return typeof raw === 'string' ? raw : '';
}

export function validateQueryShape(spec: Record<string, unknown>): HarnessQueryError | null {
  if (!Object.prototype.hasOwnProperty.call(spec, 'v')) return fail('query spec requires v');
  const version = spec.v;
  if (typeof version !== 'number' || !Number.isFinite(version) || !Number.isInteger(version)) {
    return fail('query spec v must be a finite integer');
  }
  if (version !== 1) return fail(`unsupported query version ${version} (this bridge speaks v1)`);
  const kind = str(spec, 'kind');
  if (!kind) return fail('query spec has no kind');
  const allowed = QUERY_KEYS[kind];
  if (!allowed) return fail(`unknown query kind ${JSON.stringify(kind)}`);
  const allowedSet = new Set(allowed);
  for (const key of Object.keys(spec)) {
    if (!allowedSet.has(key)) return fail(`unknown field ${JSON.stringify(key)} for query kind ${JSON.stringify(kind)}`);
  }
  return null;
}
