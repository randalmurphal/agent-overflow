import { describe, expect, it } from 'vitest';
import {
  detectCommandTrigger,
  detectReviewTargetTrigger,
} from './composerCommandTrigger';

describe('detectCommandTrigger', () => {
  it('opens on a leading slash and reports the query up to the caret', () => {
    expect(detectCommandTrigger('/', 1)).toMatchObject({ query: '', start: 0, end: 1 });
    expect(detectCommandTrigger('/w', 2)).toMatchObject({ query: 'w', start: 0, end: 2 });
    expect(detectCommandTrigger('/workflow', 9)).toMatchObject({ query: 'workflow' });
  });

  it('marks only a position-0 slash as atStart', () => {
    expect(detectCommandTrigger('/m', 2)?.atStart).toBe(true);
    expect(detectCommandTrigger('hello /m', 8)?.atStart).toBe(false);
    // Leading whitespace is NOT the start: the CLI's router tests the raw
    // string's first byte, and interception mirrors it, so a menu that
    // offered provider commands here would promise something send won't do.
    expect(detectCommandTrigger(' /m', 3)?.atStart).toBe(false);
    expect(detectCommandTrigger('line one\n/m', 11)?.atStart).toBe(false);
  });

  it('stays open for a word nothing matches — the caller decides to show it', () => {
    // Filtering moved to the caller, so a name past every registered one is
    // still a trigger; the menu simply has no rows.
    expect(detectCommandTrigger('/workflowish', 12)).toMatchObject({ query: 'workflowish' });
  });

  it('triggers on a word anywhere in the draft', () => {
    expect(detectCommandTrigger('hello /w', 8)).toMatchObject({ query: 'w', start: 6, end: 8 });
    expect(detectCommandTrigger('/workflow now /w', 16)).toMatchObject({ query: 'w', start: 14 });
  });

  it('never triggers on a slash inside a word, so paths stay text', () => {
    expect(detectCommandTrigger('src/w', 5)).toBeNull();
    expect(detectCommandTrigger('/tmp/w', 6)).toBeNull();
  });

  it('closes once the caret leaves the word', () => {
    expect(detectCommandTrigger('/workflow ', 10)).toBeNull();
    expect(detectCommandTrigger('/workflow do it', 15)).toBeNull();
    expect(detectCommandTrigger('/workflow do it', 5)).toMatchObject({ query: 'work', end: 5 });
  });

  it('refuses carets outside the value and a caret on the slash itself', () => {
    expect(detectCommandTrigger('/w', 0)).toBeNull();
    expect(detectCommandTrigger('/w', 3)).toBeNull();
    expect(detectCommandTrigger('', 0)).toBeNull();
  });
});

describe('detectReviewTargetTrigger', () => {
  it('opens in the argument of a leading /review', () => {
    expect(detectReviewTargetTrigger('/review ', 8)).toMatchObject({ query: '', start: 8, end: 8 });
    expect(detectReviewTargetTrigger('/review bra', 11)).toMatchObject({
      query: 'bra',
      start: 8,
      end: 11,
    });
  });

  it('anchors the range at the argument, so a branch name with a slash is replaced whole', () => {
    const trigger = detectReviewTargetTrigger('/review branch feat/x', 21);
    expect(trigger).toMatchObject({ query: 'branch feat/x', start: 8, end: 21 });
  });

  it('stays closed until the command word is settled', () => {
    expect(detectReviewTargetTrigger('/review', 7)).toBeNull();
    expect(detectReviewTargetTrigger('/reviewer x', 11)).toBeNull();
    expect(detectReviewTargetTrigger('please /review x', 16)).toBeNull();
  });

  it('closes once custom takes over with free-form instructions', () => {
    expect(detectReviewTargetTrigger('/review custom', 14)).toMatchObject({ query: 'custom' });
    expect(detectReviewTargetTrigger('/review custom check the locks', 30)).toBeNull();
  });

  it('closes on a second line — the argument is single-line', () => {
    expect(detectReviewTargetTrigger('/review\nbranch main', 18)).toBeNull();
    expect(detectReviewTargetTrigger('/review branch\nmain', 19)).toBeNull();
  });
});
