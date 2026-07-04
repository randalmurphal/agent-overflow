import { describe, expect, it } from 'vitest';

import { displayModelLabel, displayUsageModelLabel } from './modelLabels';

describe('displayModelLabel', () => {
  it('formats Claude slugs as provider-free display names', () => {
    expect(displayModelLabel('claude', 'claude-opus-4-7')).toBe('Opus 4.7');
    expect(displayModelLabel('claude', 'claude-sonnet-5')).toBe('Sonnet 5');
    expect(displayModelLabel('claude', 'claude-sonnet-4-6')).toBe('Sonnet 4.6');
    expect(displayModelLabel('claude', 'claude-haiku-4-5')).toBe('Haiku 4.5');
  });

  it('formats claude-tui slugs with the Claude formatter (same models, friendly label)', () => {
    // The interactive provider runs the same Claude models, so the trigger
    // shows "Opus 4.8", not the raw slug "claude-opus-4-8".
    expect(displayModelLabel('claude-tui', 'claude-opus-4-8')).toBe('Opus 4.8');
    expect(displayModelLabel('claude-tui', 'claude-sonnet-4-6')).toBe('Sonnet 4.6');
    expect(displayModelLabel('claude-tui', 'claude-opus-4-8', 'Claude Opus 4.8')).toBe('Opus 4.8');
  });

  it('strips trailing 8-digit release datestamps from Claude slugs', () => {
    // Canonical wire ids carry a release stamp the picker should not
    // display — surface "Haiku 4.5", not "Haiku 4.5 20251001".
    expect(displayModelLabel('claude', 'claude-haiku-4-5-20251001')).toBe('Haiku 4.5');
  });

  it('handles bare tier aliases the SDK forwards verbatim', () => {
    // run_in_background tasks accept short aliases like "opus"; the
    // resolver downstream picks a concrete model id, but the
    // immediate launch row carries the alias as-is.
    expect(displayModelLabel('claude', 'opus')).toBe('Opus');
    expect(displayModelLabel('claude', 'sonnet')).toBe('Sonnet');
    expect(displayModelLabel('claude', 'haiku')).toBe('Haiku');
  });

  it('removes Claude from registry names and old favorite labels', () => {
    expect(displayModelLabel('claude', 'claude-opus-4-7', 'Claude Opus 4.7')).toBe('Opus 4.7');
  });

  it('leaves non-Claude names alone', () => {
    expect(displayModelLabel('codex', 'gpt-5.4-mini', 'GPT-5.4 Mini')).toBe('GPT-5.4 Mini');
    expect(displayModelLabel('codex', 'gpt-5.4-mini')).toBe('gpt-5.4-mini');
  });
});

describe('displayUsageModelLabel', () => {
  it('formats Claude ledger slugs like the model picker, keeping the [1m] tier marker', () => {
    // These are the exact slug shapes the Claude wire stamps into
    // usage_ledger rows (see result.modelUsage keys). The [1m] marker
    // MUST survive: the ledger keeps the extended-context tier as its
    // own model, and without the suffix "claude-sonnet-5" and
    // "claude-sonnet-5[1m]" render as two identical "Sonnet 5" rows.
    expect(displayUsageModelLabel('claude-fable-5')).toBe('Fable 5');
    expect(displayUsageModelLabel('claude-sonnet-5')).toBe('Sonnet 5');
    expect(displayUsageModelLabel('claude-sonnet-5[1m]')).toBe('Sonnet 5 [1m]');
    expect(displayUsageModelLabel('claude-opus-4-8[1m]')).toBe('Opus 4.8 [1m]');
    expect(displayUsageModelLabel('claude-haiku-4-5-20251001')).toBe('Haiku 4.5');
  });

  it('title-cases GPT slugs to match the Codex catalog names', () => {
    expect(displayUsageModelLabel('gpt-5.2-codex')).toBe('GPT-5.2 Codex');
    expect(displayUsageModelLabel('gpt-5.5')).toBe('GPT-5.5');
    expect(displayUsageModelLabel('gpt-5.4-mini')).toBe('GPT-5.4 Mini');
    expect(displayUsageModelLabel('gpt-5.3-codex-spark')).toBe('GPT-5.3 Codex Spark');
  });

  it('passes unrecognized slugs through untouched', () => {
    expect(displayUsageModelLabel('o4-mini')).toBe('o4-mini');
    expect(displayUsageModelLabel('some-custom-model')).toBe('some-custom-model');
  });
});
