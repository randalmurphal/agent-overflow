import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import UnsentMessageConfirmationHost from './UnsentMessageConfirmationHost.svelte';
import {
  confirmUnsentMessageRestore,
} from '../../stores/unsentMessageConfirmation.svelte';

describe('<UnsentMessageConfirmationHost>', () => {

  it('renders nothing until a send asks', async () => {
    const { queryByText } = render(UnsentMessageConfirmationHost);
    await tick();
    expect(queryByText('Unsent message')).toBeNull();
  });

  it.each([
    { case: 'asks the question in the words the ambiguity deserves', button: 'Leave it', restore: false },
    { case: 'puts the message back when the person says so', button: 'Put it back', restore: true },
  ])('$case', async ({ button, restore }) => {
    const { getByText, getByRole } = render(UnsentMessageConfirmationHost);
    const answer = confirmUnsentMessageRestore();
    await tick();

    // The copy is the whole feature at this layer: it has to say that the
    // message MAY have arrived, and the two buttons have to name what each
    // one does rather than reading as Yes/No to a question about failure.
    expect(getByText('This message may have reached the agent. Put it back in the composer?'))
      .not.toBeNull();
    getByRole('button', { name: button }).click();
    expect(await answer).toBe(restore);
  });
});
