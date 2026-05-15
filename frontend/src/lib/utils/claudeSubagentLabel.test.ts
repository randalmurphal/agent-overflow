import { describe, expect, it } from 'vitest';
import {
  deriveClaudeSubagentDescription,
  deriveClaudeSubagentLabel,
  deriveClaudeSubagentModelLabel,
  readClaudeSubagentInput,
  readClaudeSubagentInputFromStrings,
  titleCaseClaudeSubagentToken,
} from './claudeSubagentLabel';

describe('titleCaseClaudeSubagentToken', () => {
  it('returns "" for empty input so callers can fall through without a guard', () => {
    expect(titleCaseClaudeSubagentToken('')).toBe('');
  });

  it('title-cases a single lowercase token', () => {
    expect(titleCaseClaudeSubagentToken('explore')).toBe('Explore');
  });

  it('splits kebab-case into capitalized words', () => {
    expect(titleCaseClaudeSubagentToken('general-purpose')).toBe('General Purpose');
  });

  it('splits snake_case into capitalized words', () => {
    expect(titleCaseClaudeSubagentToken('code_reviewer')).toBe('Code Reviewer');
  });

  it('splits CamelCase boundaries into capitalized words', () => {
    expect(titleCaseClaudeSubagentToken('spawnAgent')).toBe('Spawn Agent');
  });

  it('collapses runs of mixed delimiters and skips empties', () => {
    expect(titleCaseClaudeSubagentToken(' code__reviewer--lead ')).toBe('Code Reviewer Lead');
  });

  it('leaves already-capitalised tokens untouched', () => {
    expect(titleCaseClaudeSubagentToken('Explore')).toBe('Explore');
  });
});

describe('readClaudeSubagentInput', () => {
  it('returns null when neither side carries input', () => {
    expect(readClaudeSubagentInput(null, null)).toBeNull();
    expect(readClaudeSubagentInput({}, {})).toBeNull();
  });

  it('prefers payloadMeta.input over parentMeta.input (payloadMeta settles last)', () => {
    const payload = { input: { subagent_type: 'Explore' } };
    const parent = { input: { subagent_type: 'Stale' } };
    expect(readClaudeSubagentInput(payload, parent)).toEqual({ subagent_type: 'Explore' });
  });

  it('falls back to parentMeta.input when payloadMeta lacks input', () => {
    const parent = { input: { subagent_type: 'Explore' } };
    expect(readClaudeSubagentInput(null, parent)).toEqual({ subagent_type: 'Explore' });
    expect(readClaudeSubagentInput({}, parent)).toEqual({ subagent_type: 'Explore' });
  });

  it('rejects non-object input shapes (arrays, primitives)', () => {
    expect(readClaudeSubagentInput({ input: ['a', 'b'] }, null)).toBeNull();
    expect(readClaudeSubagentInput({ input: 'string-input' }, null)).toBeNull();
    expect(readClaudeSubagentInput({ input: 42 }, null)).toBeNull();
    expect(readClaudeSubagentInput({ input: null }, null)).toBeNull();
  });
});

describe('readClaudeSubagentInputFromStrings', () => {
  it('parses JSON and resolves through the same precedence rule', () => {
    const payloadMetaJson = JSON.stringify({ input: { subagent_type: 'Explore' } });
    const metaJson = JSON.stringify({ input: { subagent_type: 'Stale' } });
    expect(readClaudeSubagentInputFromStrings(payloadMetaJson, metaJson)).toEqual({
      subagent_type: 'Explore',
    });
  });

  it('handles undefined / empty / malformed JSON without throwing', () => {
    expect(readClaudeSubagentInputFromStrings(undefined, undefined)).toBeNull();
    expect(readClaudeSubagentInputFromStrings('', '')).toBeNull();
    expect(readClaudeSubagentInputFromStrings('not-json', 'still-not')).toBeNull();
  });
});

