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
  const BILLING = 'https://console.anthropic.com/settings/billing';
  it.each([
    { error: 'rate_limit', summary: 'Rate limit reached', copy: 'Add credits', href: BILLING },
    { error: 'billing_error', summary: 'Billing error', copy: 'Add credits', href: BILLING },
    { error: 'authentication_failed', summary: 'Authentication failed', copy: 'Run /login', href: undefined },
    { error: 'server_error', summary: 'Anthropic API server error', copy: 'try again in a moment', href: undefined },
    { error: 'max_output_tokens', summary: 'Reached max output tokens', copy: 'max-output-tokens cap', href: undefined },
    { error: 'unknown', summary: 'API error', copy: 'API error', href: undefined },
  ])('renders $error with its summary, copy and link', ({ error, summary, copy, href }) => {
    const { getByTestId } = render(APIErrorRow, { props: { item: makeErrorItem(error, summary) } });
    const row = getByTestId('api-error-row');
    expect(row.dataset.errorEnum).toBe(error);
    expect(row.getAttribute('role')).toBe('alert');
    expect(row.textContent).toContain(summary);
    expect(row.textContent).toContain(copy);
    expect(row.querySelector('a')?.getAttribute('href')).toBe(href);
  });
});
