import { describe, expect, it } from 'vitest';
import { applySlashCommand, detectSlashTrigger } from './slashHelpers';

describe('detectSlashTrigger', () => {
  it('returns null when the message is empty', () => {
    expect(detectSlashTrigger('', 0)).toBeNull();
  });

  it('returns null when the first character is not /', () => {
    expect(detectSlashTrigger('hello', 5)).toBeNull();
    expect(detectSlashTrigger(' /init', 6)).toBeNull();
  });

  it('detects a bare / at position 1 with an empty query', () => {
    expect(detectSlashTrigger('/', 1)).toEqual({ text: '', start: 0 });
  });

  it('detects /init with the partial up to the caret', () => {
    expect(detectSlashTrigger('/ini', 4)).toEqual({ text: 'ini', start: 0 });
  });

  it('ignores a slash that appears later in the message', () => {
    expect(detectSlashTrigger('hello /init', 11)).toBeNull();
  });

  it('does not trigger on //', () => {
    expect(detectSlashTrigger('//', 2)).toBeNull();
    expect(detectSlashTrigger('//review', 8)).toBeNull();
  });

  it('closes the trigger once the partial contains whitespace', () => {
    // "/init hello" — the space closes the command span.
    expect(detectSlashTrigger('/init hello', 11)).toBeNull();
    // Caret just after the space still does not re-open it.
    expect(detectSlashTrigger('/init ', 6)).toBeNull();
  });

  it('closes the trigger when caret is mid-word with non-whitespace to the right', () => {
    // Caret between "/ini" and "t" — trailing "t" would be joined onto the
    // command without a separator, so we don't surface a popover here.
    expect(detectSlashTrigger('/init', 4)).toBeNull();
  });

  it('keeps the trigger open when trailing content is whitespace', () => {
    // Caret after "/ini" with a trailing space — the partial is still the
    // selected command.
    expect(detectSlashTrigger('/ini ', 4)).toEqual({ text: 'ini', start: 0 });
  });

  it('rejects caret positions outside the value', () => {
    expect(detectSlashTrigger('/init', -1)).toBeNull();
    expect(detectSlashTrigger('/init', 99)).toBeNull();
  });
});

describe('applySlashCommand', () => {
  it('replaces the trigger span with /command and a trailing space', () => {
    const result = applySlashCommand('/ini', { text: 'ini', start: 0 }, 'init');
    expect(result.value).toBe('/init ');
    expect(result.nextCaret).toBe('/init '.length);
  });

  it('handles an empty partial (bare /)', () => {
    const result = applySlashCommand('/', { text: '', start: 0 }, 'review');
    expect(result.value).toBe('/review ');
    expect(result.nextCaret).toBe('/review '.length);
  });

  it('handles a full match (partial equals command)', () => {
    const result = applySlashCommand('/review', { text: 'review', start: 0 }, 'review');
    expect(result.value).toBe('/review ');
    expect(result.nextCaret).toBe('/review '.length);
  });

  it('preserves content that follows the trigger span (trailing whitespace)', () => {
    // Caret sits right after the partial; the trailing space is kept.
    const result = applySlashCommand('/ini hello', { text: 'ini', start: 0 }, 'init');
    // Replacement inserts "/init " and leaves the existing " hello" intact,
    // so the total has a doubled separator — the common case is an empty
    // tail, but if the user already typed a space we don't eat it.
    expect(result.value).toBe('/init  hello');
    expect(result.nextCaret).toBe('/init '.length);
  });
});
