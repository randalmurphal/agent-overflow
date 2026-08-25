import { afterEach, describe, expect, it, vi } from 'vitest';
import { replaceTextareaValue } from './textareaEdit';

describe('replaceTextareaValue', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.body.replaceChildren();
  });

  function textareaWith(value: string): HTMLTextAreaElement {
    const textarea = document.createElement('textarea');
    textarea.value = value;
    document.body.append(textarea);
    textarea.focus();
    return textarea;
  }

  function observeNativeEdits(textarea: HTMLTextAreaElement) {
    const observed: Array<{ start: number; end: number; text: string }> = [];
    vi.spyOn(document, 'execCommand').mockImplementation((_command, _showUI, text) => {
      const start = textarea.selectionStart;
      const end = textarea.selectionEnd;
      const inserted = text ?? '';
      observed.push({ start, end, text: inserted });
      textarea.value = textarea.value.slice(0, start) + inserted + textarea.value.slice(end);
      const caret = start + inserted.length;
      textarea.setSelectionRange(caret, caret);
      return true;
    });
    return observed;
  }

  it('inserts through only the changed range and leaves the shared text mounted', () => {
    const textarea = textareaWith('before after');
    const observed = observeNativeEdits(textarea);

    replaceTextareaValue(textarea, 'before [Image #1] after', 17);

    expect(observed).toEqual([{ start: 7, end: 7, text: '[Image #1] ' }]);
    expect(textarea.selectionStart).toBe(17);
    expect(textarea.selectionEnd).toBe(17);
  });

  it('uses one bounded replacement when deletion also renumbers later placeholders', () => {
    const textarea = textareaWith('a [Image #1] middle [Image #2] z');
    const observed = observeNativeEdits(textarea);

    replaceTextareaValue(textarea, 'a middle [Image #1] z', 1);

    expect(observed).toEqual([{
      start: 2,
      end: 29,
      text: 'middle [Image #1',
    }]);
    expect(textarea.selectionStart).toBe(1);
    expect(textarea.selectionEnd).toBe(1);
  });

  it('moves only the caret when the value is already current', () => {
    const textarea = textareaWith('unchanged');
    const edit = vi.spyOn(document, 'execCommand');

    replaceTextareaValue(textarea, 'unchanged', 3);

    expect(edit).not.toHaveBeenCalled();
    expect(textarea.selectionStart).toBe(3);
    expect(textarea.selectionEnd).toBe(3);
  });
});
