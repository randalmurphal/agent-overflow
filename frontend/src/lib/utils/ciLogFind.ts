// The review pane's find over an open CI job log: the backend counts the
// matches per line (SearchPRCILog); this keeps those on the view's
// sections. Lines no section lists are not on the view and are not
// matches. An answer applies only to the job and log version it was read
// from.
//
// A match is its line and its index on that line. Whether it is present
// now, the section to open, the row that shows it (a row of lines is at
// most one block of one section, ReviewCILogView) and its index among that
// row's matches are derived against the sections and the line text shown
// at the moment of use (`resolveCILogHit`), by review find's matching rule
// (reviewFindScan.ts#findMatchChunk) over each line's text as AnsiText
// shows it (ansiText.ts#ansiShownText, the escapes the backend's search
// hides as well), never from other published matches.

import { CI_LOG_BLOCK_LINES } from '../stores/ciLogBlocks.svelte';
import type { CILogSearch, CILogSection } from '../types/models';
import { ansiShownText } from './ansiText';
import { countFindMatches } from './reviewFindScan';

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

/** The section that lists `line` now, and the first line of the row that
 * shows it; null when no section lists it. */
export function placeCILogLine(sections: readonly CILogSection[], line: number): { sectionKey: string; rowFrom: number } | null {
  const section = sections.find((each) => each.end > each.start && each.start <= line && line < each.end);
  if (!section) return null;
  return { sectionKey: section.key, rowFrom: Math.max(section.start, Math.floor(line / CI_LOG_BLOCK_LINES) * CI_LOG_BLOCK_LINES) };
}

/** Whether a log match is present now: where; not (no section lists its
 * line, or its line shows fewer matches); or its row's text is not loaded
 * (`unknown`, with where it would be), which cannot be judged yet. */
export type CILogResolution =
  | { readonly kind: 'present'; readonly at: CILogFindLocation }
  | { readonly kind: 'absent' }
  | { readonly kind: 'unknown'; readonly sectionKey: string };

/**
 * Whether `hit` is present for `needle` (folded, `findNeedle`) in the log
 * shown now: `sections`, and `lineText`, a line as its block holds it
 * (null when the block is not loaded), read as AnsiText shows it. Its index
 * in its row counts the matches of the row's earlier lines in that text.
 */
export function resolveCILogHit(sections: readonly CILogSection[], lineText: (line: number) => string | null, needle: string, hit: CILogFindHit): CILogResolution {
  const placed = placeCILogLine(sections, hit.line);
  if (!placed || needle === '') return { kind: 'absent' };
  const shownLine = (line: number): string | null => {
    const raw = lineText(line);
    return raw === null ? null : ansiShownText(raw);
  };
  const own = shownLine(hit.line);
  if (own === null) return { kind: 'unknown', sectionKey: placed.sectionKey };
  if (hit.occurrence >= countFindMatches(own, needle)) return { kind: 'absent' };
  let before = 0;
  for (let line = placed.rowFrom; line < hit.line; line += 1) {
    const text = shownLine(line);
    if (text === null) return { kind: 'unknown', sectionKey: placed.sectionKey };
    before += countFindMatches(text, needle);
  }
  return { kind: 'present', at: { sectionKey: placed.sectionKey, rowFrom: placed.rowFrom, rowKey: findCILogKey(placed.rowFrom), occurrence: before + hit.occurrence } };
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
