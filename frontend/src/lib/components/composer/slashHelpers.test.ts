import { describe, expect, it } from 'vitest';
import { detectSlashTrigger } from './slashHelpers';

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
