import { describe, expect, it } from 'vitest';

import { displayModelLabel } from './modelLabels';

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
