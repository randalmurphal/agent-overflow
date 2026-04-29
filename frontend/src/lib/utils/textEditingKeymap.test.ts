import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  dispatchTextEditing,
  wordBoundaryBackward,
  wordBoundaryForward,
} from './textEditingKeymap';

describe('wordBoundaryBackward', () => {
  it('returns 0 at the start of the string', () => {
    expect(wordBoundaryBackward('hello world', 0)).toBe(0);
  });

  it('jumps from end of word to start of word', () => {
    expect(wordBoundaryBackward('hello world', 11)).toBe(6);
  });

  it('skips trailing whitespace before the previous word', () => {
    expect(wordBoundaryBackward('hello   ', 8)).toBe(0);
  });

  it('jumps over one word per call', () => {
    expect(wordBoundaryBackward('alpha beta gamma', 16)).toBe(11);
  });

  it('jumps to the start of the word the caret sits inside', () => {
    expect(wordBoundaryBackward('hello world', 9)).toBe(6);
  });

  it('returns 0 when only non-word characters precede the caret', () => {
    expect(wordBoundaryBackward('   ', 3)).toBe(0);
  });

  describe('VS Code-style: each separator is its own step', () => {
    it("test;test' — four steps for word/sep/word/sep", () => {
      // Step 1: drop the trailing apostrophe.
      expect(wordBoundaryBackward("test;test'", 10)).toBe(9);
      // Step 2: drop the second word.
      expect(wordBoundaryBackward('test;test', 9)).toBe(5);
      // Step 3: drop the semicolon.
      expect(wordBoundaryBackward('test;', 5)).toBe(4);
      // Step 4: drop the first word.
      expect(wordBoundaryBackward('test', 4)).toBe(0);
    });

    it('--model=opus — five steps (opus, =, model, -, -)', () => {
      expect(wordBoundaryBackward('--model=opus', 12)).toBe(8);
      expect(wordBoundaryBackward('--model=', 8)).toBe(7);
      expect(wordBoundaryBackward('--model', 7)).toBe(2);
      expect(wordBoundaryBackward('--', 2)).toBe(1);
      expect(wordBoundaryBackward('-', 1)).toBe(0);
    });

    it('folds leading whitespace into the step crossing it', () => {
      // " ; " at the tail collapses with the semicolon.
      expect(wordBoundaryBackward('foo ; ', 6)).toBe(4);
      // Whitespace adjacent to a word folds with the word.
      expect(wordBoundaryBackward('foo ', 4)).toBe(0);
    });

    it('treats Unicode letters and digits as word chars', () => {
      expect(wordBoundaryBackward('テスト', 3)).toBe(0);
      expect(wordBoundaryBackward('café', 4)).toBe(0);
      expect(wordBoundaryBackward('var123', 6)).toBe(0);
    });
  });
});

describe('wordBoundaryForward', () => {
  it('returns text length at the end of the string', () => {
    expect(wordBoundaryForward('hello world', 11)).toBe(11);
  });

  it('jumps from start to end of next word', () => {
    expect(wordBoundaryForward('hello world', 0)).toBe(5);
  });

  it('skips leading whitespace into the next word', () => {
    expect(wordBoundaryForward('   hello', 0)).toBe(8);
  });

  it('jumps over one word per call', () => {
    expect(wordBoundaryForward('alpha beta gamma', 0)).toBe(5);
    expect(wordBoundaryForward('alpha beta gamma', 5)).toBe(10);
  });

  it('returns text length when only non-word characters remain', () => {
    expect(wordBoundaryForward('hello   ', 5)).toBe(8);
  });

  describe('VS Code-style: each separator is its own step', () => {
    it("test;test' — four steps from cursor at start", () => {
      expect(wordBoundaryForward("test;test'", 0)).toBe(4);
      expect(wordBoundaryForward(";test'", 0)).toBe(1);
      expect(wordBoundaryForward("test'", 0)).toBe(4);
      expect(wordBoundaryForward("'", 0)).toBe(1);
    });

    it('folds leading whitespace into the step crossing it', () => {
      expect(wordBoundaryForward(' test', 0)).toBe(5);
      expect(wordBoundaryForward(' ;', 0)).toBe(2);
    });
  });
});

