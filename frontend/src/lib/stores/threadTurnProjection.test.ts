import { describe, expect, it } from 'vitest';
import {
  parseTokenUsage,
  turnRowToSettled,
} from './threadTurnProjection';

describe('threadTurnProjection', () => {
  it('parses camelCase and snake_case token usage fields', () => {
    expect(parseTokenUsage(JSON.stringify({
      inputTokens: 10,
      outputTokens: 20,
      cacheReadInputTokens: 3,
      cacheCreationInputTokens: 4,
      totalCostUsd: 0.05,
    }))).toEqual({
      inputTokens: 10,
      outputTokens: 20,
      cacheReadInputTokens: 3,
      cacheCreationInputTokens: 4,
      totalCostUsd: 0.05,
    });

    expect(parseTokenUsage(JSON.stringify({
      input_tokens: 1,
      output_tokens: 2,
      cache_read_input_tokens: 3,
      cache_creation_input_tokens: 4,
      total_cost_usd: 0.5,
    }))).toEqual({
      inputTokens: 1,
      outputTokens: 2,
      cacheReadInputTokens: 3,
      cacheCreationInputTokens: 4,
      totalCostUsd: 0.5,
    });
  });

  it('returns null for missing or malformed token usage', () => {
    expect(parseTokenUsage('')).toBeNull();
    expect(parseTokenUsage(null)).toBeNull();
    expect(parseTokenUsage('{not json')).toBeNull();
  });

  it('projects persisted turn rows into settled turns', () => {
    expect(turnRowToSettled({
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 4,
      startedAt: 100,
      completedAt: 200,
      stopReason: 'interrupted',
      assistantMessageId: '',
      tokenUsageJson: JSON.stringify({ input_tokens: 1, output_tokens: 2 }),
      errorMessage: 'user stopped',
    })).toEqual({
      turnId: 'turn-1',
      turnIndex: 4,
      startedAt: 100,
      completedAt: 200,
      stopReason: 'interrupted',
      assistantMessageId: null,
      tokenUsage: { inputTokens: 1, outputTokens: 2 },
      aborted: true,
      errorMessage: 'user stopped',
    });
  });

});
