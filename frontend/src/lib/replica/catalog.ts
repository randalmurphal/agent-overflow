// Bounded, versioned sidebar metadata. I/O shares the replica session's
// identity token, connection, failure latch and purge lifecycle.
import { validOwnershipEpoch } from '../transport/entityIndex';
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';

export interface CatalogRows { projects: ProjectWithCounts; threads: Thread; groups: ThreadGroup }
export type CatalogKind = keyof CatalogRows;
export const MAX_CATALOG_ROWS = 5000;
export const MAX_CATALOG_CHARS = 4 * 1024 * 1024;
const MAX_ROW_CHARS = 64 * 1024;
interface Catalog { version: 2; generation: string; stamp: string; rows: unknown[] }
export function catalogKey(kind: CatalogKind): string { return `catalog:${kind}`; }

type RecordValue = Record<string, unknown>;
function object(value: unknown): value is RecordValue {
  return !!value && typeof value === 'object' && !Array.isArray(value);
}
function fields(row: RecordValue, strings: readonly string[], numbers: readonly string[], booleans: readonly string[] = []): boolean {
  return strings.every((key) => typeof row[key] === 'string')
    && numbers.every((key) => typeof row[key] === 'number' && Number.isFinite(row[key]))
    && booleans.every((key) => typeof row[key] === 'boolean');
}
function optional(row: RecordValue, strings: readonly string[], numbers: readonly string[], booleans: readonly string[] = []): boolean {
  const present = (keys: readonly string[]) => keys.filter((key) => row[key] !== undefined && row[key] !== null);
  return fields(row, present(strings), present(numbers), present(booleans));
}
function validRow(row: unknown, kind: CatalogKind): boolean {
  if (!object(row)) return false;
  if (kind === 'projects') {
    const project = row.project;
    return object(project) && fields(project, ['id', 'name', 'path'], ['sortPosition', 'createdAt', 'updatedAt'], ['archived'])
      && optional(project, ['color', 'remoteURL', 'rootCommit'], [])
      && fields(row, [], ['threadCount']) && optional(row, [], ['lastActive']);
  }
  if (kind === 'groups') return fields(row, ['id', 'name', 'projectId'], ['createdAt', 'updatedAt'])
    && optional(row, [], ['pinnedAt', 'pinGroup']);
  return fields(row, ['id', 'title', 'provider', 'workspacePath', 'projectPath', 'model'], ['createdAt', 'updatedAt'], ['archived'])
    && (row.ownershipEpoch === undefined || validOwnershipEpoch(row.ownershipEpoch))
    && (row.provider === 'claude' || row.provider === 'codex')
    && optional(row, ['sessionRef', 'pendingForkRef', 'projectId', 'worktreePath', 'branch', 'prRef', 'mode',
      'reasoningEffort', 'runtimeMode', 'discussionId', 'parentThreadId', 'forkedFromThreadId', 'lastTokenUsage',
      'worktreeSetupState', 'importSource', 'groupId'],
    ['contextWindow', 'autoCompactStandardPercent', 'autoCompactExtendedPercent', 'latestTurnCompletedAt',
      'lastReadAt', 'pinnedAt', 'pinGroup'],
    ['fastMode', 'hasActionableProposedPlan', 'hasIncompleteTurn', 'isDraft']);
}

export function readCatalogRecord<K extends CatalogKind>(raw: unknown, generation: string, kind: K, stamp: string): CatalogRows[K][] | null {
  if (!object(raw) || raw.version !== 2 || raw.generation !== generation || raw.stamp !== stamp || !stamp || !Array.isArray(raw.rows)
    || raw.rows.length > MAX_CATALOG_ROWS) return null;
  let chars = 0;
  for (const row of raw.rows) {
    if (!validRow(row, kind)) return null;
    const size = JSON.stringify(row).length;
    if (size > MAX_ROW_CHARS || (chars += size) > MAX_CATALOG_CHARS) return null;
  }
  return raw.rows as CatalogRows[K][];
}

export function makeCatalogRecord<K extends CatalogKind>(generation: string, kind: K, rows: readonly CatalogRows[K][], stamp: string): Catalog {
  const kept: unknown[] = [];
  let chars = 0;
  for (const row of rows) {
    // Flatten Svelte proxies per row; never allocate a catalog-sized string.
    const encoded = JSON.stringify(row);
    if (encoded.length > MAX_ROW_CHARS || !validRow(row, kind)) continue;
    if (kept.length === MAX_CATALOG_ROWS || chars + encoded.length > MAX_CATALOG_CHARS) break;
    kept.push(JSON.parse(encoded));
    chars += encoded.length;
  }
  return { version: 2, generation, stamp, rows: kept };
}
