import { describe, expect, it } from 'vitest';
import {
  CLAUDE_OUTPUT_STYLE_OPTIONS,
  CLAUDE_SUBAGENT_LIMIT_MAX,
  CLAUDE_THINKING_BUDGET_DEFAULT,
  CLAUDE_THINKING_BUDGET_MAX,
  CLAUDE_THINKING_BUDGET_MIN,
  CLAUDE_THINKING_DISPLAY_OPTIONS,
  CLAUDE_THINKING_MODE_OPTIONS,
  clampSubagentLimit,
  clampThinkingBudget,
  thinkingChangeDefersRestart,
  thinkingPatch,
  subagentLimitsAreEmpty,
  toolMemoryLimitError,
} from './claudeSessionAxes';

describe('claude session axis options', () => {
  it('offers the CLI default first, and does not call it "off"', () => {
    expect(CLAUDE_OUTPUT_STYLE_OPTIONS[0].value).toBe('');
    expect(CLAUDE_OUTPUT_STYLE_OPTIONS[0].label).toBe('Claude Code default');
    expect(CLAUDE_OUTPUT_STYLE_OPTIONS[0].description.toLowerCase()).not.toContain('off');
    expect(CLAUDE_OUTPUT_STYLE_OPTIONS[0].description.toLowerCase()).not.toContain('disabled');
  });

  it('lists exactly the four built-in output styles, each with a description', () => {
    expect(CLAUDE_OUTPUT_STYLE_OPTIONS.map((o) => o.value)).toEqual([
      '',
      'Concise',
      'Proactive',
      'Explanatory',
      'Learning',
    ]);
    for (const option of CLAUDE_OUTPUT_STYLE_OPTIONS) {
      expect(option.description.length).toBeGreaterThan(0);
    }
  });

});

describe('clampSubagentLimit', () => {
  it('treats anything unusable as 0, which means "leave it to Claude Code"', () => {
    // 0 is deliberately NOT "no subagents": the CLI's schema is a positive
    // integer, so the key is omitted rather than sent as a zero.
    expect(clampSubagentLimit(0)).toBe(0);
    expect(clampSubagentLimit(-4)).toBe(0);
    expect(clampSubagentLimit(NaN)).toBe(0);
    expect(clampSubagentLimit(0.5)).toBe(0);
  });

  it('floors and caps', () => {
    expect(clampSubagentLimit(3.9)).toBe(3);
    expect(clampSubagentLimit(CLAUDE_SUBAGENT_LIMIT_MAX + 1)).toBe(CLAUDE_SUBAGENT_LIMIT_MAX);
  });

  it('reports an all-zero group as empty', () => {
    expect(subagentLimitsAreEmpty({})).toBe(true);
    expect(subagentLimitsAreEmpty({ maxSpawnDepth: 0, maxConcurrent: 0 })).toBe(true);
    expect(subagentLimitsAreEmpty({ maxConcurrent: 2 })).toBe(false);
  });
});

describe('toolMemoryLimitError', () => {
  // The accepted grammar mirrors the CLI's own parser,
  // /^(\d+(?:\.\d+)?)\s*([kmgt]?)(?:i?b)?$/i, plus its falsy words and
  // "none". Divergence here is worse than no check: it refuses values the
  // backend and the CLI would both take.
  it.each(['', '4G', '512m', '2GiB', '1.5g', '1024', '1024b', '  8G  ', 'none', 'off', '0'])(
    'accepts %j',
    (value) => {
      expect(toolMemoryLimitError(value)).toBeNull();
    },
  );

  it.each(['4 gigs', 'G', '-1G', '4G!', 'lots', '4..5G'])('refuses %j', (value) => {
    expect(toolMemoryLimitError(value)).not.toBeNull();
  });

  it('refuses an over-long value by length before shape', () => {
    expect(toolMemoryLimitError('4'.repeat(40))).toContain('under 32 characters');
  });
});

describe('thinking axis', () => {
  it("offers Claude's own choice first and does not describe it as off", () => {
    expect(CLAUDE_THINKING_MODE_OPTIONS[0].value).toBe('');
    expect(CLAUDE_THINKING_DISPLAY_OPTIONS[0].value).toBe('');
    expect(CLAUDE_THINKING_MODE_OPTIONS[0].description.toLowerCase()).not.toContain('off');
    // The two hidden-thinking values are the pair a user most easily
    // misreads as each other: one stops the thinking, the other only stops
    // the text.
    expect(CLAUDE_THINKING_MODE_OPTIONS[1].description).toContain('No extended thinking');
    expect(CLAUDE_THINKING_DISPLAY_OPTIONS[2].description).toContain('Claude still thinks');
  });

  // Zero is DISABLED on the wire, not "unset", so this clamp deliberately
  // does not share clampSubagentLimit's fall-to-zero behaviour.
  it.each([
    [0, CLAUDE_THINKING_BUDGET_MIN],
    [-5, CLAUDE_THINKING_BUDGET_MIN],
    [1023, CLAUDE_THINKING_BUDGET_MIN],
    [1024, 1024],
    [2048.9, 2048],
    [999_999, CLAUDE_THINKING_BUDGET_MAX],
  ])('clamps %d to %d', (input, want) => {
    expect(clampThinkingBudget(input)).toBe(want);
  });

  it('answers an unparseable entry with the default rather than zero', () => {
    expect(clampThinkingBudget(Number.NaN)).toBe(CLAUDE_THINKING_BUDGET_DEFAULT);
  });

  it('writes the budget only in budget mode', () => {
    expect(thinkingPatch('budget', 2048, 'omitted')).toEqual({
      mode: 'budget',
      display: 'omitted',
      budgetTokens: 2048,
    });
    expect(thinkingPatch('off', 2048, '')).toEqual({ mode: 'off', display: '' });
    expect(thinkingPatch('', 2048, 'summarized')).toEqual({ mode: '', display: 'summarized' });
  });

  it('clamps through the patch, so an out-of-range budget never reaches the backend', () => {
    expect(thinkingPatch('budget', 1, '').budgetTokens).toBe(CLAUDE_THINKING_BUDGET_MIN);
  });

  // Only the return to Claude's own choice has no wire form: every explicit
  // mode is expressible as a set_max_thinking_tokens request.
  it.each([
    ['', 'off', false],
    ['', 'budget', false],
    ['off', 'budget', false],
    ['budget', 'off', false],
    ['', '', false],
    ['off', '', true],
    ['budget', '', true],
  ] as const)('defers %s -> %s: %s', (from, to, want) => {
    expect(thinkingChangeDefersRestart(from, to)).toBe(want);
  });
});
