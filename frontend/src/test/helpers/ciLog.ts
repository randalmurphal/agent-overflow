// A followed CI log as the backend's cache serves it: the index a
// pr:ci_log frame carries and GetPRCILogLines over its lines.

import type { CILogFetch } from '../../lib/stores/ciLogBlocks.svelte';
import type { PRCILogState } from '../../lib/stores/prReviewCI.svelte';
import type { CILogLines, CILogSection } from '../../lib/types/models';

export function numberedLines(count: number, first = 1, prefix = 'line'): string[] {
  return Array.from({ length: count }, (_, index) => `${prefix} ${first + index}`);
}

/** A section of lines [start, end). */
export function section(key: string, name: string, status: string, start: number, end: number, extra: Partial<CILogSection> = {}): CILogSection {
  return { key, name, status, start, end, lines: end - start, bytes: (end - start) * 10, ...extra };
}

export class FakeCILog {
  lines: string[];
  sections: CILogSection[];
  version = 1;
  stableLines = 0;
  available = true;
  downloading = false;
  /** Every read, as [from, count]. */
  readonly reads: [number, number][] = [];
  /** When set, reads wait for it. */
  gate: Promise<void> | null = null;
  /** When set, reads fail with it. */
  failure: Error | null = null;

  /** A log whose rows default to the one job row over all its lines. */
  constructor(lines: string[], sections?: CILogSection[]) {
    this.lines = lines;
    this.sections = sections ?? [section('job', 'unit', 'running', 0, lines.length)];
  }

  get state(): PRCILogState {
    return {
      version: this.version,
      totalLines: this.lines.length,
      totalBytes: this.lines.reduce((sum, line) => sum + line.length + 1, 0),
      stableLines: this.stableLines,
      sections: this.sections,
      available: this.available,
      downloading: this.downloading,
      error: null,
      failure: null,
      seq: this.version,
    };
  }

  readonly fetch: CILogFetch = async (from, count) => {
    this.reads.push([from, count]);
    if (this.gate) await this.gate;
    if (this.failure) throw this.failure;
    const reply: CILogLines = {
      from,
      lines: this.lines.slice(from, from + count),
      totalLines: this.lines.length,
      version: this.version,
    };
    return reply;
  };

  /** The next version: `more` lines appended, every earlier line kept.
   * Rows default to the one job row over the grown log. */
  grow(more: string[], sections?: CILogSection[]): void {
    this.stableLines = this.lines.length;
    this.lines = [...this.lines, ...more];
    this.version += 1;
    this.sections = sections ?? [section('job', 'unit', 'running', 0, this.lines.length)];
  }
}
