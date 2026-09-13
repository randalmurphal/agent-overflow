import { beforeEach, describe, expect, it } from 'vitest';
import {
  CODE_WRAP_STATE_MAX_ENTRIES,
  isCodeBlockUnwrapped,
  isCodeBlockUnwrappedByKey,
  resetCodeWrapStateForTest,
  setCodeBlockUnwrapped,
  setCodeBlockUnwrappedByKey,
} from './codeWrapState';
import { contentKey } from '../../../utils/fnv1a';

beforeEach(() => {
  resetCodeWrapStateForTest();
});

describe('codeWrapState', () => {
  it('defaults every block to wrapped', () => {
    expect(isCodeBlockUnwrapped('python', 'x = 1')).toBe(false);
  });

  it('round-trips a choice by language and source, and clears it', () => {
    setCodeBlockUnwrapped('python', 'x = 1', true);
    expect(isCodeBlockUnwrapped('python', 'x = 1')).toBe(true);
    // Language is part of the identity: the same text under another fence
    // language is another block.
    expect(isCodeBlockUnwrapped('ts', 'x = 1')).toBe(false);
    expect(isCodeBlockUnwrapped('python', 'x = 2')).toBe(false);

    setCodeBlockUnwrapped('python', 'x = 1', false);
    expect(isCodeBlockUnwrapped('python', 'x = 1')).toBe(false);
  });

  it('agrees between the source form and the content-key form', () => {
    // The live host records by its streaming identity's content key; the
    // static delegate records by the code element's text. Both must land on
    // the same entry.
    setCodeBlockUnwrappedByKey('go', contentKey('fmt.Println()'), true);
    expect(isCodeBlockUnwrapped('go', 'fmt.Println()')).toBe(true);
    setCodeBlockUnwrapped('go', 'fmt.Println()', false);
    expect(isCodeBlockUnwrappedByKey('go', contentKey('fmt.Println()'))).toBe(false);
  });

  it('bounds retained choices and evicts the oldest first', () => {
    for (let index = 0; index < CODE_WRAP_STATE_MAX_ENTRIES + 1; index += 1) {
      setCodeBlockUnwrapped('python', `line ${index}`, true);
    }
    expect(isCodeBlockUnwrapped('python', 'line 0')).toBe(false);
    expect(isCodeBlockUnwrapped('python', 'line 1')).toBe(true);
    expect(isCodeBlockUnwrapped('python', `line ${CODE_WRAP_STATE_MAX_ENTRIES}`)).toBe(true);
  });

  it('re-setting a choice refreshes its recency', () => {
    setCodeBlockUnwrapped('python', 'keep', true);
    for (let index = 0; index < CODE_WRAP_STATE_MAX_ENTRIES - 1; index += 1) {
      setCodeBlockUnwrapped('python', `line ${index}`, true);
    }
    setCodeBlockUnwrapped('python', 'keep', true);
    setCodeBlockUnwrapped('python', 'one more', true);
    expect(isCodeBlockUnwrapped('python', 'keep')).toBe(true);
    expect(isCodeBlockUnwrapped('python', 'line 0')).toBe(false);
  });
});
