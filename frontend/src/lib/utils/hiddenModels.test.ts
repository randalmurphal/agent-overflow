import { describe, expect, it } from 'vitest';

import { makeSettings } from '../../test/helpers/settings';
import { hiddenModelSlugs, isModelHidden, offeredModels, pickerVisibleModels } from './hiddenModels';
import { catalogWithdrawals } from './catalogWithdrawals';

describe('hiddenModels', () => {
  const settings = makeSettings({
    claudeHiddenModels: ['claude-opus-4-5'],
    codexHiddenModels: ['gpt-5.2'],
  });

  it('claude and claude-tui share the claude hide-list', () => {
    expect(isModelHidden(settings, 'claude', 'claude-opus-4-5')).toBe(true);
    expect(isModelHidden(settings, 'claude-tui', 'claude-opus-4-5')).toBe(true);
    expect(isModelHidden(settings, 'claude', 'claude-opus-4-8')).toBe(false);
  });

  it('codex uses its own list', () => {
    expect(isModelHidden(settings, 'codex', 'gpt-5.2')).toBe(true);
    expect(isModelHidden(settings, 'codex', 'claude-opus-4-5')).toBe(false);
  });

  it('unknown providers hide nothing', () => {
    expect(hiddenModelSlugs(settings, 'gemini').size).toBe(0);
  });

  it('treats absent lists as empty (Go persists with omitempty)', () => {
    const sparse = makeSettings();
    delete sparse.claudeHiddenModels;
    expect(isModelHidden(sparse, 'claude', 'claude-opus-4-5')).toBe(false);
  });

  describe('unlisted models', () => {
    const catalog = [
      { slug: 'claude-opus-5-5', unlisted: false },
      { slug: 'claude-opus-4-5', unlisted: true },
      { slug: 'claude-sonnet-5' },
    ];

    it('pickers drop rows the provider no longer lists', () => {
      expect(pickerVisibleModels(makeSettings(), 'claude', catalog).map((m) => m.slug))
        .toEqual(['claude-opus-5-5', 'claude-sonnet-5']);
      expect(offeredModels(catalog).map((m) => m.slug)).toEqual(['claude-opus-5-5', 'claude-sonnet-5']);
    });

    it('a thread already on an unlisted model keeps its row', () => {
      expect(pickerVisibleModels(makeSettings(), 'claude', catalog, 'claude-opus-4-5').map((m) => m.slug))
        .toEqual(['claude-opus-5-5', 'claude-opus-4-5', 'claude-sonnet-5']);
    });

    it('hiding every listed model falls back to the listed ones, never unlisted', () => {
      const all = makeSettings({ claudeHiddenModels: ['claude-opus-5-5', 'claude-sonnet-5'] });
      expect(pickerVisibleModels(all, 'claude', catalog).map((m) => m.slug))
        .toEqual(['claude-opus-5-5', 'claude-sonnet-5']);
    });

    it('a selection on a model the provider stopped listing counts as withdrawn', () => {
      const models = [
        { slug: 'claude-opus-4-5', name: 'Opus 4.5', provider: 'claude', unlisted: true, providerUnlisted: true },
      ];
      expect(catalogWithdrawals({ provider: 'claude', model: 'claude-opus-4-5[1m]' }, models)).toEqual(['claude-opus-4-5[1m]']);
      expect(catalogWithdrawals({ provider: 'claude', model: 'claude-opus-4-5' }, [{ slug: 'claude-opus-4-5', name: 'Opus 4.5', provider: 'claude' }])).toEqual([]);
    });

    it('pickers keep a model a thread ran this week, and its withdrawal by the provider still warns', () => {
      // The backend clears unlisted for a model kept by recent use, and
      // says the provider does not list it in providerUnlisted.
      const kept = { slug: 'claude-opus-4-7', name: 'Opus 4.7', provider: 'claude', providerUnlisted: true };
      const recent: { slug: string; unlisted?: boolean; providerUnlisted?: boolean }[] = [
        { slug: 'claude-opus-5-5' },
        kept,
        { slug: 'claude-opus-4-5', unlisted: true, providerUnlisted: true },
      ];
      expect(pickerVisibleModels(makeSettings(), 'claude', recent).map((m) => m.slug))
        .toEqual(['claude-opus-5-5', 'claude-opus-4-7']);
      expect(offeredModels(recent).map((m) => m.slug)).toEqual(['claude-opus-5-5', 'claude-opus-4-7']);
      expect(catalogWithdrawals({ provider: 'claude', model: 'claude-opus-4-7' }, [kept])).toEqual(['claude-opus-4-7']);
    });

    it('an older computer that states only unlisted still has its hidden rows read as withdrawn', () => {
      // A computer that predates providerUnlisted marks unlisted alone, and
      // clears it for a model kept by recent use.
      const older = [
        { slug: 'claude-opus-4-7', name: 'Opus 4.7', provider: 'claude' },
        { slug: 'claude-opus-4-5', name: 'Opus 4.5', provider: 'claude', unlisted: true },
      ];
      expect(offeredModels(older).map((m) => m.slug)).toEqual(['claude-opus-4-7']);
      expect(catalogWithdrawals({ provider: 'claude', model: 'claude-opus-4-5' }, older)).toEqual(['claude-opus-4-5']);
      expect(catalogWithdrawals({ provider: 'claude', model: 'claude-opus-4-7' }, older)).toEqual([]);
    });
  });
});
