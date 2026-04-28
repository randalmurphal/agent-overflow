import { describe, expect, it } from 'vitest';

import { displayModelLabel } from './modelLabels';

describe('displayModelLabel', () => {
  it('formats Claude slugs as provider-free display names', () => {
    expect(displayModelLabel('claude', 'claude-opus-4-7')).toBe('Opus 4.7');
    expect(displayModelLabel('claude', 'claude-sonnet-4-6')).toBe('Sonnet 4.6');
    expect(displayModelLabel('claude', 'claude-haiku-4-5')).toBe('Haiku 4.5');
  });

  it('removes Claude from registry names and old favorite labels', () => {
    expect(displayModelLabel('claude', 'claude-opus-4-7', 'Claude Opus 4.7')).toBe('Opus 4.7');
  });

  it('leaves non-Claude names alone', () => {
    expect(displayModelLabel('codex', 'gpt-5.4-mini', 'GPT-5.4 Mini')).toBe('GPT-5.4 Mini');
    expect(displayModelLabel('codex', 'gpt-5.4-mini')).toBe('gpt-5.4-mini');
  });
});
