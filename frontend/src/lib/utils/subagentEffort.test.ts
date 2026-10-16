import { describe, expect, it } from 'vitest';
import type { ModelInfo } from '../types/settings';
import { effortLabel } from './effortLabels';
import { findClaudeCatalogModel, modelEffortAffix, subagentEffortLabel } from './subagentEffort';

const opus: ModelInfo = {
  slug: 'claude-opus-4-7',
  name: 'Claude Opus 4.7',
  provider: 'claude',
  reasoningEfforts: [
    { slug: 'low', label: 'Low' },
    { slug: 'medium', label: 'Medium' },
    { slug: 'high', label: 'High' },
    { slug: 'xhigh', label: 'Extra high' },
    { slug: 'max', label: 'Max' },
  ],
} as ModelInfo;
const sonnet: ModelInfo = {
  slug: 'claude-sonnet-4-6',
  name: 'Claude Sonnet 4.6',
  provider: 'claude',
  reasoningEfforts: [
    { slug: 'low', label: 'Low' },
    { slug: 'medium', label: 'Medium' },
    { slug: 'high', label: 'High' },
  ],
} as ModelInfo;
const haiku: ModelInfo = {
  slug: 'claude-haiku-4-5',
  name: 'Claude Haiku 4.5',
  provider: 'claude',
  reasoningEfforts: [],
} as ModelInfo;
const catalog = [opus, sonnet, haiku];

describe('effortLabel', () => {
  it('labels a tier the way the effort picker does', () => {
    expect(effortLabel('xhigh')).toBe('xHigh');
    expect(effortLabel('xhigh', 'Extra high')).toBe('xHigh');
    expect(effortLabel('max')).toBe('Max');
    expect(effortLabel('high', 'High')).toBe('High');
    expect(effortLabel('minimal')).toBe('Minimal');
    expect(effortLabel('')).toBe('');
  });
});

describe('findClaudeCatalogModel', () => {
  it('matches the id the CLI reports, with or without a datestamp or context marker', () => {
    expect(findClaudeCatalogModel(catalog, 'claude-opus-4-7')).toBe(opus);
    expect(findClaudeCatalogModel(catalog, 'claude-opus-4-7[1m]')).toBe(opus);
    expect(findClaudeCatalogModel(catalog, 'claude-haiku-4-5-20251001')).toBe(haiku);
  });

  it('knows nothing of an alias or an unlisted model', () => {
    expect(findClaudeCatalogModel(catalog, 'haiku')).toBeUndefined();
    expect(findClaudeCatalogModel(catalog, 'claude-opus-9')).toBeUndefined();
    expect(findClaudeCatalogModel(catalog, '')).toBeUndefined();
  });
});

describe('subagentEffortLabel', () => {
  it('shows the effort a Claude model offers with the picker label', () => {
    expect(subagentEffortLabel('claude', 'xhigh', opus)).toBe('xHigh');
    expect(subagentEffortLabel('claude', 'high', sonnet)).toBe('High');
  });

  it('shows no effort for a model without effort tiers, which the CLI runs without one', () => {
    expect(subagentEffortLabel('claude', 'xhigh', haiku)).toBe('');
  });

  it('steps an xhigh or max the model does not offer down to high, as the CLI does', () => {
    expect(subagentEffortLabel('claude', 'xhigh', sonnet)).toBe('High');
    expect(subagentEffortLabel('claude', 'max', sonnet)).toBe('High');
  });

  it('keeps the effort of a model the catalog does not list', () => {
    expect(subagentEffortLabel('claude', 'max', undefined)).toBe('Max');
  });

  it('shows the effort Codex reported as is', () => {
    expect(subagentEffortLabel('codex', 'xhigh', haiku)).toBe('xHigh');
    expect(subagentEffortLabel('codex', 'ultra', undefined)).toBe('Ultra');
  });

  it('shows nothing when no effort is known', () => {
    expect(subagentEffortLabel('claude', '', opus)).toBe('');
    expect(subagentEffortLabel('codex', '  ', undefined)).toBe('');
  });
});

describe('modelEffortAffix', () => {
  it('joins the model and effort labels, either alone', () => {
    expect(modelEffortAffix('Opus 4.7', 'xHigh')).toBe('Opus 4.7 xHigh');
    expect(modelEffortAffix('Opus 4.7', '')).toBe('Opus 4.7');
    expect(modelEffortAffix('', 'High')).toBe('High');
  });
});
