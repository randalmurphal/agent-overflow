import { expect, it } from 'vitest';
import { classifyRevealText } from './threadRevealText';
import { THINKING_TAIL_RUNES } from './threadPaneShared';

it.each(['assistant_text', 'thinking', 'compaction_reasoning'])('classifies received text without losing its authority distinction (%s)', (kind) => {
  expect(classifyRevealText(kind, 'hello', 'hello')).toBe('same');
  expect(classifyRevealText(kind, 'hello world', 'hello')).toBe('extension');
  expect(classifyRevealText(kind, 'hello', 'hello world')).toBe('trailing');
  expect(classifyRevealText(kind, 'goodbye', 'hello world')).toBe('replacement');
  expect(classifyRevealText(kind, '', 'hello')).toBe('trailing');
  expect(classifyRevealText(kind, 'hello', '')).toBe('extension');
});

it.each(['thinking', 'compaction_reasoning'])('recognizes Unicode reasoning previews and older interior slices (%s)', (kind) => {
  const preview = '😀'.repeat(THINKING_TAIL_RUNES);
  const received = `earlier reasoning ${preview}`;
  expect(classifyRevealText(kind, preview, received)).toBe('same');
  expect(classifyRevealText(kind, 'earlier reasoning 😀', received)).toBe('trailing');
  expect(classifyRevealText(kind, preview, received + ' next')).toBe('trailing');
  expect(classifyRevealText('assistant_text', preview, received)).toBe('replacement');
});
