import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';

// Real-Chromium geometry for how a markdown table divides its width. The auto
// table layout sizes each column between its cells' min-content and
// max-content, which happy-dom's zero geometry cannot compute. Both render
// paths are covered: a settled table is static HTML, a streaming one is the
// component tree.

const COLUMN_PX = 640;

// The Key column holds only short identifiers; the other columns hold enough
// prose that the table cannot fit unwrapped, so every column is squeezed.
const SQUEEZED_TABLE = [
  '| Key | Values | Default | Applied |',
  '| --- | --- | --- | --- |',
  '| `onNoMatch` | `create` or `stage` | `create` | `stage` |',
  '| `protectedFields` | list of field paths the integration may not overwrite on a backed asset | empty | name, status, addresses, sub domains, domains, external addresses |',
  '| `matchFields` | ordered list from hostname, mac, ip | unset, meaning hostname and MAC | unset |',
].join('\n');

const LONG_CODE = 'frontend/src/lib/components/settings/KeybindingsSettings.svelte';
const LONG_CODE_TABLE = [
  '| Key | File | Notes |',
  '| --- | --- | --- |',
  `| \`pathKey\` | \`${LONG_CODE}\` | a long token that must still wrap inside its cell rather than widen it |`,
].join('\n');

// Six identifier columns need more room unwrapped than the column has.
const IDENTIFIER_TABLE = [
  '| `firstIdentifierName` | `secondIdentifierName` | `thirdIdentifierName` | `fourthIdentifierName` | `fifthIdentifierName` | `sixthIdentifierName` |',
  '| --- | --- | --- | --- | --- | --- |',
  '| a | b | c | d | e | f |',
].join('\n');

const SMALL_TABLE =['| # | Sev |', '| --- | --- |', '| 1 | `high` |'].join('\n');

const PATHS: Array<[string, boolean]> = [
  ['settled', false],
  ['streaming', true],
];

async function mountTable(source: string, streaming: boolean) {
  const column = document.createElement('div');
  column.style.width = `${COLUMN_PX}px`;
  document.body.appendChild(column);
  render(ChatMarkdown, { target: column, props: { source, streaming, pathRefs: [] } });
  const wrapper = await waitFor(() => {
    const found = column.querySelector<HTMLElement>('[data-streamdown-table]');
    expect(found).not.toBeNull();
    return found!;
  });
  return { wrapper, table: wrapper.querySelector('table')! };
}

/** Line boxes an inline element spans: more than one means it wrapped. */
const lineCount = (el: Element) => el.getClientRects().length;

const codeByText = (root: Element, text: string) =>
  [...root.querySelectorAll('code')].find((code) => code.textContent === text)!;

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
  document.body.innerHTML = '';
});

describe.each(PATHS)('markdown table sizing, %s', (_name, streaming) => {
  it('keeps short inline code whole in a squeezed column', async () => {
    const { wrapper, table } = await mountTable(SQUEEZED_TABLE, streaming);

    // The table is squeezed (prose wraps) yet fits: nothing pans.
    expect(table.getBoundingClientRect().height).toBeGreaterThan(
      table.rows.length * table.rows[0].getBoundingClientRect().height,
    );
    expect(wrapper.scrollWidth).toBe(wrapper.clientWidth);
    for (const text of ['onNoMatch', 'protectedFields', 'matchFields', 'create', 'stage']) {
      expect(lineCount(codeByText(table, text)), text).toBe(1);
    }
  });

  it('still wraps inline code too long to hold a column open', async () => {
    const { wrapper, table } = await mountTable(LONG_CODE_TABLE, streaming);

    expect(lineCount(codeByText(table, LONG_CODE))).toBeGreaterThan(1);
    expect(lineCount(codeByText(table, 'pathKey'))).toBe(1);
    expect(wrapper.scrollWidth).toBe(wrapper.clientWidth);
  });

  it('pans rather than splitting short inline code that cannot fit', async () => {
    const { wrapper, table } = await mountTable(IDENTIFIER_TABLE, streaming);

    expect(wrapper.scrollWidth).toBeGreaterThan(wrapper.clientWidth);
    expect(wrapper.getBoundingClientRect().width).toBeLessThanOrEqual(COLUMN_PX);
    for (const code of table.querySelectorAll('code')) {
      expect(lineCount(code), code.textContent ?? '').toBe(1);
    }
  });

  it('sizes a table that fits to its content, not to the column', async () => {
    const { wrapper, table } = await mountTable(SMALL_TABLE, streaming);

    expect(table.getBoundingClientRect().width).toBeLessThan(COLUMN_PX / 4);
    expect(wrapper.scrollWidth).toBe(wrapper.clientWidth);
  });

  it('holds a table with more content than room to the column width', async () => {
    const { wrapper, table } = await mountTable(SQUEEZED_TABLE, streaming);

    expect(Math.round(table.getBoundingClientRect().width)).toBe(wrapper.clientWidth);
  });
});
