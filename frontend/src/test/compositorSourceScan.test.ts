import { describe, expect, it } from 'vitest';
import { findCompositorSourceFindings } from './compositorSourceScan';

function kinds(source: string): string[] {
  return findCompositorSourceFindings(source).map((finding) => finding.kind);
}

describe('compositor source scan', () => {
  it.each([
    ['Tailwind will-change', '<div class="will-change-transform">', 'will-change utility'],
    ['CSS will-change', '.row { will-change: transform; }', 'will-change declaration'],
    ['DOM willChange', "row.style.willChange = 'transform';", 'willChange property assignment'],
    ['DOM transform', "row.style.transform = 'translateY(1px)';", 'DOM transform property assignment'],
    ['transform setProperty', "row.style.setProperty('transform', value);", 'transform setProperty'],
    ['will-change setProperty', "row.style.setProperty('will-change', value);", 'will-change setProperty'],
    ['Svelte style directive', '<div style:translate={offset}>', 'Svelte transform style directive'],
    ['CSS transform', '.row { transform: translateY(1px); }', 'transform declaration or keyframe'],
    ['Tailwind transform', '<div class="translate-y-px">', 'Tailwind transform utility'],
    ['Web Animations keyframe', "el.animate([{ transform: 'translateY(1px)' }]);", 'transform declaration or keyframe'],
  ])('finds %s', (_label, source, expectedKind) => {
    expect(kinds(source)).toContain(expectedKind);
  });

  it('ignores comments and assignments that only clear inherited state', () => {
    const source = `
      // row.style.transform = 'translateY(1px)';
      /* .row { will-change: transform; } */
      <!-- <div class="will-change-transform translate-y-px"> -->
      row.style.transform = 'none';
      row.style.willChange = '';
      row.style.setProperty('translate', '');
    `;
    expect(findCompositorSourceFindings(source)).toEqual([]);
  });
});
