import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import APIErrorRow from './APIErrorRow.svelte';
import type { Item } from '../../types/models';

function makeErrorItem(enumValue: string, summary: string): Item {
  return {
    id: 'error:0:0',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 1,
    kind: 'api_error',
    role: 'system',
    status: 'completed',
    summary,
    meta: JSON.stringify({ error: enumValue, fatal: true }),
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

describe('<APIErrorRow>', () => {
  it('renders the summary and stamps the enum on data-error-enum', () => {
    const { getByTestId } = render(APIErrorRow, {
      props: { item: makeErrorItem('rate_limit', 'Rate limit reached') },
    });
    const row = getByTestId('api-error-row');
    expect(row.textContent).toContain('Rate limit reached');
    expect(row.dataset.errorEnum).toBe('rate_limit');
    expect(row.getAttribute('role')).toBe('alert');
  });

  it('rate_limit errors link to the Anthropic billing console', () => {
    const { getByTestId, getByText } = render(APIErrorRow, {
      props: { item: makeErrorItem('rate_limit', 'Rate limit reached') },
    });
    const row = getByTestId('api-error-row');
    const link = row.querySelector('a');
    expect(link).not.toBeNull();
    expect(link?.getAttribute('href')).toContain('console.anthropic.com');
    expect(getByText(/Add credits/)).toBeInTheDocument();
  });

  it('billing_error errors link to the Anthropic billing console', () => {
    const { getByTestId } = render(APIErrorRow, {
      props: { item: makeErrorItem('billing_error', 'Billing error') },
    });
    const link = getByTestId('api-error-row').querySelector('a');
    expect(link?.getAttribute('href')).toContain('console.anthropic.com');
  });

  it('authentication_failed surfaces the /login hint without a link', () => {
    const { getByTestId, getByText } = render(APIErrorRow, {
      props: { item: makeErrorItem('authentication_failed', 'Authentication failed') },
    });
    const row = getByTestId('api-error-row');
    expect(row.querySelector('a')).toBeNull();
    expect(getByText(/Run \/login/)).toBeInTheDocument();
  });

  it('server_error surfaces a transient-retry hint without a link', () => {
    const { getByTestId, getByText } = render(APIErrorRow, {
      props: { item: makeErrorItem('server_error', 'Anthropic API server error') },
    });
    expect(getByTestId('api-error-row').querySelector('a')).toBeNull();
    expect(getByText(/try again in a moment/)).toBeInTheDocument();
  });

  it('max_output_tokens surfaces a re-prompt hint', () => {
    const { getByText } = render(APIErrorRow, {
      props: { item: makeErrorItem('max_output_tokens', 'Reached max output tokens') },
    });
    expect(getByText(/max-output-tokens cap/)).toBeInTheDocument();
  });

  it('unknown enum renders summary verbatim without hint or link', () => {
    const { getByTestId } = render(APIErrorRow, {
      props: { item: makeErrorItem('unknown', 'API error') },
    });
    const row = getByTestId('api-error-row');
    expect(row.textContent).toContain('API error');
    expect(row.querySelector('a')).toBeNull();
  });
});
