import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';

import ProviderIcon from './ProviderIcon.svelte';

afterEach(() => cleanup());

describe('<ProviderIcon>', () => {
  it('renders the orange Claude glyph for claude with no terminal chip', () => {
    const { container } = render(ProviderIcon, { props: { provider: 'claude' } });
    const glyph = container.querySelector('svg.lucide-claude');
    expect(glyph).not.toBeNull();
    expect(glyph?.getAttribute('class')).toContain('text-[#d97757]');
    // No green terminal chip wrapper for plain claude.
    expect(container.querySelector('span')).toBeNull();
  });

  it('renders the Claude glyph in terminal green for claude-tui, no chip', () => {
    // The claude-tui mark is the Claude glyph tinted terminal green — the
    // user-facing signal that a thread drives the real TUI. No square chip: on
    // the black terminal surface a boxed/outlined glyph reads as a muddy hollow
    // shape at icon sizes, so the solid green starburst is used instead.
    const { container } = render(ProviderIcon, { props: { provider: 'claude-tui' } });
    const glyph = container.querySelector('svg.lucide-claude');
    expect(glyph).not.toBeNull();
    const glyphClass = glyph?.getAttribute('class') ?? '';
    expect(glyphClass).toContain('text-provider-claude-tui');
    // No square chip wrapper, and not claude's orange tint.
    expect(container.querySelector('span')).toBeNull();
    expect(glyphClass).not.toContain('text-[#d97757]');
  });

  it('renders the OpenAI mark (not the Claude glyph) for codex', () => {
    const { container } = render(ProviderIcon, { props: { provider: 'codex' } });
    expect(container.querySelector('svg.lucide-claude')).toBeNull();
    expect(container.querySelector('svg')).not.toBeNull();
  });
});
