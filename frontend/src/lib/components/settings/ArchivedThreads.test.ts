import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';

import { makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ArchivedThreads from './ArchivedThreads.svelte';

describe('<ArchivedThreads>', () => {
  it('renders friendly model aliases', async () => {
    setBindingMock('ListArchivedThreads', async () => [
      makeThread({
        id: 'codex-archived',
        archived: true,
        provider: 'codex',
        model: 'gpt-5.6-sol',
      }),
    ]);

    const { findByText } = render(ArchivedThreads);
    expect(await findByText(/codex · GPT 5\.6 Sol/)).toBeTruthy();
  });
});
