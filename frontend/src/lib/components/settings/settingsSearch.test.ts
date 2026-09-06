import { describe, expect, it } from 'vitest';
import { searchHitKey, searchSettings, revealSettingsField } from './settingsSearch';
import { SETTINGS_FIELDS } from './fields';

function keys(query: string): string[] {
  return searchSettings(query).map(searchHitKey);
}

describe('searchSettings', () => {
  it('distinguishes provider accounts on the shared Accounts page', () => {
    const accounts = searchSettings('accounts').filter((hit) => hit.kind === 'field' && hit.page.id === 'accounts');
    expect(accounts).toHaveLength(2);
    expect(new Set(accounts.map((hit) => hit.kind === 'field' && hit.field.heading)).size).toBe(2);
    expect(keys('codex accounts')).toContain('codex.accounts');
  });

  it('returns nothing for an empty or whitespace query', () => {
    expect(searchSettings('')).toEqual([]);
    expect(searchSettings('   ')).toEqual([]);
  });

  it('ranks label hits ahead of hint and keyword hits', () => {
    const hits = keys('diff');
    // The two rows that say "diff" in their label lead; the code font, whose
    // hint mentions diffs, follows them.
    expect(hits.slice(0, 2)).toEqual(['chat.diff-word-wrap', 'chat.collapse-diff-previews']);
    expect(hits).toContain('typography.code-font');
  });

  it('requires every token to match somewhere, in any order', () => {
    expect(keys('code font')).toContain('typography.code-font');
    expect(keys('font code')).toContain('typography.code-font');
    expect(keys('code font zzz')).toEqual([]);
  });

  it('finds a page by its label even when it registers no fields', () => {
    expect(SETTINGS_FIELDS.some((f) => f.section === 'keybindings')).toBe(false);
    expect(keys('keybind')[0]).toBe('page:keybindings');
  });

  it('matches on keywords the rendered copy never says', () => {
    expect(keys('sleep')).toContain('performance.keep-awake-screen');
    expect(keys('dark')).toContain('theme.mode');
  });

  it('matches on the in-page heading and the page label', () => {
    expect(keys('safety')).toEqual(
      expect.arrayContaining(['threads.confirm-archive', 'threads.confirm-delete']),
    );
    expect(keys('codex binary')).toContain('codex.binary-path');
  });

  it('is case-insensitive', () => {
    expect(keys('LOW POWER')).toContain('performance.low-power-mode');
  });

  it('keeps page order within a rank so equal hits read like the pages', () => {
    const hits = searchSettings('claude');
    const claudeFields = hits.filter(
      (h) => h.kind === 'field' && h.page.id === 'claude' && h.field.label === 'Enabled',
    );
    expect(claudeFields).toHaveLength(1);
    const ids = hits.map(searchHitKey);
    // Setup fields precede Session fields, as they do on the page.
    expect(ids.indexOf('claude.binary-path')).toBeLessThan(ids.indexOf('claude.thinking'));
  });
});

describe('revealSettingsField', () => {
  it('scrolls to the anchor, flashes it, and clears the flash on animationend', () => {
    const root = document.createElement('div');
    const el = document.createElement('div');
    el.dataset.settingsField = 'theme.mode';
    el.scrollIntoView = () => {};
    root.appendChild(el);

    expect(revealSettingsField(root, 'theme.mode')).toBe(true);
    expect(el.classList.contains('settings-field-flash')).toBe(true);
    el.dispatchEvent(new Event('animationend'));
    expect(el.classList.contains('settings-field-flash')).toBe(false);
  });

  it('reports a field the page is not rendering instead of throwing', () => {
    const root = document.createElement('div');
    expect(revealSettingsField(root, 'theme.mode')).toBe(false);
  });
});
