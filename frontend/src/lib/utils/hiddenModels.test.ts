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

    it('a selection on a model that became unlisted counts as withdrawn', () => {
      const models = [
        { slug: 'claude-opus-4-5', name: 'Opus 4.5', provider: 'claude', unlisted: true },
      ];
      expect(catalogWithdrawals({ provider: 'claude', model: 'claude-opus-4-5[1m]' }, models)).toEqual(['claude-opus-4-5[1m]']);
      expect(catalogWithdrawals({ provider: 'claude', model: 'claude-opus-4-5' }, [{ ...models[0], unlisted: false }])).toEqual([]);
    });
  });
});