describe('deriveClaudeSubagentLabel', () => {
  it('title-cases input.subagent_type when toolName === "Agent"', () => {
    expect(deriveClaudeSubagentLabel({ subagent_type: 'general-purpose' }, 'Agent')).toBe(
      'General Purpose',
    );
  });

  it('falls back to "Agent" when subagent_type is missing on an Agent row', () => {
    expect(deriveClaudeSubagentLabel({}, 'Agent')).toBe('Agent');
    expect(deriveClaudeSubagentLabel(null, 'Agent')).toBe('Agent');
  });

  it('uses the tool name for non-Agent tools so the row never goes blank', () => {
    // The non-Agent branch exists for defensive callers; SubagentGroup
    // can in principle hit it if a non-Agent tool somehow declared a
    // group parent. Title-casing the tool name keeps the label sane.
    expect(deriveClaudeSubagentLabel(null, 'Read')).toBe('Read');
    expect(deriveClaudeSubagentLabel(null, 'snake_case_tool')).toBe('Snake Case Tool');
  });

  it('falls back to "Subagent" when toolName is also empty', () => {
    expect(deriveClaudeSubagentLabel(null, '')).toBe('Subagent');
  });
});

describe('deriveClaudeSubagentModelLabel', () => {
  it('returns "" for non-Agent tools so non-subagent rows never render a stray "()" affix', () => {
    expect(
      deriveClaudeSubagentModelLabel(
        { model: 'opus' },
        { subagent_model: 'claude-opus-4-7' },
        'Read',
      ),
    ).toBe('');
  });

  it('prefers parentMeta.subagent_model (the post-spawn stamp) over input.model', () => {
    expect(
      deriveClaudeSubagentModelLabel(
        { model: 'sonnet' },
        { subagent_model: 'claude-opus-4-7' },
        'Agent',
      ),
    ).toBe('Opus 4.7');
  });

  it('falls back to input.model for the pre-stamp window', () => {
    expect(deriveClaudeSubagentModelLabel({ model: 'opus' }, {}, 'Agent')).toBe('Opus');
    expect(deriveClaudeSubagentModelLabel({ model: 'opus' }, null, 'Agent')).toBe('Opus');
  });

  it('returns "" when neither source carries a model', () => {
    expect(deriveClaudeSubagentModelLabel(null, null, 'Agent')).toBe('');
    expect(deriveClaudeSubagentModelLabel({}, {}, 'Agent')).toBe('');
  });

  it('rejects empty / whitespace-only subagent_model strings (must fall through to input.model)', () => {
    expect(
      deriveClaudeSubagentModelLabel(
        { model: 'opus' },
        { subagent_model: '   ' },
        'Agent',
      ),
    ).toBe('Opus');
  });
});

describe('deriveClaudeSubagentDescription', () => {
  it('returns input.description verbatim when present', () => {
    expect(deriveClaudeSubagentDescription({ description: 'Find foo' })).toBe('Find foo');
  });

  it('falls back to input.prompt when description is missing', () => {
    expect(deriveClaudeSubagentDescription({ prompt: 'Short prompt' })).toBe('Short prompt');
  });

  it('truncates a prompt longer than 80 chars and appends an ellipsis', () => {
    const longPrompt = 'A'.repeat(120);
    const result = deriveClaudeSubagentDescription({ prompt: longPrompt });
    expect(result).toBe(`${'A'.repeat(80)}…`);
  });

  it('does NOT truncate description (it is already display-shaped)', () => {
    const longDescription = 'B'.repeat(120);
    expect(deriveClaudeSubagentDescription({ description: longDescription })).toBe(longDescription);
  });

  it('returns "" when neither description nor prompt is present', () => {
    expect(deriveClaudeSubagentDescription({})).toBe('');
    expect(deriveClaudeSubagentDescription(null)).toBe('');
  });

  it('trims whitespace-only descriptions to empty so the empty branch fires', () => {
    expect(deriveClaudeSubagentDescription({ description: '   ', prompt: 'fallback' })).toBe(
      'fallback',
    );
  });
});
