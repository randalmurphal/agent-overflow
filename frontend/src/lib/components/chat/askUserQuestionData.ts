export interface AskOption {
  label: string;
  description?: string;
  preview?: string;
}

export interface AskQuestion {
  id?: string;
  header?: string;
  question: string;
  multiSelect?: boolean;
  options?: AskOption[];
}

type AnswerMap = Record<string, string | string[]>;

export function extractQuestions(meta: Record<string, unknown> | null): AskQuestion[] {
  if (!meta) return [];
  const input = meta.input;
  if (!input || typeof input !== 'object') return [];
  const list = (input as Record<string, unknown>).questions;
  if (!Array.isArray(list)) return [];
  return list.filter(
    (q): q is AskQuestion =>
      !!q && typeof q === 'object' && typeof (q as Record<string, unknown>).question === 'string',
  );
}

export function extractAnswers(content: unknown): AnswerMap {
  if (typeof content === 'string') return parseAnswersString(content);
  if (Array.isArray(content)) {
    const text = content
      .map((part) => {
        if (part && typeof part === 'object' && typeof (part as Record<string, unknown>).text === 'string') {
          return (part as Record<string, unknown>).text as string;
        }
        return '';
      })
      .join('');
    return parseAnswersString(text);
  }
  if (content && typeof content === 'object') {
    const candidate = (content as Record<string, unknown>).answers ?? content;
    return normalizeAnswerObject(candidate);
  }
  return {};
}

export function headerLabelForQuestions(questions: AskQuestion[]): string {
  if (questions.length === 0) return 'Question';
  if (questions.length === 1) return `Question: ${questions[0].question}`;
  return `Question: ${questions.length} questions`;
}

export function answersForQuestion(q: AskQuestion, answersByQuestion: AnswerMap): string[] {
  const id = q.id ?? '';
  const direct = id ? answersByQuestion[id] : undefined;
  if (direct !== undefined) return normalizeQuestionAnswers(q, direct);
  const byHeader = q.header ? answersByQuestion[q.header] : undefined;
  if (byHeader !== undefined) return normalizeQuestionAnswers(q, byHeader);
  const byQuestion = answersByQuestion[q.question];
  if (byQuestion !== undefined) return normalizeQuestionAnswers(q, byQuestion);
  return [];
}

export function classifyAnswers(q: AskQuestion, answers: string[]): { matched: Set<string>; customs: string[] } {
  const optionLabels = new Set((q.options ?? []).map((o) => o.label));
  const matched = new Set<string>();
  const customs: string[] = [];
  for (const answer of answers) {
    if (optionLabels.has(answer)) {
      matched.add(answer);
    } else if (answer.trim()) {
      customs.push(answer);
    }
  }
  return { matched, customs };
}

function parseAnswersString(text: string): AnswerMap {
  const trimmed = text.trim();
  if (!trimmed) return {};
  try {
    const parsed = JSON.parse(trimmed) as unknown;
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      const answers = normalizeAnswerObject((parsed as Record<string, unknown>).answers ?? parsed);
      if (Object.keys(answers).length > 0) return answers;
    }
  } catch {
    // Not JSON; Claude's canonical answer echo is parsed below.
  }
  return parseClaudeAnsweredSentence(trimmed);
}

function normalizeAnswerObject(candidate: unknown): AnswerMap {
  if (!candidate || typeof candidate !== 'object' || Array.isArray(candidate)) return {};
  const out: AnswerMap = {};
  for (const [k, v] of Object.entries(candidate as Record<string, unknown>)) {
    if (typeof v === 'string') out[k] = v;
    else if (Array.isArray(v)) {
      const list = v.filter((entry): entry is string => typeof entry === 'string');
      if (list.length > 0) out[k] = list;
    }
  }
  return out;
}

function parseClaudeAnsweredSentence(text: string): Record<string, string> {
  if (!text.startsWith('User has answered your questions:')) return {};
  const out: Record<string, string> = {};
  const pairPattern = /"((?:\\.|[^"\\])*)"\s*=\s*"((?:\\.|[^"\\])*)"/g;
  for (const match of text.matchAll(pairPattern)) {
    const key = decodeQuotedSegment(match[1] ?? '');
    const value = decodeQuotedSegment(match[2] ?? '');
    if (key) out[key] = value;
  }
  return out;
}

function decodeQuotedSegment(value: string): string {
  try {
    return JSON.parse(`"${value}"`) as string;
  } catch {
    return value.replace(/\\"/g, '"').replace(/\\\\/g, '\\');
  }
}

function normalizeQuestionAnswers(q: AskQuestion, answer: string | string[]): string[] {
  if (Array.isArray(answer)) return answer;
  if (q.multiSelect) {
    const parsedOptions = splitKnownMultiSelectOptions(answer, q.options ?? []);
    if (parsedOptions.length > 1) return parsedOptions;
  }
  return [answer];
}

function splitKnownMultiSelectOptions(answer: string, options: AskOption[]): string[] {
  const optionLabels = new Set(options.map((option) => option.label));
  if (optionLabels.size === 0 || optionLabels.has(answer)) return [];
  const parts = answer.split(',').map((part) => part.trim()).filter(Boolean);
  if (parts.length < 2) return [];
  return parseKnownOptionSequence(parts, optionLabels) ?? [];
}

function parseKnownOptionSequence(parts: string[], optionLabels: Set<string>): string[] | null {
  function parseFrom(index: number): string[] | null {
    if (index >= parts.length) return [];
    for (let end = index + 1; end <= parts.length; end++) {
      const candidate = parts.slice(index, end).join(', ');
      if (!optionLabels.has(candidate)) continue;
      const rest = parseFrom(end);
      if (rest) return [candidate, ...rest];
    }
    return null;
  }
  return parseFrom(0);
}
