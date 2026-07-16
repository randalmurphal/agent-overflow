// Decoding for backend syntax-highlight span metadata.
//
// The backend (internal/highlight) returns per-line runs as flat
// [byteLen, classId, ...] pairs over the line's UTF-8 bytes; this
// module slices the text the frontend already holds into styled
// segments. Class ids map to `syntax-<name>` CSS classes via the
// HighlightClassNames table fetched once per page load; app.css owns
// the colors per theme, so a theme toggle costs zero re-requests.

import { HighlightClassNames, HighlightSchemaVersion } from '../stores/bindings';
import type { EncodedLine } from '../../../bindings/agent-overflow/internal/highlight/models.js';

export type { EncodedLine };

export interface SpanSegment {
  text: string;
  /** `syntax-<name>` or '' for plain text. */
  className: string;
}

// Index = class id; entry '' = unstyled ("none" and anything unknown).
let classNameTable: string[] = [];
let classNamesLoaded = false;

/** Test seam + ensureSyntaxClassNames target. Id 0 maps to ''. */
export function initSyntaxClassNames(names: string[]): void {
  classNameTable = names.map((name, id) => (id === 0 || !name ? '' : `syntax-${name}`));
  classNamesLoaded = true;
}

/** Synchronous probe: true once the class-name table has loaded. The
 * persisted-blob ingest uses it (with highlightSchemaVersionSync) to
 * seed caches synchronously at row init, winning the race against the
 * children's first-render cache reads. */
export function syntaxClassNamesReady(): boolean {
  return classNamesLoaded;
}

let classNamesPromise: Promise<void> | null = null;

/** Test-only: drop the memoized table fetch so a test can exercise the
 * first-load path (including its rejection branch). */
export function resetSyntaxClassNamesForTest(): void {
  classNamesPromise = null;
  classNameTable = [];
  classNamesLoaded = false;
  schemaVersionPromise = null;
  schemaVersion = null;
}

/**
 * Fetches the classId → name table once. Span consumers await this
 * before resolving their first request so spans never render with an
 * empty table; subsequent calls are free. A rejection clears the memo
 * so the next request retries instead of pinning highlighting off for
 * the rest of the session.
 */
export function ensureSyntaxClassNames(): Promise<void> {
  classNamesPromise ??= HighlightClassNames().then(
    (names) => {
      initSyntaxClassNames(names);
    },
    (err: unknown) => {
      classNamesPromise = null;
      throw err;
    },
  );
  return classNamesPromise;
}

let schemaVersionPromise: Promise<string> | null = null;
let schemaVersion: string | null = null;

/** Test seam: pin the schema version without a fetch. */
export function initHighlightSchemaVersionForTest(version: string | null): void {
  schemaVersion = version;
  schemaVersionPromise = version === null ? null : Promise.resolve(version);
}

/**
 * Fetches the backend's span schema version once. Persisted span blobs
 * (item meta codeSpans, payload preview/full spans) carry the stamp of
 * the backend that wrote them; ingest compares against the CONNECTED
 * backend's version — which is also the RPC server that would
 * recompute — and skips mismatches so stale spans degrade to the RPC
 * path instead of coloring by an old grammar's opinion. A rejection
 * clears the memo so the next ingest retries.
 */
export function ensureHighlightSchemaVersion(): Promise<string> {
  schemaVersionPromise ??= HighlightSchemaVersion().then(
    (version) => {
      schemaVersion = version;
      return version;
    },
    (err: unknown) => {
      schemaVersionPromise = null;
      throw err;
    },
  );
  return schemaVersionPromise;
}

/** Synchronous read of the fetched schema version, or null before the
 * first ensureHighlightSchemaVersion resolves. Pairs with
 * syntaxClassNamesReady for the persisted-blob ingest's sync path. */
export function highlightSchemaVersionSync(): string | null {
  return schemaVersion;
}

/**
 * Boot warm for both tables (App.svelte onMount), so the persisted-span
 * ingest takes its synchronous zero-RPC path from the very first
 * history mount — without it, every fence/diff of the first thread
 * view defers past its children's cache reads and RPCs anyway. The
 * returned promise NEVER rejects, so boot can await it (bounded) as
 * part of startup readiness without a failure or hang holding the UI
 * hostage. Failure is non-fatal: both ensure* helpers clear their memo
 * on rejection, so the next ingest retries and the RPC path covers.
 */
export function warmHighlightTables(): Promise<void> {
  return Promise.all([ensureHighlightSchemaVersion(), ensureSyntaxClassNames()]).then(
    () => undefined,
    (err: unknown) => {
      console.warn('highlight table warm failed (next ingest retries):', err);
    },
  );
}

function syntaxClassName(id: number): string {
  return classNameTable[id] ?? '';
}

/**
 * Slices `text` into styled segments per the line's runs. Exact
 * partition: concatenating the segment texts always reproduces `text`
 * (load-bearing for copy behavior and the review pane's exact-height
 * contract). Run lengths are UTF-8 byte counts; this walk converts
 * them to UTF-16 slice offsets.
 */
export function spanSegments(text: string, line: EncodedLine | null | undefined): SpanSegment[] {
  const runs = line?.r;
  if (!text) return [];
  if (!runs || runs.length < 2) return [{ text, className: '' }];

  const segments: SpanSegment[] = [];
  let charIdx = 0;
  for (let i = 0; i + 1 < runs.length && charIdx < text.length; i += 2) {
    let bytes = runs[i] ?? 0;
    const start = charIdx;
    while (bytes > 0 && charIdx < text.length) {
      const cp = text.codePointAt(charIdx) as number;
      bytes -= cp < 0x80 ? 1 : cp < 0x800 ? 2 : cp < 0x10000 ? 3 : 4;
      charIdx += cp >= 0x10000 ? 2 : 1;
    }
    if (charIdx > start) {
      segments.push({ text: text.slice(start, charIdx), className: syntaxClassName(runs[i + 1] ?? 0) });
    }
  }
  if (charIdx < text.length) {
    segments.push({ text: text.slice(charIdx), className: '' });
  }
  return segments;
}
