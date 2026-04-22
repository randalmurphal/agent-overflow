// Pure string transforms used by ProposedPlanCard. Each path is load-
// bearing for what the user sees when a plan renders, so the
// title-stripping and filename sanitization both deserve a test.

import { describe, expect, it } from 'vitest';
import {
  proposedPlanTitle,
  stripDisplayedPlanMarkdown,
  buildProposedPlanMarkdownFilename,
  normalizePlanMarkdownForExport,
} from './proposedPlan';

describe('proposedPlanTitle', () => {
  it('returns the first heading as the title', () => {
    expect(proposedPlanTitle('# My plan\nbody')).toBe('My plan');
  });

  it('accepts H1–H6', () => {
    expect(proposedPlanTitle('### Nested heading')).toBe('Nested heading');
  });

  it('trims leading whitespace up to 3 spaces before the heading marker', () => {
    expect(proposedPlanTitle('   # Indented')).toBe('Indented');
  });

  it('returns null when no heading is found', () => {
    expect(proposedPlanTitle('just body text')).toBeNull();
  });

  it('returns null for empty input', () => {
    expect(proposedPlanTitle('')).toBeNull();
  });

  it('treats a heading deeper in the document the same as the first line', () => {
    // The regex uses the /m flag so any line that starts with a heading
    // marker qualifies — falls out of the match at the first one.
    expect(proposedPlanTitle('body\n# Title later\nmore body')).toBe('Title later');
  });
});

describe('stripDisplayedPlanMarkdown', () => {
  it('removes the leading heading line', () => {
    expect(stripDisplayedPlanMarkdown('# Title\n\nbody')).toBe('body');
  });

  it('does not strip a heading that is not the first non-empty line', () => {
    // If the first line is plain text, no heading strip happens.
    expect(stripDisplayedPlanMarkdown('body text\n# Still here')).toBe('body text\n# Still here');
  });

  it('additionally strips a "Summary" H1 that follows the title heading', () => {
    // The classic ProposedPlan markdown is:
    //   # <title>
    //   # Summary
    //   <summary body>
    // Stripping "Summary" collapses the two redundant headings.
    expect(
      stripDisplayedPlanMarkdown('# Title\n\n## Summary\n\nthe plan'),
    ).toBe('the plan');
  });

  it('handles trailing whitespace by trimming the end', () => {
    expect(stripDisplayedPlanMarkdown('# Title\nbody   \n\n')).toBe('body');
  });

  it('returns an empty string if all content is stripped', () => {
    expect(stripDisplayedPlanMarkdown('# Title')).toBe('');
  });
});

describe('buildProposedPlanMarkdownFilename', () => {
  it('slugifies the title into a lowercase kebab-case .md name', () => {
    expect(buildProposedPlanMarkdownFilename('# Fix the login flow')).toBe('fix-the-login-flow.md');
  });

  it('drops punctuation and parentheses', () => {
    expect(buildProposedPlanMarkdownFilename("# Don't touch (yet)!"))
      .toBe('dont-touch-yet.md');
  });

  it('falls back to "plan.md" when the title has no usable characters', () => {
    expect(buildProposedPlanMarkdownFilename('# !!!')).toBe('plan.md');
  });

  it('falls back to "plan.md" when no title is present', () => {
    expect(buildProposedPlanMarkdownFilename('no heading here')).toBe('plan.md');
  });

  it('collapses runs of spaces into single hyphens', () => {
    expect(buildProposedPlanMarkdownFilename('# Make    it   so')).toBe('make-it-so.md');
  });

  it('strips leading and trailing hyphens after sanitization', () => {
    expect(buildProposedPlanMarkdownFilename('# ---wrap---')).toBe('wrap.md');
  });
});

describe('normalizePlanMarkdownForExport', () => {
  it('ensures exactly one trailing newline', () => {
    expect(normalizePlanMarkdownForExport('body')).toBe('body\n');
    expect(normalizePlanMarkdownForExport('body\n')).toBe('body\n');
    expect(normalizePlanMarkdownForExport('body\n\n\n')).toBe('body\n');
  });

  it('strips trailing whitespace on the last line', () => {
    expect(normalizePlanMarkdownForExport('body   ')).toBe('body\n');
  });

  it('preserves interior blank lines', () => {
    expect(normalizePlanMarkdownForExport('a\n\nb')).toBe('a\n\nb\n');
  });
});
