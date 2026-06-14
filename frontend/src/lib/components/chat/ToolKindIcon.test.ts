// Verifies that ToolKindIcon emits the right per-kind color class for
// every member of the ToolKindIcon enum. The class binding is the only
// behavior that matters at this layer; a typo in any branch would
// silently fall through to an unstyled icon because Tailwind v4 drops
// classes it doesn't see in source.
//
// SVG elements expose `.className` as `SVGAnimatedString` (not a plain
// string), so we read `getAttribute('class')` for happy-dom safety.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ToolKindIcon from './ToolKindIcon.svelte';
import type { ToolKindIcon as Kind } from './toolCardHeader';

const CASES: Array<{ kind: Kind; colorClass: string }> = [
  { kind: 'terminal', colorClass: 'text-ico-terminal' },
  { kind: 'file', colorClass: 'text-ico-file' },
  { kind: 'eye', colorClass: 'text-ico-eye' },
  { kind: 'search', colorClass: 'text-ico-search' },
  { kind: 'globe', colorClass: 'text-ico-globe' },
  { kind: 'robot', colorClass: 'text-ico-robot' },
  { kind: 'speech-bubble', colorClass: 'text-ico-speech-bubble' },
  { kind: 'checklist', colorClass: 'text-ico-checklist' },
  { kind: 'puzzle', colorClass: 'text-ico-puzzle' },
  { kind: 'clock', colorClass: 'text-ico-clock' },
  { kind: 'brain', colorClass: 'text-ico-brain' },
  { kind: 'compaction', colorClass: 'text-ico-compaction' },
  { kind: 'generic', colorClass: 'text-ico-generic' },
];

describe('<ToolKindIcon>', () => {
  for (const { kind, colorClass } of CASES) {
    it(`renders the ${kind} icon with ${colorClass}`, () => {
      const { container } = render(ToolKindIcon, { props: { kind } });
      const svg = container.querySelector('svg')!;
      expect(svg).not.toBeNull();
      expect(svg.getAttribute('data-icon')).toBe(kind);
      const className = svg.getAttribute('class') ?? '';
      expect(className).toContain(colorClass);
      // Sanity: the size + shrink classes survive the color rewrite.
      expect(className).toContain('h-3.5');
      expect(className).toContain('w-3.5');
      expect(className).toContain('shrink-0');
    });
  }

  it('uses the kind in the default aria-label and accepts an override', () => {
    const { container, rerender } = render(ToolKindIcon, {
      props: { kind: 'terminal' },
    });
    let svg = container.querySelector('svg')!;
    expect(svg.getAttribute('aria-label')).toBe('terminal tool');

    rerender({ kind: 'terminal', ariaLabel: 'Run bash command' });
    svg = container.querySelector('svg')!;
    expect(svg.getAttribute('aria-label')).toBe('Run bash command');
  });
});
