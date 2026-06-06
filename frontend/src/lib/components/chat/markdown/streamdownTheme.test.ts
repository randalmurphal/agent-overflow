import { describe, expect, it } from 'vitest';
import { chatMarkdownTheme } from './streamdownTheme';

describe('chatMarkdownTheme', () => {
  it('keeps fenced code wrapable instead of horizontally scrollable', () => {
    const codePreBase = chatMarkdownTheme.code.pre;
    const preClasses = codePreBase.split(/\s+/);
    const skeletonBase = chatMarkdownTheme.code.skeleton;
    const skeletonClasses = skeletonBase.split(/\s+/);

    expect(preClasses).toContain('whitespace-pre-wrap');
    expect(preClasses).toContain('wrap-anywhere');
    expect(preClasses).toContain('overflow-x-visible');
    expect(codePreBase).not.toMatch(/\boverflow-x-auto\b/);
    expect(codePreBase).not.toMatch(/\bwhitespace-nowrap\b/);
    expect(skeletonClasses).toContain('max-w-full');
    expect(skeletonClasses).toContain('whitespace-pre-wrap');
    expect(skeletonClasses).toContain('wrap-anywhere');
    expect(skeletonBase).not.toMatch(/\bwhitespace-nowrap\b/);
  });

  it('keeps inline code wrapable instead of horizontally scrollable', () => {
    const codespanBase = chatMarkdownTheme.codespan.base;
    const classes = codespanBase.split(/\s+/);

    expect(classes).toContain('inline');
    expect(classes).toContain('whitespace-pre-wrap');
    expect(classes).toContain('wrap-anywhere');
    expect(codespanBase).not.toMatch(/\boverflow-x-auto\b/);
    expect(codespanBase).not.toMatch(/\bwhitespace-nowrap\b/);
    expect(codespanBase).not.toMatch(/\binline-block\b/);
    expect(codespanBase).not.toMatch(/\bmax-w-full\b/);
  });
});
