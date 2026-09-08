import { UsageBucket } from '../stores/bindings';

const totals = ['inputTokens', 'outputTokens', 'cacheReadInputTokens', 'cacheCreationInputTokens',
  'reasoningOutputTokens', 'costUsd', 'turnCount', 'sessionCount', 'unpricedRows'] as const;

/** One bucket per group across selected hosts; inputs are never mutated. */
export function combineUsageBuckets(rows: readonly UsageBucket[]): UsageBucket[] {
  const buckets = new Map<string, UsageBucket>();
  for (const row of rows) {
    const current = buckets.get(row.bucket);
    if (!current) { buckets.set(row.bucket, new UsageBucket(row)); continue; }
    for (const key of totals) current[key] += row[key] ?? 0;
    if (current.costSource !== row.costSource) current.costSource = '';
  }
  return [...buckets.values()];
}
