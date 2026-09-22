import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import AsyncQuestionPicker from './AsyncQuestionPicker.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { noteThread } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
import { AsyncQuestion } from '../../stores/bindings';
import { refreshAsyncQuestions } from '../../stores/asyncQuestions.svelte';
import { TransportError } from '../../transport/wsClient';

const question = (itemId: string, index: number, title: string) => new AsyncQuestion({ itemId, index, title, options: ['A', 'B'], state: 'unanswered', answer: '', sendId: '', userItemId: '', createdAt: 1 });
let rows: AsyncQuestion[];
beforeEach(() => {
  resetBindingMocks(); localStorage.clear(); noteThread('thread', HOME_BACKEND);
  rows = [question('one', 0, 'First question?')];
  setBindingMock('ListAsyncQuestions', async () => rows);
});
afterEach(cleanup);

it('appends questions without resetting the focused answer and preserves drafts on remount', async () => {
  const view = render(AsyncQuestionPicker, { threadId: 'thread' });
  const input = await view.findByRole('textbox', { name: 'Your answer' });
  await fireEvent.input(input, { target: { value: 'My draft' } });
  rows = [...rows, question('two', 0, 'Second question?'), question('two', 1, 'Third question?')];
  refreshAsyncQuestions('thread');
  await waitFor(() => expect(view.getByTestId('async-question-position')).toHaveTextContent('1 / 3'));
  expect(view.getByRole('textbox')).toHaveValue('My draft');
  await fireEvent.click(view.getByRole('button', { name: /^Next$/ }));
  await fireEvent.input(view.getByRole('textbox'), { target: { value: 'Second draft' } });
  view.unmount();
  const next = render(AsyncQuestionPicker, { threadId: 'thread' });
  await waitFor(() => expect(next.getByRole('textbox')).toHaveValue('Second draft'));
  await fireEvent.click(next.getByRole('button', { name: 'Previous' }));
  expect(next.getByRole('textbox')).toHaveValue('My draft');
});

it('submits only the explicit snapshot and retries uncertain sends with the same identity', async () => {
  const submit = setBindingMock('SubmitAsyncAnswers', vi.fn().mockRejectedValueOnce(new Error('Connection lost')).mockResolvedValue(undefined));
  const view = render(AsyncQuestionPicker, { threadId: 'thread' });
  await fireEvent.input(await view.findByRole('textbox'), { target: { value: 'First answer' } });
  await fireEvent.click(view.getByRole('button', { name: 'Send answered (1)' }));
  await view.findByRole('alert');
  rows = [...rows, question('two', 0, 'New question?')]; refreshAsyncQuestions('thread');
  await waitFor(() => expect(view.getByTestId('async-question-position')).toHaveTextContent('1 / 2'));
  await fireEvent.click(view.getByRole('button', { name: 'Retry sending answers' }));
  await waitFor(() => expect(submit).toHaveBeenCalledTimes(2));
  expect(submit.mock.calls[1]).toEqual(submit.mock.calls[0]);
  expect(submit.mock.calls[0][2]).toEqual([{ itemId: 'one', index: 0, answer: 'First answer' }]);
});

it('does not submit a selected option or expire questions on its own', async () => {
  const submit = setBindingMock('SubmitAsyncAnswers', vi.fn());
  const view = render(AsyncQuestionPicker, { threadId: 'thread' });
  await fireEvent.click(await view.findByTestId('user-input-option-2'));
  expect(view.getByRole('textbox')).toHaveValue('B');
  expect(submit).not.toHaveBeenCalled();
  rows = [...rows, question('other', 0, 'Later?')]; refreshAsyncQuestions('thread');
  await waitFor(() => expect(view.getByTestId('async-question-position')).toHaveTextContent('1 / 2'));
  expect(submit).not.toHaveBeenCalled();
});

it('allows a new answer while an earlier snapshot is still being submitted', async () => {
  let release!: () => void;
  const submit = setBindingMock('SubmitAsyncAnswers', vi.fn(() => new Promise<void>(resolve => { release = resolve; })));
  const view = render(AsyncQuestionPicker, { threadId: 'thread' });
  await fireEvent.input(await view.findByRole('textbox'), { target: { value: 'First answer' } });
  await fireEvent.click(view.getByRole('button', { name: 'Send answered (1)' }));
  rows = [...rows, question('two', 0, 'Second question?')]; refreshAsyncQuestions('thread');
  await waitFor(() => expect(view.getByTestId('async-question-position')).toHaveTextContent('1 / 2'));
  view.unmount();
  const returned = render(AsyncQuestionPicker, { threadId: 'thread' });
  await returned.findByRole('textbox');
  await fireEvent.click(returned.getByRole('button', { name: /^Next$/ }));
  expect(returned.getByRole('textbox')).not.toBeDisabled();
  await fireEvent.input(returned.getByRole('textbox'), { target: { value: 'A later draft' } });
  release();
  await waitFor(() => expect(returned.getByRole('button', { name: 'Send answered (1)' })).not.toBeDisabled());
  expect(returned.getByRole('textbox')).toHaveValue('A later draft');
  expect(submit.mock.calls[0][2]).toEqual([{ itemId: 'one', index: 0, answer: 'First answer' }]);
});

it('reconciles a competing answer without trapping other saved answers in a retry', async () => {
  rows = [...rows, question('two', 0, 'Second question?')];
  setBindingMock('SubmitAsyncAnswers', async () => {
    rows = rows.slice(1);
    throw new TransportError('already_handled', 'answered elsewhere');
  });
  const view = render(AsyncQuestionPicker, { threadId: 'thread' });
  await fireEvent.input(await view.findByRole('textbox'), { target: { value: 'First answer' } });
  await fireEvent.click(view.getByRole('button', { name: /^Next$/ }));
  await fireEvent.input(view.getByRole('textbox'), { target: { value: 'Second answer' } });
  await fireEvent.click(view.getByRole('button', { name: 'Send answered (2)' }));
  await view.findByRole('alert');
  await waitFor(() => expect(view.getByTestId('async-question-position')).toHaveTextContent('1 / 1'));
  expect(view.getByRole('textbox')).toHaveValue('Second answer');
  expect(view.getByRole('button', { name: 'Send answered (1)' })).not.toBeDisabled();
});
