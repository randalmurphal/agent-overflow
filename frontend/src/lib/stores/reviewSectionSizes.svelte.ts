// Dragged heights of the PR header's collapsible sections (Description,
// Conversation). One remembered height per SECTION, shared by every
// review pane and persisted through appStorage — the preference is about
// how much of the pane the diff keeps, which is a property of the screen,
// not of one PR. Null means "never dragged": the section falls back to
// its default CSS cap and shrinks to content.
//
// Same live/persist split as sidebarLayout: the drag calls the live
// setter per frame and flushes once on pointer-up.

import { appStorageGet, appStorageSet } from './appStorage';

export type ReviewSectionId = 'description' | 'conversation';

export const REVIEW_SECTION_MIN_HEIGHT = 64;
/** Generous ceiling — the real bound is the pane, and shrinking the diff
 *  to nothing is the user's call to make. */
export const REVIEW_SECTION_MAX_HEIGHT = 2000;

const STORAGE_KEY = 'reviewSectionHeights';

function clampHeight(px: number): number {
  return Math.max(
    REVIEW_SECTION_MIN_HEIGHT,
    Math.min(REVIEW_SECTION_MAX_HEIGHT, Math.round(px)),
  );
}

/**
 * Exported for direct unit testing of the corrupt-stored-value fallback,
 * which otherwise only runs at module import.
 */
export function readPersistedSectionHeights(): Partial<Record<ReviewSectionId, number>> {
  const raw = appStorageGet(STORAGE_KEY);
  if (!raw) return {};
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    const out: Partial<Record<ReviewSectionId, number>> = {};
    for (const section of ['description', 'conversation'] as const) {
      const value = parsed[section];
      if (typeof value === 'number' && Number.isFinite(value)) {
        out[section] = clampHeight(value);
      }
    }
    return out;
  } catch {
    return {};
  }
}

const heights = $state<Partial<Record<ReviewSectionId, number>>>(readPersistedSectionHeights());

/** The dragged height in px, or null when the default cap applies. */
export function reviewSectionHeight(section: ReviewSectionId): number | null {
  return heights[section] ?? null;
}

/** Live update for the drag — in-memory only, safe at pointer rate. */
export function setReviewSectionHeightLive(section: ReviewSectionId, px: number): void {
  heights[section] = clampHeight(px);
}

/** Flush the current heights to appStorage. Idempotent. */
export function persistReviewSectionHeights(): void {
  appStorageSet(STORAGE_KEY, JSON.stringify(heights));
}