function makeEvent(
  key: string,
  init: Partial<KeyboardEventInit> & { isComposing?: boolean } = {},
  target?: EventTarget,
): KeyboardEvent {
  const ev = new KeyboardEvent('keydown', {
    key,
    bubbles: true,
    cancelable: true,
    ...init,
  });
  if (target) {
    Object.defineProperty(ev, 'target', { value: target, configurable: true });
  }
  if (init.isComposing !== undefined) {
    Object.defineProperty(ev, 'isComposing', { value: init.isComposing });
  }
  return ev;
}

function createTextarea(value: string, selectionStart: number, selectionEnd?: number): HTMLTextAreaElement {
  const ta = document.createElement('textarea');
  document.body.appendChild(ta);
  ta.value = value;
  ta.setSelectionRange(selectionStart, selectionEnd ?? selectionStart);
  return ta;
}

describe('dispatchTextEditing', () => {
  // Stub `execCommand` to a no-op so we can observe the selection state
  // the dispatcher produced *before* the deletion / insertion would have
  // mutated it. The repo-wide polyfill in `src/test/setup.ts` is fine for
  // component tests but would erase the selection range we want to assert.
  let execSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    execSpy = vi.spyOn(document, 'execCommand').mockImplementation(() => true);
  });

  afterEach(() => {
    execSpy.mockRestore();
    document.body.innerHTML = '';
  });

  it('returns false for a key with no Alt or Ctrl modifier', () => {
    const ta = createTextarea('hello', 5);
    const ev = makeEvent('Backspace', {}, ta);
    expect(dispatchTextEditing(ev)).toBe(false);
    expect(execSpy).not.toHaveBeenCalled();
  });

  it('returns false when Cmd (metaKey) is held — leave OS line ops native', () => {
    const ta = createTextarea('hello world', 11);
    const ev = makeEvent('Backspace', { metaKey: true, altKey: true }, ta);
    expect(dispatchTextEditing(ev)).toBe(false);
    expect(execSpy).not.toHaveBeenCalled();
  });

  it('returns false when both Alt and Ctrl are held', () => {
    const ta = createTextarea('hello world', 11);
    const ev = makeEvent('Backspace', { altKey: true, ctrlKey: true }, ta);
    expect(dispatchTextEditing(ev)).toBe(false);
  });

  it('returns false during IME composition', () => {
    const ta = createTextarea('hello', 5);
    const ev = makeEvent('Backspace', { altKey: true, isComposing: true }, ta);
    expect(dispatchTextEditing(ev)).toBe(false);
  });

  it('returns false for non-text inputs', () => {
    const input = document.createElement('input');
    input.type = 'number';
    document.body.appendChild(input);
    input.value = '42';
    const ev = makeEvent('Backspace', { altKey: true }, input);
    expect(dispatchTextEditing(ev)).toBe(false);
  });

  it('returns false for readOnly textarea', () => {
    const ta = createTextarea('hello', 5);
    ta.readOnly = true;
    const ev = makeEvent('Backspace', { altKey: true }, ta);
    expect(dispatchTextEditing(ev)).toBe(false);
  });

  it('returns false for disabled textarea', () => {
    const ta = createTextarea('hello', 5);
    ta.disabled = true;
    const ev = makeEvent('Backspace', { altKey: true }, ta);
    expect(dispatchTextEditing(ev)).toBe(false);
  });

  describe('Alt+Backspace = delete word back', () => {
    it('selects the prior word and calls execCommand("delete")', () => {
      const ta = createTextarea('hello world', 11);
      const ev = makeEvent('Backspace', { altKey: true }, ta);
      const handled = dispatchTextEditing(ev);
      expect(handled).toBe(true);
      expect(ta.selectionStart).toBe(6);
      expect(ta.selectionEnd).toBe(11);
      expect(execSpy).toHaveBeenCalledWith('delete');
    });

    it('Ctrl+Backspace produces the same behavior', () => {
      const ta = createTextarea('hello world', 11);
      const ev = makeEvent('Backspace', { ctrlKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(ta.selectionStart).toBe(6);
      expect(ta.selectionEnd).toBe(11);
    });

    it('deletes existing selection without recomputing word boundary', () => {
      const ta = createTextarea('hello world', 0, 5);
      const ev = makeEvent('Backspace', { altKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      // selection unchanged; just calls execCommand
      expect(ta.selectionStart).toBe(0);
      expect(ta.selectionEnd).toBe(5);
      expect(execSpy).toHaveBeenCalledWith('delete');
    });

    it('is a no-op at position 0', () => {
      const ta = createTextarea('hello', 0);
      const ev = makeEvent('Backspace', { altKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(execSpy).not.toHaveBeenCalled();
    });
  });

  describe('Alt+Delete = delete word forward', () => {
    it('selects the next word forward and deletes', () => {
      const ta = createTextarea('hello world', 0);
      const ev = makeEvent('Delete', { altKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(ta.selectionStart).toBe(0);
      expect(ta.selectionEnd).toBe(5);
      expect(execSpy).toHaveBeenCalledWith('delete');
    });

    it('is a no-op at end of string', () => {
      const ta = createTextarea('hello', 5);
      const ev = makeEvent('Delete', { ctrlKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(execSpy).not.toHaveBeenCalled();
    });
  });

  describe('Alt+ArrowLeft = caret to prev word', () => {
    it('moves caret backward without selecting', () => {
      const ta = createTextarea('alpha beta gamma', 16);
      const ev = makeEvent('ArrowLeft', { altKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(ta.selectionStart).toBe(11);
      expect(ta.selectionEnd).toBe(11);
      expect(execSpy).not.toHaveBeenCalled();
    });

    it('Shift-extend: anchors at end of selection, moves active end backward', () => {
      const ta = createTextarea('alpha beta gamma', 16);
      const ev = makeEvent('ArrowLeft', { altKey: true, shiftKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(ta.selectionStart).toBe(11);
      expect(ta.selectionEnd).toBe(16);
    });
  });

  describe('Alt+ArrowRight = caret to next word', () => {
    it('moves caret forward without selecting', () => {
      const ta = createTextarea('alpha beta gamma', 0);
      const ev = makeEvent('ArrowRight', { ctrlKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(ta.selectionStart).toBe(5);
      expect(ta.selectionEnd).toBe(5);
    });

    it('Shift-extend: anchors at start of selection, moves active end forward', () => {
      const ta = createTextarea('alpha beta gamma', 0);
      const ev = makeEvent('ArrowRight', { altKey: true, shiftKey: true }, ta);
      expect(dispatchTextEditing(ev)).toBe(true);
      expect(ta.selectionStart).toBe(0);
      expect(ta.selectionEnd).toBe(5);
    });
  });

  it('returns false for a key it does not handle (e.g. Alt+Home)', () => {
    const ta = createTextarea('hello', 5);
    const ev = makeEvent('Home', { altKey: true }, ta);
    expect(dispatchTextEditing(ev)).toBe(false);
  });

  it('handles a plain text input the same as a textarea', () => {
    const input = document.createElement('input');
    input.type = 'text';
    document.body.appendChild(input);
    input.value = 'hello world';
    input.setSelectionRange(11, 11);
    const ev = makeEvent('Backspace', { altKey: true }, input);
    expect(dispatchTextEditing(ev)).toBe(true);
    expect(input.selectionStart).toBe(6);
    expect(input.selectionEnd).toBe(11);
  });
});
