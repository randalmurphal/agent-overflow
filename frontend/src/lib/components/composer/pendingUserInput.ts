import type { UserInputQuestion, UserInputRequest } from '../../types/events';

export interface UserInputDraftAnswer {
  selectedOptionLabels?: string[];
  customAnswer?: string;
}

export type UserInputAnswers = Record<string, UserInputDraftAnswer>;

export function createUserInputAnswers(): UserInputAnswers {
  return Object.create(null) as UserInputAnswers;
}

function copyUserInputAnswers(currentAnswers: UserInputAnswers): UserInputAnswers {
  return Object.assign(createUserInputAnswers(), currentAnswers);
}

function normalizeCustomAnswer(value: string | undefined): string | null {
  const trimmed = value?.trim() ?? '';
  return trimmed ? trimmed : null;
}

function normalizeSelectedOptionLabels(value: string[] | undefined): string[] {
  if (!Array.isArray(value)) return [];
  return Array.from(new Set(value.map((entry) => entry.trim()).filter(Boolean)));
}

export function resolvedAnswer(
  answers: UserInputAnswers,
  question: UserInputQuestion | undefined,
): string | string[] | null {
  if (!question) return null;
  const draft = answers[question.id];
  const customAnswer = normalizeCustomAnswer(draft?.customAnswer);
  if (customAnswer) return customAnswer;
  const selected = normalizeSelectedOptionLabels(draft?.selectedOptionLabels);
  if (question.multiSelect) {
    return selected.length > 0 ? selected : null;
  }
  return selected[0] ?? null;
}

export function selectedAnswers(
  answers: UserInputAnswers,
  question: UserInputQuestion | undefined,
): string[] {
  if (!question) return [];
  return normalizeSelectedOptionLabels(answers[question.id]?.selectedOptionLabels);
}

export function hasAnswer(answers: UserInputAnswers, question: UserInputQuestion): boolean {
  return resolvedAnswer(answers, question) !== null;
}

export function isRequestComplete(request: UserInputRequest, answers: UserInputAnswers): boolean {
  return request.questions.every((question) => hasAnswer(answers, question));
}

export function firstUnansweredIndex(request: UserInputRequest, answers: UserInputAnswers): number {
  const index = request.questions.findIndex((question) => !hasAnswer(answers, question));
  return index === -1 ? Math.max(0, request.questions.length - 1) : index;
}

export function toggleOptionAnswer(
  currentAnswers: UserInputAnswers,
  question: UserInputQuestion,
  label: string,
): UserInputAnswers {
  const nextAnswers = copyUserInputAnswers(currentAnswers);
  if (!question.multiSelect) {
    nextAnswers[question.id] = { customAnswer: '', selectedOptionLabels: [label] };
    return nextAnswers;
  }

  const current = selectedAnswers(currentAnswers, question);
  const next = current.includes(label)
    ? current.filter((answer) => answer !== label)
    : [...current, label];
  nextAnswers[question.id] = {
    customAnswer: '',
    ...(next.length > 0 ? { selectedOptionLabels: next } : {}),
  };
  return nextAnswers;
}

export function setCustomAnswer(
  currentAnswers: UserInputAnswers,
  question: UserInputQuestion,
  value: string,
): UserInputAnswers {
  const selectedOptionLabels = value.trim()
    ? []
    : normalizeSelectedOptionLabels(currentAnswers[question.id]?.selectedOptionLabels);
  const nextAnswers = copyUserInputAnswers(currentAnswers);
  nextAnswers[question.id] = {
    customAnswer: value,
    ...(selectedOptionLabels.length > 0 ? { selectedOptionLabels } : {}),
  };
  return nextAnswers;
}

export function toResponseAnswers(answers: UserInputAnswers): Record<string, string | string[]> {
  const response: Record<string, string | string[]> = {};
  for (const [questionID, draft] of Object.entries(answers)) {
    const customAnswer = normalizeCustomAnswer(draft.customAnswer);
    if (customAnswer) {
      response[questionID] = customAnswer;
      continue;
    }
    const selected = normalizeSelectedOptionLabels(draft.selectedOptionLabels);
    if (selected.length === 0) continue;
    response[questionID] = selected.length === 1 ? selected[0] : selected;
  }
  return response;
}
