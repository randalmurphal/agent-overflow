import { describe, expect, it } from 'vitest';

import { makeSettings } from '../../test/helpers/settings';
import { hiddenModelSlugs, isModelHidden } from './hiddenModels';

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
});
