export const HISTOGRAM_BUCKET_MS = 0.25;
export const HISTOGRAM_CEILING_MS = 250;
export const HISTOGRAM_BUCKET_COUNT =
  Math.round(HISTOGRAM_CEILING_MS / HISTOGRAM_BUCKET_MS) + 1;

const PROCESS_GROUPS = ['browser', 'gpu', 'renderer', 'utility'];

function processGroup(type) {
  switch (String(type).toLowerCase()) {
    case 'browser':
      return 'browser';
    case 'gpu':
    case 'gpu-process':
      return 'gpu';
    case 'renderer':
      return 'renderer';
    default:
      return 'utility';
  }
}

function finiteNonNegative(value, label) {
  if (!Number.isFinite(value) || value < 0) {
    throw new Error(`${label} must be a finite non-negative number, got ${value}`);
  }
  return value;
}

export function sparseHistogram(entries) {
  const buckets = new Array(HISTOGRAM_BUCKET_COUNT).fill(0);
  for (const [rawIndex, rawCount] of entries ?? []) {
    const index = Number(rawIndex);
    const count = Number(rawCount);
    if (!Number.isInteger(index) || index < 0 || index >= HISTOGRAM_BUCKET_COUNT) {
      throw new Error(`histogram bucket index out of range: ${rawIndex}`);
    }
    if (!Number.isInteger(count) || count < 0) {
      throw new Error(`histogram bucket count must be a non-negative integer: ${rawCount}`);
    }
    buckets[index] += count;
  }
  return buckets;
}

export function mergeSparseHistograms(histograms) {
  const buckets = new Array(HISTOGRAM_BUCKET_COUNT).fill(0);
  for (const entries of histograms) {
    for (const [rawIndex, rawCount] of entries ?? []) {
      const index = Number(rawIndex);
      const count = Number(rawCount);
      if (!Number.isInteger(index) || index < 0 || index >= HISTOGRAM_BUCKET_COUNT) {
        throw new Error(`histogram bucket index out of range: ${rawIndex}`);
      }
      if (!Number.isInteger(count) || count < 0) {
        throw new Error(`histogram bucket count must be a non-negative integer: ${rawCount}`);
      }
      buckets[index] += count;
    }
  }
  return buckets;
}

export function histogramCount(buckets) {
  return buckets.reduce((total, count) => total + count, 0);
}

export function histogramPercentile(buckets, fraction) {
  if (!Number.isFinite(fraction) || fraction <= 0 || fraction > 1) {
    throw new Error(`histogram percentile must be in (0, 1], got ${fraction}`);
  }
  const count = histogramCount(buckets);
  if (count === 0) return 0;
  const target = Math.ceil(count * fraction);
  let seen = 0;
  for (let index = 0; index < buckets.length; index += 1) {
    seen += buckets[index] ?? 0;
    if (seen >= target) return index * HISTOGRAM_BUCKET_MS;
  }
  return HISTOGRAM_CEILING_MS;
}

export function histogramCountAbove(buckets, thresholdMs) {
  finiteNonNegative(thresholdMs, 'histogram threshold');
  const firstBucket = Math.min(
    buckets.length,
    Math.floor(thresholdMs / HISTOGRAM_BUCKET_MS) + 1,
  );
  let count = 0;
  for (let index = firstBucket; index < buckets.length; index += 1) {
    count += buckets[index] ?? 0;
  }
  return count;
}

export function estimateMissedRefreshes(buckets, baselineMs) {
  finiteNonNegative(baselineMs, 'refresh baseline');
  if (baselineMs === 0) return 0;
  let missed = 0;
  for (let index = 0; index < buckets.length; index += 1) {
    const count = buckets[index] ?? 0;
    if (count === 0) continue;
    const durationMs = index === buckets.length - 1
      ? HISTOGRAM_CEILING_MS
      : (index + 0.5) * HISTOGRAM_BUCKET_MS;
    const refreshes = Math.max(1, Math.round(durationMs / baselineMs));
    missed += count * (refreshes - 1);
  }
  return missed;
}

