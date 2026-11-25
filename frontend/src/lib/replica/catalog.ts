import { safeRepositoryMetadata } from '../utils/repositoryCoordinates';
// Bounded, versioned sidebar metadata. I/O shares the replica session's
// identity token, connection, failure latch and purge lifecycle.
import { validOwnershipEpoch } from '../transport/entityIndex';
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';
import { isProviderID } from '../types/providers';

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
const EMPTY_KEYS: readonly string[] = [];
const PROJECT_REQ_STR = ['id', 'name', 'path'] as const;
const PROJECT_REQ_NUM = ['sortPosition', 'createdAt', 'updatedAt'] as const;
const PROJECT_REQ_BOOL = ['archived'] as const;
const PROJECT_OPT_STR = ['color', 'repositoryID', 'identityError'] as const;
const PROJECT_WRAP_REQ_NUM = ['threadCount'] as const;
const PROJECT_WRAP_OPT_NUM = ['lastActive'] as const;
const GROUP_REQ_STR = ['id', 'name', 'projectId'] as const;
const GROUP_REQ_NUM = ['createdAt', 'updatedAt'] as const;
const THREAD_REQ_STR = ['id', 'title', 'provider', 'workspacePath', 'projectPath', 'model'] as const;
const THREAD_REQ_NUM = ['createdAt', 'updatedAt'] as const;
const THREAD_REQ_BOOL = ['archived'] as const;
const ATTENTION_REQ_STR = ['kind', 'reason'] as const;
const ATTENTION_REQ_NUM = ['at'] as const;
const THREAD_OPT_STR = [
  'sessionRef', 'pendingForkRef', 'projectId', 'worktreePath', 'branch', 'prRef', 'mode',
  'reasoningEffort', 'runtimeMode', 'forkedFromThreadId', 'lastTokenUsage',
  'worktreeSetupState', 'importSource', 'groupId',
] as const;
const THREAD_OPT_NUM = [
  'contextWindow', 'autoCompactStandardPercent', 'autoCompactExtendedPercent', 'latestTurnCompletedAt',
  'lastReadAt', 'pinnedAt', 'pinGroup',
] as const;
const THREAD_OPT_BOOL = [
  'forkPreparing', 'fastMode', 'hasActionableProposedPlan', 'hasIncompleteTurn', 'hasFailedTurn', 'isDraft',
] as const;

function fields(row: RecordValue, strings: readonly string[], numbers: readonly string[], booleans: readonly string[] = EMPTY_KEYS): boolean {
  for (let i = 0; i < strings.length; i++) {
    if (typeof row[strings[i]] !== 'string') return false;
  }
  for (let i = 0; i < numbers.length; i++) {
    const v = row[numbers[i]];
    if (typeof v !== 'number' || !Number.isFinite(v)) return false;
  }
  for (let i = 0; i < booleans.length; i++) {
    if (typeof row[booleans[i]] !== 'boolean') return false;
  }
  return true;
}
function optional(row: RecordValue, strings: readonly string[], numbers: readonly string[], booleans: readonly string[] = EMPTY_KEYS): boolean {
  for (let i = 0; i < strings.length; i++) {
    const v = row[strings[i]];
    if (v !== undefined && v !== null && typeof v !== 'string') return false;
  }
  for (let i = 0; i < numbers.length; i++) {
    const v = row[numbers[i]];
    if (v !== undefined && v !== null && (typeof v !== 'number' || !Number.isFinite(v))) return false;
  }
  for (let i = 0; i < booleans.length; i++) {
    const v = row[booleans[i]];
    if (v !== undefined && v !== null && typeof v !== 'boolean') return false;
  }
  return true;
}
function validRow(row: unknown, kind: CatalogKind): boolean {
  if (!object(row)) return false;
  if (kind === 'projects') {
    const project = row.project;
    return object(project) && fields(project, PROJECT_REQ_STR, PROJECT_REQ_NUM, PROJECT_REQ_BOOL)
      && optional(project, PROJECT_OPT_STR, EMPTY_KEYS)
      && fields(row, EMPTY_KEYS, PROJECT_WRAP_REQ_NUM) && optional(row, EMPTY_KEYS, PROJECT_WRAP_OPT_NUM);
  }
  if (kind === 'groups') return fields(row, GROUP_REQ_STR, GROUP_REQ_NUM);
  return fields(row, THREAD_REQ_STR, THREAD_REQ_NUM, THREAD_REQ_BOOL)
    && (row.ownershipEpoch === undefined || validOwnershipEpoch(row.ownershipEpoch))
    && isProviderID(row.provider)
    && (row.attention == null || (object(row.attention) && fields(row.attention, ATTENTION_REQ_STR, ATTENTION_REQ_NUM)))
    && optional(row, THREAD_OPT_STR, THREAD_OPT_NUM, THREAD_OPT_BOOL);
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
  return safeRepositoryMetadata(raw.rows) as CatalogRows[K][];
}

/**
 * The encoded form of rows that are values, or null for a row the catalog
 * leaves out. A row object that is never written into encodes the same way
 * every time, so a writer of such rows keeps one cache and each write
 * encodes only the rows that changed since the last. A remembered row is
 * shared by every record built from it, so it is frozen.
 */
export type CatalogRowCache = WeakMap<object, { plain: unknown; chars: number } | null>;

function frozen<T>(value: T): T {
  if (value !== null && typeof value === 'object') {
    for (const child of Object.values(value)) frozen(child);
    Object.freeze(value);
  }
  return value;
}

export function makeCatalogRecord<K extends CatalogKind>(
  generation: string, kind: K, rows: readonly CatalogRows[K][], stamp: string, cache?: CatalogRowCache,
): Catalog {
  const kept: unknown[] = [];
  let chars = 0;
  for (const row of rows) {
    let entry = cache?.get(row);
    if (entry === undefined) {
      // Flatten Svelte proxies per row; never allocate a catalog-sized string.
      const encoded = JSON.stringify(safeRepositoryMetadata(row));
      entry = encoded.length > MAX_ROW_CHARS || !validRow(row, kind) ? null : { plain: JSON.parse(encoded), chars: encoded.length };
      if (cache) {
        if (entry !== null) frozen(entry.plain);
        cache.set(row, entry);
      }
    }
    if (entry === null) continue;
    if (kept.length === MAX_CATALOG_ROWS || chars + entry.chars > MAX_CATALOG_CHARS) break;
    kept.push(entry.plain);
    chars += entry.chars;
  }
  return { version: 2, generation, stamp, rows: kept };
}
