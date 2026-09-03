// Settings search: matching over the field index, and revealing a hit on
// its page.
//
// Matching is deliberately small. Every whitespace-separated token of the
// query must appear somewhere in a field's text (label, hint, keywords,
// in-page heading, page label, group label). Hits rank by where the first
// token landed — label, then heading, then everything else — so "font"
// lists the two font pickers ahead of a hint that merely mentions fonts.
// Pages match on their own label and description and rank with label hits,
// so "keybindings" finds the page even though it registers no fields.

import { SETTINGS_FIELDS, type SettingsFieldDef } from './fields';
import {
  SETTINGS_SECTIONS,
  settingsSectionDef,
  type SettingsSection,
  type SettingsSectionDef,
} from './sections';

export type SettingsSearchHit =
  | { kind: 'field'; field: SettingsFieldDef; page: SettingsSectionDef }
  | { kind: 'page'; page: SettingsSectionDef };

/** The stable key a result list renders by. */
export function searchHitKey(hit: SettingsSearchHit): string {
  return hit.kind === 'field' ? hit.field.id : `page:${hit.page.id}`;
}

export function searchHitSection(hit: SettingsSearchHit): SettingsSection {
  return hit.page.id;
}

interface IndexedField {
  field: SettingsFieldDef;
  page: SettingsSectionDef;
  label: string;
  heading: string;
  rest: string;
}

// Lowercased once at module init: the index is static and a keystroke
// should cost a scan, not a re-lowercase of every hint.
const FIELD_INDEX: readonly IndexedField[] = SETTINGS_FIELDS.map((field) => {
  const page = settingsSectionDef(field.section);
  return {
    field,
    page,
    label: field.label.toLowerCase(),
    heading: (field.heading ?? '').toLowerCase(),
    rest: [field.hint ?? '', ...(field.keywords ?? []), page.label, page.group]
      .join(' ')
      .toLowerCase(),
  };
});

const PAGE_INDEX = SETTINGS_SECTIONS.map((page) => ({
  page,
  label: page.label.toLowerCase(),
  rest: `${page.description} ${page.group}`.toLowerCase(),
}));

const RANK_LABEL = 0;
const RANK_HEADING = 1;
const RANK_REST = 2;

function rankOf(tokens: string[], label: string, heading: string, rest: string): number | null {
  let rank = RANK_LABEL;
  for (const token of tokens) {
    if (label.includes(token)) continue;
    if (heading.includes(token)) {
      rank = Math.max(rank, RANK_HEADING);
      continue;
    }
    if (rest.includes(token)) {
      rank = Math.max(rank, RANK_REST);
      continue;
    }
    return null;
  }
  return rank;
}

export function searchSettings(query: string): SettingsSearchHit[] {
  const tokens = query.toLowerCase().split(/\s+/).filter((t) => t.length > 0);
  if (tokens.length === 0) return [];

  const ranked: Array<{ rank: number; order: number; hit: SettingsSearchHit }> = [];
  for (const [order, entry] of PAGE_INDEX.entries()) {
    const rank = rankOf(tokens, entry.label, '', entry.rest);
    if (rank !== null) ranked.push({ rank, order, hit: { kind: 'page', page: entry.page } });
  }
  for (const [order, entry] of FIELD_INDEX.entries()) {
    const rank = rankOf(tokens, entry.label, entry.heading, entry.rest);
    if (rank === null) continue;
    ranked.push({
      rank,
      order: PAGE_INDEX.length + order,
      hit: { kind: 'field', field: entry.field, page: entry.page },
    });
  }
  // Stable within a rank: index order is page order, so equal-ranked hits
  // read top-to-bottom the way the pages do.
  ranked.sort((a, b) => a.rank - b.rank || a.order - b.order);
  return ranked.map((r) => r.hit);
}

const FLASH_CLASS = 'settings-field-flash';

/**
 * Scrolls a rendered field into view and flashes it. `root` is the page
 * panel, so a stale anchor from a page that has since unmounted can never
 * match. A field the page is not currently rendering (a `conditional`
 * entry) is silently a no-op: the page itself was already opened, which is
 * as close as the UI can get.
 */
export function revealSettingsField(root: ParentNode, fieldId: string): boolean {
  const el = root.querySelector<HTMLElement>(`[data-settings-field="${fieldId}"]`);
  if (!el) return false;
  el.scrollIntoView({ block: 'center' });
  // Re-triggering on an element already mid-flash restarts the animation:
  // removing the class, forcing a style flush, then re-adding it.
  el.classList.remove(FLASH_CLASS);
  void el.offsetWidth;
  el.classList.add(FLASH_CLASS);
  el.addEventListener('animationend', () => el.classList.remove(FLASH_CLASS), { once: true });
  return true;
}
