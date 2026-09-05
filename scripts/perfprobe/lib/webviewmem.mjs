// The PowerShell sampler consumes only this shape, after CDP owner validation.
export function samplerIdentity(manifest, browserPid) {
  if (!Number.isSafeInteger(browserPid) || browserPid <= 0) throw new Error('sampler needs a positive browser PID');
  if (manifest.browserPid !== undefined && Number(manifest.browserPid) !== browserPid) {
    throw new Error('sampler browser PID differs from the manifest');
  }
  const identity = {
    instanceId: manifest.instanceId,
    targetId: manifest.target?.targetId,
    pageMarker: manifest.target?.pageMarker,
    origin: manifest.origin,
    browserPid,
  };
  for (const field of ['instanceId', 'targetId', 'pageMarker', 'origin']) {
    if (typeof identity[field] !== 'string' || !identity[field].trim()) throw new Error(`sampler identity has no ${field}`);
  }
  return identity;
}
