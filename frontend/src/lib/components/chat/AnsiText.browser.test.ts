import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import shared from '../../../test/fixtures/ansiShownVectors.json';
import AnsiText from './AnsiText.svelte';
import { ansiShownText } from '../../utils/ansiText';

// What AnsiText's DOM holds is what find counts (utils/ansiText.ts), for
// every vector the backend's search is held to as well.
describe('AnsiText shows the shown text of every shared vector', () => {
  it.each(shared.vectors.map((vector) => [vector.name, vector] as const))('%s', async (_name, vector) => {
    const { container } = render(AnsiText, { props: { source: vector.input } });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(container.querySelector('pre')!.textContent).toBe(vector.shown);
  });

  it('a run of lines, as a log row renders them', async () => {
    const lines = shared.vectors.map((vector) => vector.input);
    const { container } = render(AnsiText, { props: { source: lines.join('\n') } });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(container.querySelector('pre')!.textContent).toBe(lines.map(ansiShownText).join('\n'));
  });
});
