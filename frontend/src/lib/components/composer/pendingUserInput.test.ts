import { describe, expect, it } from 'vitest';
import type { UserInputQuestion, UserInputRequest } from '../../types/events';
import {
  createUserInputAnswers,
  resolvedAnswer,
  setCustomAnswer,
  toResponseAnswers,
  toggleOptionAnswer,
} from './pendingUserInput';

function singleSelect(overrides: Partial<UserInputQuestion> = {}): UserInputQuestion {
  return {
    id: 'q',
    header: 'Q',
    question: 'Pick one',
    options: [
      { label: 'A', description: '' },
      { label: 'B', description: '' },
    ],
    ...overrides,
  };
}

function multiSelect(overrides: Partial<UserInputQuestion> = {}): UserInputQuestion {
  return singleSelect({ multiSelect: true, ...overrides });
}

function requestWith(...questions: UserInputQuestion[]): UserInputRequest {
  return {
    requestId: 'req',
    threadId: 'thread',
    toolName: 'request_user_input',
    title: 'Title',
    questions,
  };
}

describe('pendingUserInput', () => {
  describe('setCustomAnswer', () => {
    it('multi-select keeps selections alongside the typed answer', () => {
      const q = multiSelect();
      let answers = toggleOptionAnswer(createUserInputAnswers(), q, 'A');
      answers = setCustomAnswer(answers, q, 'custom');
      expect(answers[q.id].selectedOptionLabels).toEqual(['A']);
      expect(answers[q.id].customAnswer).toBe('custom');
    });

    it('single-select clears selections as soon as a value is typed', () => {
      const q = singleSelect();
      let answers = toggleOptionAnswer(createUserInputAnswers(), q, 'A');
      answers = setCustomAnswer(answers, q, 'custom');
      expect(answers[q.id].selectedOptionLabels ?? []).toEqual([]);
      expect(answers[q.id].customAnswer).toBe('custom');
    });
  });

  describe('toggleOptionAnswer', () => {
    it('multi-select preserves an existing custom answer', () => {
      const q = multiSelect();
      let answers = setCustomAnswer(createUserInputAnswers(), q, 'custom');
      answers = toggleOptionAnswer(answers, q, 'A');
      expect(answers[q.id].customAnswer).toBe('custom');
      expect(answers[q.id].selectedOptionLabels).toEqual(['A']);
    });

    it('multi-select toggles an option off without dropping the custom answer', () => {
      const q = multiSelect();
      let answers = setCustomAnswer(createUserInputAnswers(), q, 'custom');
      answers = toggleOptionAnswer(answers, q, 'A');
      answers = toggleOptionAnswer(answers, q, 'A');
      expect(answers[q.id].selectedOptionLabels ?? []).toEqual([]);
      expect(answers[q.id].customAnswer).toBe('custom');
    });

    it('single-select replaces the selection and clears any custom answer', () => {
      const q = singleSelect();
      let answers = setCustomAnswer(createUserInputAnswers(), q, 'custom');
      answers = toggleOptionAnswer(answers, q, 'B');
      expect(answers[q.id].selectedOptionLabels).toEqual(['B']);
      expect(answers[q.id].customAnswer).toBe('');
    });
  });

  describe('resolvedAnswer', () => {
    it('multi-select combines selections and the typed answer, de-duplicated', () => {
      const q = multiSelect();
      let answers = toggleOptionAnswer(createUserInputAnswers(), q, 'A');
      answers = toggleOptionAnswer(answers, q, 'B');
      answers = setCustomAnswer(answers, q, 'A'); // duplicates an existing selection
      expect(resolvedAnswer(answers, q)).toEqual(['A', 'B']);
    });

    it('multi-select with a single pick resolves to a one-element array', () => {
      const q = multiSelect();
      const answers = toggleOptionAnswer(createUserInputAnswers(), q, 'A');
      expect(resolvedAnswer(answers, q)).toEqual(['A']);
    });

    it('single-select lets a typed answer override the option', () => {
      const q = singleSelect();
      let answers = toggleOptionAnswer(createUserInputAnswers(), q, 'A');
      answers = setCustomAnswer(answers, q, 'custom');
      expect(resolvedAnswer(answers, q)).toBe('custom');
    });
  });

  describe('toResponseAnswers', () => {
    it('serializes multi-select as an array (even one pick) and single-select as a string', () => {
      const multi = multiSelect({ id: 'multi' });
      const single = singleSelect({ id: 'single' });
      const request = requestWith(multi, single);
      let answers = toggleOptionAnswer(createUserInputAnswers(), multi, 'A');
      answers = toggleOptionAnswer(answers, single, 'B');
      expect(toResponseAnswers(request, answers)).toEqual({ multi: ['A'], single: 'B' });
    });

    it('combines selections and the typed entry for multi-select', () => {
      const q = multiSelect({ id: 'features' });
      const request = requestWith(q);
      let answers = toggleOptionAnswer(createUserInputAnswers(), q, 'A');
      answers = toggleOptionAnswer(answers, q, 'B');
      answers = setCustomAnswer(answers, q, 'extra');
      expect(toResponseAnswers(request, answers)).toEqual({ features: ['A', 'B', 'extra'] });
    });

    it('omits unanswered questions', () => {
      const request = requestWith(singleSelect({ id: 'single' }));
      expect(toResponseAnswers(request, createUserInputAnswers())).toEqual({});
    });
  });
});
