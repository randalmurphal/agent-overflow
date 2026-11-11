// The review pane's find over an open CI job log: the backend counts the
// matches per line (SearchPRCILog); this keeps those on the view's
// sections. Lines no section lists are not on the view and are not
// matches. An answer applies only to the job and log version it was read
// from.
//
// A match is its line and its index on that line, which a log that grows
// never changes. The section to open, the row that shows it (a row of
// lines is at most one block of one section, ReviewCILogView) and its
// index among that row's matches are derived against the sections shown
// at the moment of use (`locateCILogHit`).

import { CI_LOG_BLOCK_LINES } from '../stores/ciLogBlocks.svelte';
import type { CILogSearch, CILogSection } from '../types/models';

/** `data-find-key` of a row of log lines, by its first line. */
export function findCILogKey(from: number): string {
  return `cl:${from}`;
}

export interface CILogFindHit {
  /** `line` and `occurrence`, as one string. */
  readonly identity: string;
  readonly line: number;
  /** Its index among the line's matches. */
  readonly occurrence: number;
}

/** Where a log match is now. */
export interface CILogFindLocation {
  readonly sectionKey: string;
  /** The row's first line, and its `data-find-key`. */
  readonly rowFrom: number;
  readonly rowKey: string;
  /** Its index among that row's matches. */
  readonly occurrence: number;
}

function shownSections(sections: readonly CILogSection[]): CILogSection[] {
  return sections.filter((section) => section.end > section.start).sort((a, b) => a.start - b.start);
}

/** The view's matches for a search answer, in line order; at most `limit`. */
export function ciLogFindHits(search: CILogSearch, sections: readonly CILogSection[], limit: number): CILogFindHit[] {
  const out: CILogFindHit[] = [];
  const shown = shownSections(sections);
  let at = 0;
  for (const hit of search.hits) {
    while (at < shown.length && shown[at].end <= hit.line) at += 1;
    const section = shown[at];
    if (!section || section.start > hit.line) continue;
    for (let occurrence = 0; occurrence < hit.count; occurrence += 1) {
      if (out.length >= limit) return out;
      out.push({ identity: `${hit.line}:${occurrence}`, line: hit.line, occurrence });
    }
  }
  return out;
}

/** Where `hit` is against the sections shown now, or null when no section
 * lists its line. Its index in its row counts the matches `hits` (the
 * published matches, in line order) has on the row's earlier lines; a
 * log's earlier lines never change. */
export function locateCILogHit(sections: readonly CILogSection[], hits: readonly CILogFindHit[], hit: CILogFindHit): CILogFindLocation | null {
  const section = sections.find((each) => each.end > each.start && each.start <= hit.line && hit.line < each.end);
  if (!section) return null;
  const rowFrom = Math.max(section.start, Math.floor(hit.line / CI_LOG_BLOCK_LINES) * CI_LOG_BLOCK_LINES);
  let before = 0;
  for (const other of hits) {
    if (other.line >= hit.line) break;
    if (other.line >= rowFrom) before += 1;
  }
  return { sectionKey: section.key, rowFrom, rowKey: findCILogKey(rowFrom), occurrence: before + hit.occurrence };
}

/** The open job's log as the view shows it. */
export interface CILogFindShown {
  readonly jobId: string;
  readonly version: number;
  readonly sections: readonly CILogSection[];
}

/** The hits of an answer read for job `askedJob`, mapped onto the log now
 * shown; null when that is another job or another version of the log. */
export function ciLogAnswerHits(search: CILogSearch, askedJob: string, shown: CILogFindShown | null, limit: number): CILogFindHit[] | null {
  if (!shown || shown.jobId !== askedJob || shown.version !== search.version) return null;
  return ciLogFindHits(search, shown.sections, limit);
}