export function processCpuDelta(previous, current, elapsedMs, logicalProcessors) {
  finiteNonNegative(elapsedMs, 'CPU sample elapsed time');
  if (!Number.isInteger(logicalProcessors) || logicalProcessors < 1) {
    throw new Error(`logical processor count must be a positive integer, got ${logicalProcessors}`);
  }
  const previousById = new Map(
    (previous ?? []).map((process) => [Number(process.id), Number(process.cpuTime)]),
  );
  const seconds = elapsedMs / 1000;
  const cpuSeconds = Object.fromEntries(PROCESS_GROUPS.map((group) => [group, 0]));
  let newProcesses = 0;
  let disappearedProcesses = 0;
  const currentIds = new Set();

  for (const process of current ?? []) {
    const id = Number(process.id);
    const cpuTime = Number(process.cpuTime);
    currentIds.add(id);
    if (!Number.isFinite(cpuTime) || cpuTime < 0 || !previousById.has(id)) {
      newProcesses += 1;
      continue;
    }
    const previousCpuTime = previousById.get(id);
    const delta = cpuTime - previousCpuTime;
    if (!Number.isFinite(delta) || delta < 0) {
      newProcesses += 1;
      continue;
    }
    cpuSeconds[processGroup(process.type)] += delta;
  }
  for (const id of previousById.keys()) {
    if (!currentIds.has(id)) disappearedProcesses += 1;
  }

  const totalCpuSeconds = Object.values(cpuSeconds).reduce((total, value) => total + value, 0);
  const rawPercent = seconds > 0 ? (totalCpuSeconds / seconds) * 100 : 0;
  const byGroupPercent = Object.fromEntries(
    PROCESS_GROUPS.map((group) => [
      group,
      seconds > 0 ? (cpuSeconds[group] / seconds / logicalProcessors) * 100 : 0,
    ]),
  );
  return {
    processCount: (current ?? []).length,
    newProcesses,
    disappearedProcesses,
    cpuSeconds: totalCpuSeconds,
    rawPercent,
    normalizedPercent: rawPercent / logicalProcessors,
    byGroupPercent,
  };
}

export function metricMap(result) {
  return Object.fromEntries((result?.metrics ?? []).map(({ name, value }) => [name, value]));
}

export function metricDelta(previous, current, name, scale = 1) {
  const before = Number(previous?.[name]);
  const after = Number(current?.[name]);
  if (!Number.isFinite(before) || !Number.isFinite(after) || after < before) return null;
  return (after - before) * scale;
}

export function percentile(values, fraction) {
  if (!Number.isFinite(fraction) || fraction <= 0 || fraction > 1) {
    throw new Error(`percentile must be in (0, 1], got ${fraction}`);
  }
  const finite = values.filter(Number.isFinite).toSorted((left, right) => left - right);
  if (finite.length === 0) return 0;
  return finite[Math.min(finite.length - 1, Math.ceil(finite.length * fraction) - 1)];
}

export function parseNumericCsv(text) {
  const lines = text.trim().split(/\r?\n/).filter(Boolean);
  if (lines.length === 0) throw new Error('CSV is empty');
  const headers = lines[0].split(',');
  const rows = [];
  for (const [index, line] of lines.slice(1).entries()) {
    const values = line.split(',');
    if (values.join(',') === headers.join(',')) continue;
    if (values.length !== headers.length) {
      throw new Error(`CSV row ${index + 2} has ${values.length} fields, want ${headers.length}`);
    }
    const row = {};
    for (let field = 0; field < headers.length; field += 1) {
      const header = headers[field];
      const value = values[field];
      row[header] = header === 'utc' ? value : Number(value);
      if (header !== 'utc' && !Number.isFinite(row[header])) {
        throw new Error(`CSV row ${index + 2} field ${header} is not numeric: ${value}`);
      }
    }
    rows.push(row);
  }
  return rows;
}
