import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';

import ProviderIcon from './ProviderIcon.svelte';

afterEach(() => cleanup());

describe('<ProviderIcon>', () => {
  it('renders the orange Claude glyph for claude with no terminal chip', () => {
    const { container } = render(ProviderIcon, { props: { provider: 'claude' } });
    const glyph = container.querySelector('.lucide-claude');
    expect(glyph).not.toBeNull();
    // Split into class tokens, same as the negative assertion below: on a
    // whole class string `toContain` is a substring match, so this would pass
    // on the claude-tui tint — the one wrong answer it exists to rule out.
    expect((glyph?.getAttribute('class') ?? '').split(/\s+/)).toContain('text-provider-claude');
    // No green terminal chip wrapper for plain claude: the glyph (itself a
    // mask span since the icon conversion) sits directly in the container.
    expect(glyph?.parentElement).toBe(container);
  });

  it('renders the Claude glyph in terminal green for claude-tui, no chip', () => {
    // The claude-tui mark is the Claude glyph tinted terminal green — the
    // user-facing signal that a thread drives the real TUI. No square chip: on
    // the black terminal surface a boxed/outlined glyph reads as a muddy hollow
    // shape at icon sizes, so the solid green starburst is used instead.
    const { container } = render(ProviderIcon, { props: { provider: 'claude-tui' } });
    const glyph = container.querySelector('.lucide-claude');
    expect(glyph).not.toBeNull();
    const glyphClasses = (glyph?.getAttribute('class') ?? '').split(/\s+/);
    expect(glyphClasses).toContain('text-provider-claude-tui');
    // No square chip wrapper, and not claude's coral tint. Split into class
    // tokens rather than substring-matched: `text-provider-claude` is a
    // PREFIX of `text-provider-claude-tui`, so `toContain` on the whole class
    // string would pass on the very class this asserts against.
    expect(glyph?.parentElement).toBe(container);
    expect(glyphClasses).not.toContain('text-provider-claude');
  });

  it('renders the OpenAI mark (not the Claude glyph) for codex', () => {
    const { container } = render(ProviderIcon, { props: { provider: 'codex' } });
    expect(container.querySelector('.lucide-claude')).toBeNull();
    expect(container.querySelector('.lucide-openai')).not.toBeNull();
  });
});
