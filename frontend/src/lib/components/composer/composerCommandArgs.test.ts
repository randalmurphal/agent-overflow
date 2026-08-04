import { describe, expect, it } from 'vitest';
import { resolveArgCandidate } from './composerCommandArgs';

const models = [
  { id: 'claude-opus-5', label: 'Opus 5' },
  { id: 'claude-opus-5[1m]', label: 'Opus 5 (1M)' },
  { id: 'claude-sonnet-4-5', label: 'Sonnet 4.5' },
  { id: 'claude-haiku-4-5', label: 'Haiku 4.5' },
];

describe('resolveArgCandidate', () => {
  it('takes an exact id, case-insensitively', () => {
    expect(resolveArgCandidate('claude-sonnet-4-5', models, 'model').id).toBe('claude-sonnet-4-5');
    expect(resolveArgCandidate('CLAUDE-SONNET-4-5', models, 'model').id).toBe('claude-sonnet-4-5');
  });

  it('takes an exact label before considering substrings', () => {
    // "Opus 5" is a substring of "Opus 5 (1M)" too — the exact label has to win
    // or the plain model would be unreachable by its own name.
    expect(resolveArgCandidate('opus 5', models, 'model').id).toBe('claude-opus-5');
  });

  it('takes a unique substring of either the id or the label', () => {
    expect(resolveArgCandidate('haiku', models, 'model').id).toBe('claude-haiku-4-5');
    expect(resolveArgCandidate('sonnet', models, 'model').id).toBe('claude-sonnet-4-5');
  });

  it('refuses an ambiguous match and names the candidates', () => {
    const resolved = resolveArgCandidate('opus', models, 'model');
    expect(resolved.id).toBeUndefined();
    expect(resolved.error).toMatch(/matches several models/);
    expect(resolved.error).toMatch(/Opus 5/);
    expect(resolved.error).toMatch(/Opus 5 \(1M\)/);
  });

  it('refuses an unmatched arg and lists what is available', () => {
    const resolved = resolveArgCandidate('gpt-9', models, 'model');
    expect(resolved.id).toBeUndefined();
    expect(resolved.error).toMatch(/No model matches “gpt-9”/);
    expect(resolved.error).toMatch(/Opus 5/);
  });

  it('asks for an argument rather than picking one when the arg is blank', () => {
    expect(resolveArgCandidate('  ', models, 'model').error).toMatch(/Name a model/);
  });

  it('says so when there is nothing to choose from', () => {
    expect(resolveArgCandidate('high', [], 'effort tier').error).toMatch(/No effort tier options/);
  });

  it('caps the suggestion list so an error stays readable', () => {
    const many = Array.from({ length: 20 }, (_, i) => ({ id: `m${i}`, label: `Model ${i}` }));
    const resolved = resolveArgCandidate('nope', many, 'model');
    expect(resolved.error).toMatch(/…$/);
    expect(resolved.error!.split(', ').length).toBeLessThanOrEqual(8);
  });
});
